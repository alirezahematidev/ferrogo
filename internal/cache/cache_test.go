package cache

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func key(s string) string {
	b := NewKeyBuilder("test")
	b.AddString("k", s)
	return b.Sum()
}

func TestKeyBuilderIsUnambiguous(t *testing.T) {
	a := NewKeyBuilder("d")
	a.AddString("x", "ab")
	a.AddString("y", "c")
	b := NewKeyBuilder("d")
	b.AddString("x", "a")
	b.AddString("y", "bc")
	if a.Sum() == b.Sum() {
		t.Fatal("field boundaries are not part of the key")
	}
	c := NewKeyBuilder("other")
	c.AddString("x", "ab")
	c.AddString("y", "c")
	if a.Sum() == c.Sum() {
		t.Fatal("domain is not part of the key")
	}
	d := NewKeyBuilder("d")
	d.AddString("x", "ab")
	d.AddString("y", "c")
	if a.Sum() != d.Sum() || len(a.Sum()) != 64 {
		t.Fatal("key is not deterministic")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	c, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	src := writeFile(t, t.TempDir(), "prog.wasm", []byte("\x00asm payload"))
	k := key("a")
	if _, ok, err := c.Get(k); ok || err != nil {
		t.Fatalf("empty cache: ok=%v err=%v", ok, err)
	}
	e, err := c.Put(k, src, Meta{Kind: "program", Target: "wasm32-wasip1"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(e.Path) != ".wasm" {
		t.Errorf("extension not preserved: %s", e.Path)
	}
	got, ok, err := c.Get(k)
	if err != nil || !ok {
		t.Fatalf("get after put: ok=%v err=%v", ok, err)
	}
	data, err := os.ReadFile(got.Path)
	if err != nil || !bytes.Equal(data, []byte("\x00asm payload")) {
		t.Fatalf("content = %q, %v", data, err)
	}
	if got.Meta.Target != "wasm32-wasip1" || got.Meta.Size != int64(len(data)) {
		t.Errorf("meta = %+v", got.Meta)
	}
	// The stored artifact is read-only.
	if fi, _ := os.Stat(got.Path); fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("artifact is writable: %v", fi.Mode())
	}
}

func TestCorruptEntryIsAMiss(t *testing.T) {
	c, _ := Open(t.TempDir(), 0)
	src := writeFile(t, t.TempDir(), "a.bin", []byte("original"))
	k := key("corrupt")
	e, err := c.Put(k, src, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get(k); ok || err != nil {
		t.Fatalf("tampered entry served: ok=%v err=%v", ok, err)
	}
	// The slot is free again and can be repopulated.
	if _, err := c.Put(k, src, Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(k); !ok {
		t.Fatal("rebuilt entry missing")
	}
}

func TestConcurrentPutsSameKey(t *testing.T) {
	c, _ := Open(t.TempDir(), 0)
	src := writeFile(t, t.TempDir(), "a.bin", bytes.Repeat([]byte("z"), 1<<20))
	k := key("race")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := c.Put(k, src, Meta{})
			if err == nil && e.Meta.Size != 1<<20 {
				t.Errorf("partial entry: %d bytes", e.Meta.Size)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	tmp, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp"))
	if len(tmp) != 0 {
		t.Errorf("leftover temp dirs: %d", len(tmp))
	}
}

func TestTrimEvictsLeastRecentlyUsed(t *testing.T) {
	c, _ := Open(t.TempDir(), 250)
	clock := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return clock }
	src := writeFile(t, t.TempDir(), "a.bin", bytes.Repeat([]byte("x"), 100))
	keys := []string{key("1"), key("2"), key("3")}
	for _, k := range keys {
		if _, err := c.Put(k, src, Meta{}); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Hour)
	}
	// Touch the oldest so the middle one becomes least recently used.
	if _, ok, _ := c.Get(keys[0]); !ok {
		t.Fatal("missing entry")
	}
	removed, err := c.Trim()
	if err != nil || removed != 1 {
		t.Fatalf("Trim = %d, %v; want 1", removed, err)
	}
	if _, ok, _ := c.Get(keys[1]); ok {
		t.Error("least recently used entry survived")
	}
	for _, k := range []string{keys[0], keys[2]} {
		if _, ok, _ := c.Get(k); !ok {
			t.Error("recently used entry evicted")
		}
	}
}

func TestStatsAndClear(t *testing.T) {
	c, _ := Open(t.TempDir(), 0)
	src := writeFile(t, t.TempDir(), "a.bin", []byte("12345"))
	for _, s := range []string{"a", "b"} {
		if _, err := c.Put(key(s), src, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := c.Stats()
	if err != nil || st.Entries != 2 || st.Bytes != 10 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Stats(); st.Entries != 0 {
		t.Errorf("entries after clear = %d", st.Entries)
	}
}

func TestInvalidKeyRejected(t *testing.T) {
	c, _ := Open(t.TempDir(), 0)
	for _, k := range []string{"", "../escape", strings.Repeat("G", 64)} {
		if _, _, err := c.Get(k); err == nil {
			t.Errorf("Get(%q) accepted", k)
		}
	}
}

func TestWorkDir(t *testing.T) {
	c, _ := Open(t.TempDir(), 0)
	dir, cleanup, err := c.NewWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "x", []byte("y"))
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("work dir not removed: %v", err)
	}
}

// advancingClock makes every Get old enough to rewrite meta.json, the
// worst case for concurrent readers of one entry.
func advancingClock(c *Cache) {
	var ticks atomic.Int64
	base := time.Now()
	c.now = func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * 2 * touchInterval) }
}

func TestConcurrentGetsSameKey(t *testing.T) {
	c, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	advancingClock(c)
	payload := []byte("\x00asm shared payload")
	src := writeFile(t, t.TempDir(), "prog.wasm", payload)
	k := key("hot")
	if _, err := c.Put(k, src, Meta{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				e, ok, err := c.Get(k)
				if err == nil && !ok {
					err = errors.New("valid entry reported as a miss")
				}
				if err == nil && j%10 == 0 {
					_, err = c.Put(k, src, Meta{})
				}
				if err != nil {
					errs <- err
					return
				}
				_ = e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if e, ok, err := c.Get(k); err != nil || !ok {
		t.Fatalf("entry lost after concurrent use: ok=%v err=%v", ok, err)
	} else if got, _ := os.ReadFile(e.Path); !bytes.Equal(got, payload) {
		t.Fatal("artifact content changed")
	}
}

func TestFailedTouchKeepsEntry(t *testing.T) {
	c, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	advancingClock(c)
	src := writeFile(t, t.TempDir(), "prog.wasm", []byte("\x00asm payload"))
	k := key("touch")
	if _, err := c.Put(k, src, Meta{}); err != nil {
		t.Fatal(err)
	}
	orig := rename
	rename = func(string, string) error { return fs.ErrPermission }
	defer func() { rename = orig }()
	e, ok, err := c.Get(k)
	if err != nil || !ok {
		t.Fatalf("failed last-used update broke Get: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(e.Path); err != nil {
		t.Fatalf("valid artifact removed: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(e.Path), ".meta-*")); len(matches) != 0 {
		t.Fatalf("temp metadata left behind: %v", matches)
	}
}

func benchCache(b *testing.B) (*Cache, string, string) {
	c, err := Open(b.TempDir(), 0)
	if err != nil {
		b.Fatal(err)
	}
	src := filepath.Join(b.TempDir(), "prog.wasm")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 64<<10), 0o600); err != nil {
		b.Fatal(err)
	}
	k := key("bench")
	if _, err := c.Put(k, src, Meta{}); err != nil {
		b.Fatal(err)
	}
	return c, k, src
}

func BenchmarkCacheGetConcurrent(b *testing.B) {
	c, k, _ := benchCache(b)
	var fails atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, ok, err := c.Get(k); err != nil || !ok {
				fails.Add(1)
			}
		}
	})
	b.ReportMetric(float64(fails.Load())/float64(b.N), "fails/op")
}

func BenchmarkCacheGetPutConcurrent(b *testing.B) {
	c, k, src := benchCache(b)
	var fails atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for i := 0; pb.Next(); i++ {
			var err error
			if i%4 == 0 {
				_, err = c.Put(k, src, Meta{})
			} else {
				_, _, err = c.Get(k)
			}
			if err != nil {
				fails.Add(1)
			}
		}
	})
	b.ReportMetric(float64(fails.Load())/float64(b.N), "fails/op")
}
