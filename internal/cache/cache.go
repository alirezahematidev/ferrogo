// Package cache implements FerroGo's persistent, content-addressed artifact
// store. Entries are keyed by SHA-256 over every build input, written
// atomically (temp directory + rename), verified on read, and evicted
// least-recently-used when the store exceeds its size limit.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// SchemaVersion is the on-disk layout version. Changing it orphans old entries.
const SchemaVersion = 1

// touchInterval throttles last-used updates so hits rarely write.
const touchInterval = time.Minute

var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// errCorrupt marks an entry that must be deleted and rebuilt. Any other read
// error is treated as transient and leaves the entry in place.
var errCorrupt = errors.New("corrupt entry")

// rename is os.Rename, replaceable in tests.
var rename = os.Rename

// retry runs op until it succeeds, reports a missing file or corruption, or
// seven retries (about 130 ms of backoff) fail. Windows refuses to open or
// replace a file while another goroutine or process is replacing it.
func retry(op func() error) error {
	err := op()
	for wait := time.Millisecond; err != nil && wait <= 64*time.Millisecond; wait *= 2 {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errCorrupt) {
			break
		}
		time.Sleep(wait)
		err = op()
	}
	return err
}

// Cache is a handle to a cache directory. It is safe for concurrent use by
// multiple goroutines and multiple processes.
type Cache struct {
	root     string
	maxBytes int64
	now      func() time.Time
}

// Meta is the metadata stored next to each artifact.
type Meta struct {
	Schema   int       `json:"schema"`
	Key      string    `json:"key"`
	File     string    `json:"file"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Kind     string    `json:"kind"`
	Target   string    `json:"target"`
	Backend  string    `json:"backend"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
	// Inputs is a human-readable summary of the fingerprint (toolchain,
	// profile, ...) for `ferrogo cache` inspection. The key is authoritative.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// Entry is a cached artifact.
type Entry struct {
	Key  string
	Path string
	Meta Meta
}

// Stats summarises cache usage.
type Stats struct {
	Dir      string
	Entries  int
	Bytes    int64 // artifact bytes
	MaxBytes int64 // 0 = unlimited
	// OtherBytes covers build directories and runtime compilation caches.
	OtherBytes int64
}

// Open opens (creating if needed) the cache rooted at dir. maxBytes bounds the
// total artifact size; 0 disables eviction.
func Open(dir string, maxBytes int64) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("cache: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	c := &Cache{root: filepath.Join(abs, fmt.Sprintf("v%d", SchemaVersion)), maxBytes: maxBytes, now: time.Now}
	for _, sub := range []string{"artifacts", "tmp", "work"} {
		if err := os.MkdirAll(filepath.Join(c.root, sub), 0o700); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
	}
	return c, nil
}

// Dir returns the versioned cache root.
func (c *Cache) Dir() string { return c.root }

// Subdir returns (creating if needed) a named directory inside the cache, used
// for shared build directories and runtime compilation caches.
func (c *Cache) Subdir(name string) (string, error) {
	dir := filepath.Join(c.root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cache: %w", err)
	}
	return dir, nil
}

func (c *Cache) entryDir(key string) string {
	return filepath.Join(c.root, "artifacts", key[:2], key)
}

// Get returns the entry for key. A missing, incomplete or corrupt entry is a
// miss (ok == false); corrupt entries are removed so they can be rebuilt.
func (c *Cache) Get(key string) (entry *Entry, ok bool, err error) {
	if !keyPattern.MatchString(key) {
		return nil, false, fmt.Errorf("cache: invalid key %q", key)
	}
	dir := c.entryDir(key)
	var meta Meta
	err = retry(func() error {
		var err error
		if meta, err = readMeta(filepath.Join(dir, "meta.json")); err == nil {
			err = c.verify(dir, key, meta)
		}
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if errors.Is(err, errCorrupt) {
		// Never reuse an artifact whose fingerprint or content does not match.
		if rmErr := removeAll(dir); rmErr != nil {
			return nil, false, fmt.Errorf("cache: removing corrupt entry %s: %w", key, rmErr)
		}
		return nil, false, nil
	}
	if err != nil {
		// A transient I/O error is not corruption: keep the entry.
		return nil, false, fmt.Errorf("cache: reading entry %s (check the cache directory permissions, or retry if another process holds it): %w", key, err)
	}
	if now := c.now(); now.Sub(meta.LastUsed) > touchInterval {
		meta.LastUsed = now
		// The last-used time is only an eviction hint. A failed update (for
		// example while another reader replaces meta.json on Windows) must
		// not fail the lookup.
		_ = writeMeta(dir, meta)
	}
	return &Entry{Key: key, Path: filepath.Join(dir, meta.File), Meta: meta}, true, nil
}

func (c *Cache) verify(dir, key string, meta Meta) error {
	if meta.Schema != SchemaVersion || meta.Key != key || meta.File == "" || filepath.Base(meta.File) != meta.File {
		return fmt.Errorf("%w: metadata mismatch", errCorrupt)
	}
	sum, size, err := hashFile(filepath.Join(dir, meta.File))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: artifact missing", errCorrupt)
	}
	if err != nil {
		return err
	}
	if size != meta.Size || sum != meta.SHA256 {
		return fmt.Errorf("%w: artifact content mismatch", errCorrupt)
	}
	return nil
}

// Put stores the file at src under key and returns the new entry. If another
// writer stored the key first, the existing entry is returned. Readers never
// observe a partially written entry.
func (c *Cache) Put(key, src string, meta Meta) (*Entry, error) {
	if !keyPattern.MatchString(key) {
		return nil, fmt.Errorf("cache: invalid key %q", key)
	}
	if e, ok, err := c.Get(key); err != nil || ok {
		return e, err
	}

	tmp, err := os.MkdirTemp(filepath.Join(c.root, "tmp"), key[:16]+"-")
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = removeAll(tmp) // best-effort cleanup of an uncommitted temp dir
		}
	}()

	meta.Schema = SchemaVersion
	meta.Key = key
	meta.File = "artifact" + filepath.Ext(src)
	meta.SHA256, meta.Size, err = copyFile(src, filepath.Join(tmp, meta.File))
	if err != nil {
		return nil, fmt.Errorf("cache: storing artifact: %w", err)
	}
	now := c.now()
	meta.Created, meta.LastUsed = now, now
	if err := writeMeta(tmp, meta); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}

	final := c.entryDir(key)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		// Lost a race with a concurrent writer: use the winner's entry.
		if e, ok, getErr := c.Get(key); getErr == nil && ok {
			return e, nil
		}
		return nil, fmt.Errorf("cache: committing entry: %w", err)
	}
	committed = true
	syncDir(filepath.Dir(final))
	return &Entry{Key: key, Path: filepath.Join(final, meta.File), Meta: meta}, nil
}

// NewWorkDir creates a private scratch directory inside the cache (so that
// artifacts can be renamed into the store on the same filesystem).
func (c *Cache) NewWorkDir() (string, func() error, error) {
	dir, err := os.MkdirTemp(filepath.Join(c.root, "work"), "build-")
	if err != nil {
		return "", nil, fmt.Errorf("cache: %w", err)
	}
	return dir, func() error { return removeAll(dir) }, nil
}

type entryInfo struct {
	dir      string
	size     int64
	lastUsed time.Time
}

func (c *Cache) entries() ([]entryInfo, error) {
	var out []entryInfo
	base := filepath.Join(c.root, "artifacts")
	shards, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		keys, err := os.ReadDir(filepath.Join(base, shard.Name()))
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			dir := filepath.Join(base, shard.Name(), k.Name())
			meta, err := readMeta(filepath.Join(dir, "meta.json"))
			if err != nil {
				// Orphaned directory: count it as oldest so Trim removes it.
				out = append(out, entryInfo{dir: dir, size: dirSize(dir)})
				continue
			}
			out = append(out, entryInfo{dir: dir, size: meta.Size, lastUsed: meta.LastUsed})
		}
	}
	return out, nil
}

// Stats reports cache usage.
func (c *Cache) Stats() (Stats, error) {
	entries, err := c.entries()
	if err != nil {
		return Stats{}, fmt.Errorf("cache: %w", err)
	}
	s := Stats{Dir: c.root, Entries: len(entries), MaxBytes: c.maxBytes}
	for _, e := range entries {
		s.Bytes += e.size
	}
	dirs, err := os.ReadDir(c.root)
	if err != nil {
		return Stats{}, fmt.Errorf("cache: %w", err)
	}
	for _, d := range dirs {
		if d.IsDir() && d.Name() != "artifacts" {
			s.OtherBytes += dirSize(filepath.Join(c.root, d.Name()))
		}
	}
	return s, nil
}

// Trim evicts least-recently-used entries until the artifact total fits
// maxBytes. It returns the number of entries removed.
func (c *Cache) Trim() (int, error) {
	if c.maxBytes <= 0 {
		return 0, nil
	}
	entries, err := c.entries()
	if err != nil {
		return 0, fmt.Errorf("cache: %w", err)
	}
	var total int64
	for _, e := range entries {
		total += e.size
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUsed.Before(entries[j].lastUsed) })
	removed := 0
	var errs []error
	for _, e := range entries {
		if total <= c.maxBytes {
			break
		}
		if err := removeAll(e.dir); err != nil {
			// e.g. a running executable on Windows; skip it and keep going.
			errs = append(errs, err)
			continue
		}
		total -= e.size
		removed++
	}
	return removed, errors.Join(errs...)
}

// Clear removes every entry and all build directories. Builds running
// concurrently in other processes may fail and must be retried.
func (c *Cache) Clear() error {
	if err := removeAll(c.root); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	for _, sub := range []string{"artifacts", "tmp", "work"} {
		if err := os.MkdirAll(filepath.Join(c.root, sub), 0o700); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
	}
	return nil
}

// KeyBuilder computes cache keys. Each field is length-prefixed so that
// distinct input sets can never produce the same byte stream.
type KeyBuilder struct{ h hash.Hash }

// NewKeyBuilder starts a key in the given domain (e.g. "ferrogo-artifact-v1").
func NewKeyBuilder(domain string) *KeyBuilder {
	b := &KeyBuilder{h: sha256.New()}
	b.Add("domain", []byte(domain))
	return b
}

// Add mixes a named field into the key.
func (b *KeyBuilder) Add(name string, value []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(name)))
	b.h.Write(n[:])
	b.h.Write([]byte(name))
	binary.BigEndian.PutUint64(n[:], uint64(len(value)))
	b.h.Write(n[:])
	b.h.Write(value)
}

// AddString mixes a named string field into the key.
func (b *KeyBuilder) AddString(name, value string) { b.Add(name, []byte(value)) }

// Sum returns the hex-encoded key.
func (b *KeyBuilder) Sum() string { return hex.EncodeToString(b.h.Sum(nil)) }

func readMeta(path string) (Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("%w: metadata: %v", errCorrupt, err)
	}
	return m, nil
}

// writeMeta atomically replaces dir/meta.json.
func writeMeta(dir string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := retry(func() error { return rename(tmp, filepath.Join(dir, "meta.json")) }); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copyFile copies src to dst (fsynced, read-only and executable) and returns
// the content hash and size.
func copyFile(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	// Executables and shared libraries need the execute bit on some systems.
	if err := os.Chmod(dst, 0o500); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func dirSize(dir string) int64 {
	var total int64
	// Errors (e.g. entries removed concurrently) only make the estimate smaller.
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// removeAll removes path, first making read-only entries writable if needed
// (Windows refuses to delete read-only files).
func removeAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil {
			_ = os.Chmod(p, 0o700) // best effort; RemoveAll below reports real failures
		}
		return nil
	})
	return os.RemoveAll(path)
}
