// Package compiler turns Rust sources into artifacts. It generates a private
// Cargo project, runs cargo with structured arguments and a scrubbed
// environment, parses diagnostics, and stores results in the artifact cache.
package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/cache"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/process"
	"github.com/alirezahematidev/ferrogo/internal/sandbox"
	"github.com/alirezahematidev/ferrogo/internal/toolchain"
)

// keyDomain separates artifact keys from any other use of the cache hasher.
// Bump it when the build recipe changes in a way the inputs do not capture.
const keyDomain = "ferrogo-artifact-v1"

// maxCargoOutput bounds cargo's captured stdout+stderr.
const maxCargoOutput = 64 << 20

// Kind is the artifact kind.
type Kind string

// Artifact kinds.
const (
	KindProgram Kind = "program" // an executable with fn main
	KindLibrary Kind = "library" // a cdylib exposing exported functions
)

// SourceFile is one Rust source file of a generated crate.
type SourceFile struct {
	// Path is relative to the crate's src/ directory ("main.rs", "lib.rs", "util.rs").
	Path    string
	Content []byte
	// DisplayName is used in diagnostics instead of src/<Path>. Empty means Path.
	DisplayName string
	// LineOffset is added to every reported line (for code extracted from a larger file).
	LineOffset int
	// LineMap, when set, maps each line of Content (index 0 = line 1) to its
	// origin, for sources assembled from fragments of several files. It
	// takes precedence over DisplayName and LineOffset.
	LineMap []Origin
}

// Origin is the original location of one line of generated source.
type Origin struct {
	File string
	Line int
	// ColumnOffset is added to columns on this line.
	ColumnOffset int
}

func (s SourceFile) displayName() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Path
}

// Locate maps a 1-based line and column of this file to the location the
// user knows.
func (s SourceFile) Locate(line, col int) (string, int, int) {
	if s.LineMap != nil && line >= 1 && line <= len(s.LineMap) {
		o := s.LineMap[line-1]
		return o.File, o.Line, col + o.ColumnOffset
	}
	return s.displayName(), line + s.LineOffset, col
}

// LocateGenerated maps a location reported against src/<path> (as rustc and
// panics print it) through the matching source; ok is false if no source
// matches.
func LocateGenerated(sources []SourceFile, file string, line, col int) (string, int, int, bool) {
	for _, s := range sources {
		if file == "src/"+s.Path {
			f, l, c := s.Locate(line, col)
			return f, l, c, true
		}
	}
	return file, line, col, false
}

// Request describes one build.
type Request struct {
	// Sources[0] must be the crate root: main.rs for programs, lib.rs for libraries.
	Sources []SourceFile
	Kind    Kind
	Target  string
	// Backend is recorded in the cache key and metadata.
	Backend string
	// Profile is ProfileRelease (default) or ProfileDebug.
	Profile  string
	OptLevel string
	Edition  string
	// Panic overrides the panic strategy ("unwind" or "abort").
	Panic     string
	RustFlags []string
	// Env holds extra build environment variables (part of the cache key).
	Env     map[string]string
	Timeout time.Duration
	// Progress receives cargo's human-readable progress output.
	Progress io.Writer
	// NoCache forces a rebuild (the result is still stored).
	NoCache bool
	// Dependencies are added to [dependencies]. Their build scripts and
	// proc-macros run on the build host.
	Dependencies []Dependency
	// Network lets cargo download missing crates; otherwise --offline.
	Network bool

	depsFP string // set by Build when Dependencies are present
}

// Artifact is a built (or cached) artifact.
type Artifact struct {
	Path     string
	Key      string
	Kind     Kind
	Target   string
	Size     int64
	Cached   bool
	Duration time.Duration
	// Warnings are the warning-level diagnostics of the build that produced
	// the artifact (replayed from the cache on hits).
	Warnings []Diagnostic
}

// Compiler builds artifacts. It is safe for concurrent use.
type Compiler struct {
	tc     *toolchain.Toolchain
	cache  *cache.Cache
	sem    chan struct{}
	flight flightGroup
	log    *slog.Logger
	// sandboxed runs every toolchain process under the OS sandbox.
	sandboxed bool
}

// EnableSandbox runs all toolchain processes (cargo, rustc, build scripts,
// linkers) under the OS sandbox: no network unless a build allows it, no
// writes outside the cache, CARGO_HOME and the temp directory, and no reads
// of the home directory beyond the toolchain, Cargo home, cache and the
// build's own inputs. This stops include_str!/include_bytes! from reading
// secrets in the home directory. It fails with ErrUnsupported where no
// sandbox is available.
func (c *Compiler) EnableSandbox() error {
	if err := sandbox.Available(); err != nil {
		return fmt.Errorf("%w: compiler sandbox: %v", errdefs.ErrUnsupported, err)
	}
	c.sandboxed = true
	return nil
}

// run executes a toolchain process, sandboxed when enabled. inputs are
// extra paths the process must read and write (project and path-dependency
// directories).
func (c *Compiler) run(ctx context.Context, spec process.Spec, network bool, inputs ...string) (*process.Result, error) {
	if !c.sandboxed {
		return process.Run(ctx, spec)
	}
	writable := []string{c.cache.Dir(), os.TempDir()}
	if home := c.tc.CargoHome(); home != "" {
		writable = append(writable, home)
	}
	readable := []string{c.tc.Sysroot}
	if v := os.Getenv("RUSTUP_HOME"); v != "" {
		readable = append(readable, v)
	}
	policy := sandbox.Policy{
		AllowNetwork: network, Dir: spec.Dir,
		Writable: append(writable, inputs...),
		Readable: append(readable, inputs...),
	}
	path, args, err := sandbox.Command(policy, spec.Path, spec.Args)
	if err != nil {
		return nil, fmt.Errorf("%w: compiler sandbox: %v", errdefs.ErrUnsupported, err)
	}
	spec.Path, spec.Args = path, args
	return process.Run(ctx, spec)
}

// New returns a compiler using tc and storing artifacts in c. maxConcurrent
// bounds simultaneous cargo invocations (minimum 1).
func New(tc *toolchain.Toolchain, c *cache.Cache, maxConcurrent int, log *slog.Logger) *Compiler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Compiler{tc: tc, cache: c, sem: make(chan struct{}, max(1, maxConcurrent)), log: log}
}

// Toolchain returns the toolchain used for builds.
func (c *Compiler) Toolchain() *toolchain.Toolchain { return c.tc }

// Build compiles req, returning a cached artifact when one matches.
func (c *Compiler) Build(ctx context.Context, req Request) (*Artifact, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if !c.tc.HasTarget(req.Target) {
		return nil, &Error{Reason: ReasonTarget, Target: req.Target,
			Output: fmt.Sprintf("error: the %s target is not installed; fix: %s", req.Target, c.tc.TargetFix(req.Target))}
	}
	manifest := req.manifest()
	var lock []byte
	if len(req.Dependencies) > 0 {
		configs, err := c.cargoConfigFiles()
		if err != nil {
			return nil, err
		}
		req.depsFP = c.depsFingerprint(req, configs)
		if lock, err = c.lockfile(ctx, req, manifest, req.depsFP); err != nil {
			return nil, err
		}
	}
	key, inputs, err := c.key(req, manifest, lock)
	if err != nil {
		return nil, err
	}
	if !req.NoCache {
		if a, err := c.lookup(key, req); a != nil || err != nil {
			return a, err
		}
	}
	for {
		a, err, shared := c.flight.do(key, func() (*Artifact, error) {
			return c.build(ctx, req, manifest, key, inputs, lock)
		})
		// A follower must not inherit the leader's cancellation.
		if shared && err != nil && ctx.Err() == nil &&
			(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			continue
		}
		if shared && a != nil {
			cp := *a
			cp.Cached = true
			return &cp, err
		}
		return a, err
	}
}

func (c *Compiler) lookup(key string, req Request) (*Artifact, error) {
	e, ok, err := c.cache.Get(key)
	if err != nil || !ok {
		return nil, err
	}
	a := &Artifact{Path: e.Path, Key: key, Kind: req.Kind, Target: req.Target, Size: e.Meta.Size, Cached: true}
	if raw := e.Meta.Inputs["warnings"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &a.Warnings); err != nil {
			c.log.Warn("ignoring unreadable cached warnings", "key", key, "error", err)
		}
	}
	return a, nil
}

// key computes the artifact cache key over every input that can change the
// artifact. It also returns a readable summary stored in the entry metadata.
func (c *Compiler) key(req Request, manifest string, lock []byte) (string, map[string]string, error) {
	b := cache.NewKeyBuilder(keyDomain)
	b.AddString("rustc", c.tc.RustcVerbose)
	b.AddString("cargo", c.tc.CargoVersion)
	b.AddString("backend", req.Backend)
	b.AddString("kind", string(req.Kind))
	b.AddString("target", req.Target)
	b.AddString("profile", req.profile())
	b.AddString("opt-level", req.OptLevel)
	b.AddString("edition", req.edition())
	b.AddString("panic", req.Panic)
	b.AddString("rustflags", strings.Join(req.RustFlags, "\x1f"))
	envKeys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		envKeys = append(envKeys, k)
	}
	slices.Sort(envKeys)
	for _, k := range envKeys {
		b.AddString("env:"+k, req.Env[k])
	}
	b.AddString("manifest", manifest)
	b.Add("cargo-lock", lock)
	// Unsandboxed builds can read files a sandboxed build could not (for
	// example via include_str!), so their artifacts must never be served to
	// a sandboxed runtime.
	b.AddString("compiler-sandbox", fmt.Sprint(c.sandboxed))
	for _, s := range req.Sources {
		b.Add("src:"+s.Path, s.Content)
	}
	configs, err := c.cargoConfigFiles()
	if err != nil {
		return "", nil, err
	}
	for _, p := range configs {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", nil, fmt.Errorf("compiler: reading cargo config %s: %w", p, err)
		}
		b.Add("cargo-config:"+p, data)
	}
	inputs := map[string]string{
		"rustc":   c.tc.Version.String() + " (" + c.tc.CommitHash + ")",
		"cargo":   c.tc.CargoVersion,
		"profile": req.profile(),
		"edition": req.edition(),
	}
	return b.Sum(), inputs, nil
}

// cargoConfigFiles lists Cargo configuration files that cargo would read for
// a build in the cache's work directory: ancestors' .cargo/config(.toml) and
// $CARGO_HOME/config(.toml).
func (c *Compiler) cargoConfigFiles() ([]string, error) {
	var dirs []string
	for dir := filepath.Join(c.cache.Dir(), "work"); ; {
		dirs = append(dirs, filepath.Join(dir, ".cargo"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if home := c.tc.CargoHome(); home != "" {
		dirs = append(dirs, home)
	}
	var files []string
	seen := map[string]bool{}
	for _, d := range dirs {
		for _, name := range []string{"config.toml", "config"} {
			p := filepath.Join(d, name)
			if seen[p] {
				continue
			}
			seen[p] = true
			fi, err := os.Stat(p)
			if err == nil && fi.Mode().IsRegular() {
				files = append(files, p)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
				return nil, fmt.Errorf("compiler: checking cargo config %s: %w", p, err)
			}
		}
	}
	return files, nil
}

func (c *Compiler) build(ctx context.Context, req Request, manifest, key string, inputs map[string]string, lock []byte) (*Artifact, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	start := time.Now()

	work, cleanup, err := c.cache.NewWorkDir()
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := cleanup(); cerr != nil {
			c.log.Warn("removing build directory failed", "dir", work, "error", cerr)
		}
	}()
	if err := writeProject(work, manifest, req.Sources); err != nil {
		return nil, err
	}

	// Dependency builds share a persistent target directory per dependency
	// set, so each dependency compiles once; plain snippets build in their
	// private work directory.
	targetDir := filepath.Join(work, "target")
	if lock != nil {
		lockText := strings.Replace(string(lock), `name = "`+lockRoot+`"`, `name = "`+req.packageName()+`"`, 1)
		if err := os.WriteFile(filepath.Join(work, "Cargo.lock"), []byte(lockText), 0o600); err != nil {
			return nil, fmt.Errorf("compiler: writing Cargo.lock: %w", err)
		}
		targets, err := c.cache.Subdir("targets")
		if err != nil {
			return nil, err
		}
		targetDir = filepath.Join(targets, req.depsFP[:16])
	}
	args := []string{
		"build",
		"--manifest-path", filepath.Join(work, "Cargo.toml"),
		"--message-format=json",
		"--color", "never",
		"--target", req.Target,
		"--target-dir", targetDir,
	}
	if !req.Network {
		args = append(args, "--offline")
	}
	if lock != nil {
		args = append(args, "--locked")
	}
	if req.profile() == ProfileRelease {
		args = append(args, "--release")
	}
	env := c.cargoEnv(req, "")
	if _, ok := env["CARGO_INCREMENTAL"]; !ok {
		env["CARGO_INCREMENTAL"] = "0"
	}

	c.log.Debug("running cargo", "key", key, "target", req.Target, "args", args)
	res, runErr := c.run(ctx, process.Spec{
		Path: c.tc.Cargo, Args: args, Env: c.tc.BaseEnv(env), Dir: work,
		Stderr: req.Progress, MaxOutput: maxCargoOutput, Timeout: req.Timeout,
	}, req.Network, req.dependencyPaths()...)
	if runErr != nil {
		switch {
		case errors.Is(runErr, errdefs.ErrTimeout):
			return nil, &Error{Reason: ReasonTimeout, Target: req.Target,
				Output: fmt.Sprintf("error: compilation exceeded the %s timeout", req.Timeout)}
		case res == nil || ctx.Err() != nil:
			return nil, runErr
		default:
			return nil, &Error{Reason: ReasonInternal, Target: req.Target, Output: runErr.Error()}
		}
	}

	mapper := newSourceMapper(req.Sources)
	parsed := parseCargoJSON(res.Stdout, mapper)
	stderr := string(res.Stderr)
	if res.ExitCode != 0 || !parsed.success {
		return nil, &Error{
			Reason: classify(parsed.diagnostics, stderr), Target: req.Target,
			Diagnostics: parsed.diagnostics, Output: networkHint(stderr, req.Network),
		}
	}
	output, err := findArtifact(parsed.artifacts, req)
	if err != nil {
		return nil, &Error{Reason: ReasonInternal, Target: req.Target, Diagnostics: parsed.diagnostics,
			Output: err.Error() + "\n" + stderr}
	}

	var warnings []Diagnostic
	for _, d := range parsed.diagnostics {
		if d.Level == "warning" {
			warnings = append(warnings, d)
		}
	}
	if len(warnings) > 0 {
		data, err := json.Marshal(warnings)
		if err != nil {
			return nil, fmt.Errorf("compiler: encoding warnings: %w", err)
		}
		inputs["warnings"] = string(data)
	}
	entry, err := c.cache.Put(key, output, cache.Meta{
		Kind: string(req.Kind), Target: req.Target, Backend: req.Backend, Inputs: inputs,
	})
	if err != nil {
		return nil, err
	}
	if n, err := c.cache.Trim(); err != nil {
		c.log.Warn("cache trim incomplete", "removed", n, "error", err)
	}
	return &Artifact{
		Path: entry.Path, Key: key, Kind: req.Kind, Target: req.Target, Size: entry.Meta.Size,
		Duration: time.Since(start), Warnings: warnings,
	}, nil
}

func writeProject(dir, manifest string, sources []SourceFile) error {
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(manifest), 0o600); err != nil {
		return fmt.Errorf("compiler: writing manifest: %w", err)
	}
	for _, s := range sources {
		p := filepath.Join(dir, "src", filepath.FromSlash(s.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("compiler: %w", err)
		}
		if err := os.WriteFile(p, s.Content, 0o600); err != nil {
			return fmt.Errorf("compiler: writing %s: %w", s.Path, err)
		}
	}
	return nil
}

// findArtifact picks the produced file from cargo's artifact messages.
func findArtifact(msgs []cargoMessage, req Request) (string, error) {
	name := req.packageName()
	for _, m := range msgs {
		if m.Target == nil || m.Target.Name != name {
			continue
		}
		if req.Kind == KindProgram && m.Executable != nil && *m.Executable != "" {
			return *m.Executable, nil
		}
		if req.Kind == KindLibrary {
			for _, f := range m.Filenames {
				switch strings.ToLower(filepath.Ext(f)) {
				case ".so", ".dylib", ".dll", ".wasm":
					return f, nil
				}
			}
		}
	}
	return "", fmt.Errorf("compiler: cargo reported no %s artifact for %s", req.Kind, name)
}

// flightGroup coalesces concurrent builds of the same key.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  *Artifact
	err  error
}

func (g *flightGroup) do(key string, fn func() (*Artifact, error)) (*Artifact, error, bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall{}
	}
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-call.done
		return call.val, call.err, true
	}
	// The default error is what followers see if fn panics.
	call := &flightCall{done: make(chan struct{}), err: errors.New("compiler: build aborted")}
	g.calls[key] = call
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(call.done)
	}()
	call.val, call.err = fn()
	return call.val, call.err, false
}
