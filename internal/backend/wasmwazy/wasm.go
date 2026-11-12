// Package wasmwazy executes WebAssembly (wasm32-wasip1) programs in the wazy
// runtime. Guests get no host capabilities beyond what RunSpec grants:
// stdio, explicit environment variables, explicit directory mounts, clocks
// and randomness. Memory is bounded and execution is interrupted when the
// context is done.
//
// shortcut: copy of internal/backend/wasm with wazero swapped for wazy and the
// uninterruptible cache removed; engine-neutral helpers (boundedBuffer,
// contextReader, trapReason, compile retry) are duplicated. Fold the two
// packages behind a small engine interface if wazy becomes the default.
package wasmwazy

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	mrand "math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/sys"

	"github.com/alirezahematidev/ferrogo/internal/backend"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/outcap"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// DefaultMemoryLimit is the guest memory limit when none is configured.
const DefaultMemoryLimit = 256 << 20

// pageSize is the WebAssembly page size.
const pageSize = 64 << 10

// maxPages is the largest wasm32 memory (4 GiB).
const maxPages = 65536

// Runner runs WASI programs. It is safe for concurrent use.
//
// wazy always compiles interruption checks in, so one compilation cache
// serves every run.
type Runner struct {
	cache wazy.CompilationCache
}

var _ backend.Runner = (*Runner)(nil)

// New returns a Runner. Machine code compiled from WebAssembly is cached in
// cacheDir (shared across processes), or in memory if cacheDir is empty.
func New(cacheDir string) (*Runner, error) {
	if cacheDir == "" {
		return &Runner{cache: wazy.NewCompilationCache()}, nil
	}
	cc, err := wazy.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("wasm: compilation cache: %w", err)
	}
	return &Runner{cache: cc}, nil
}

// Close releases the compilation cache.
func (r *Runner) Close() error {
	return r.cache.Close(context.Background())
}

// MemoryPages converts a byte limit to whole WebAssembly pages.
func MemoryPages(limit int64) (uint32, error) {
	if limit == 0 {
		limit = DefaultMemoryLimit
	}
	pages := limit / pageSize
	if pages < 1 {
		return 0, fmt.Errorf("wasm: memory limit %d is smaller than one 64 KiB page", limit)
	}
	return uint32(min(pages, maxPages)), nil
}

// Run instantiates the WASI program at spec.Artifact, which runs its _start
// function to completion.
func (r *Runner) Run(ctx context.Context, spec backend.RunSpec) (*backend.RunResult, error) {
	if spec.Dir != "" {
		return nil, errors.New("wasm: working directories are not supported; use mounts")
	}
	pages, err := MemoryPages(spec.MemoryLimit)
	if err != nil {
		return nil, err
	}
	for _, m := range spec.Mounts {
		if fi, err := os.Stat(m.HostPath); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("wasm: mount %s: not a directory", m.HostPath)
		}
	}
	code, err := os.ReadFile(spec.Artifact)
	if err != nil {
		return nil, fmt.Errorf("wasm: %w", err)
	}

	// Setup and machine-code compilation use the caller's context only: the
	// time limit covers execution, not compiling WASM on first use.
	rt := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
		WithCompilationCache(r.cache).
		WithMemoryLimitPages(pages))
	defer rt.Close(context.Background()) // releases the per-run runtime; nothing to report

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return nil, fmt.Errorf("wasm: instantiating WASI: %w", err)
	}
	if err := instantiateHost(ctx, rt, backend.ModuleSpec{Host: spec.Host, MaxPayload: wire.DefaultMaxPayload}); err != nil {
		return nil, err
	}
	compiled, err := r.compile(ctx, rt, code, spec.Artifact)
	if err != nil {
		return nil, err
	}

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	runCtx, cancelCause := context.WithCancelCause(runCtx)
	defer cancelCause(nil)
	capture := outcap.New(spec.MaxOutput, func() { cancelCause(errdefs.ErrOutputLimit) })
	stdout, stderr := capture.Stream(spec.Stdout), capture.Stream(spec.Stderr)

	fsCfg := wazy.NewFSConfig()
	for _, m := range spec.Mounts {
		if m.ReadOnly {
			fsCfg = fsCfg.WithReadOnlyDirMount(m.HostPath, m.GuestPath)
		} else {
			fsCfg = fsCfg.WithDirMount(m.HostPath, m.GuestPath)
		}
	}
	modCfg := wazy.NewModuleConfig().
		WithName("").
		WithArgs(append([]string{"main"}, spec.Args...)...).
		WithStdout(stdout).
		WithStderr(stderr).
		WithFSConfig(fsCfg).
		WithNanosleep(contextSleep(runCtx))
	modCfg = withEntropy(modCfg, spec.Deterministic, spec.Seed)
	if spec.Stdin != nil {
		modCfg = modCfg.WithStdin(&contextReader{ctx: runCtx, r: spec.Stdin})
	}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		modCfg = modCfg.WithEnv(k, v)
	}

	start := time.Now()
	mod, runErr := rt.InstantiateModule(runCtx, compiled, modCfg)
	finished := time.Now()
	res := &backend.RunResult{Duration: finished.Sub(start)}
	if mod != nil {
		if mem := mod.Memory(); mem != nil {
			res.PeakMemory = uint64(mem.Size()) // linear memory never shrinks
		}
		if err := mod.Close(context.Background()); err != nil {
			return nil, fmt.Errorf("wasm: closing module: %w", err)
		}
	}
	res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()

	// Decide by what happened before the program finished, not by how it
	// exited: when the context ends, sleeps and blocked reads return early
	// and wazy stops the guest asynchronously, so a guest can still finish
	// "normally" with a truncated sleep. Such a run did not complete within
	// its limits.
	var exitErr *sys.ExitError
	stopped := errors.As(runErr, &exitErr) &&
		(exitErr.ExitCode() == sys.ExitCodeContextCanceled || exitErr.ExitCode() == sys.ExitCodeDeadlineExceeded) ||
		errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) // wazy wraps the ctx error
	switch {
	case errors.Is(context.Cause(runCtx), errdefs.ErrOutputLimit):
		res.ExitCode = -1
		return res, fmt.Errorf("%w: more than %d bytes", errdefs.ErrOutputLimit, spec.MaxOutput)
	case ctx.Err() != nil && (stopped || endedBefore(ctx, finished)):
		res.ExitCode = -1
		return res, ctx.Err()
	case stopped || endedBefore(runCtx, finished):
		res.ExitCode = -1
		return res, fmt.Errorf("%w: program exceeded %s", errdefs.ErrTimeout, spec.Timeout)
	}
	if runErr == nil {
		return res, nil
	}
	if exitErr != nil {
		res.ExitCode = int(exitErr.ExitCode())
		return res, nil
	}
	res.ExitCode = -1
	res.Trap = trapReason(runErr)
	if strings.Contains(string(res.Stderr), "memory allocation of ") {
		return res, fmt.Errorf("%w: guest memory is limited to %d bytes", errdefs.ErrMemoryLimit, int64(pages)*pageSize)
	}
	return res, nil
}

// trapReason extracts "unreachable" from "module[] function[_start] failed:
// wasm error: unreachable\nwasm stack trace: ...".
func trapReason(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	if _, after, ok := strings.Cut(msg, "wasm error: "); ok {
		return after
	}
	return msg
}

// contextSleep implements WASI sleeps that end early when ctx is done, so a
// sleeping guest cannot outlive its timeout.
func contextSleep(ctx context.Context) sys.Nanosleep {
	return func(ns int64) {
		t := time.NewTimer(time.Duration(ns))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

// contextReader makes a blocking stdin read return when ctx is done. A read
// that is already blocked in the underlying reader finishes in the
// background; its data is discarded because the run is over.
type contextReader struct {
	ctx     context.Context
	r       io.Reader
	pending chan readResult
}

type readResult struct {
	data []byte
	err  error
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if c.pending == nil {
		ch := make(chan readResult, 1)
		buf := make([]byte, len(p))
		go func() {
			n, err := c.r.Read(buf)
			ch <- readResult{data: buf[:n], err: err}
		}()
		c.pending = ch
	}
	select {
	case res := <-c.pending:
		c.pending = nil
		n := copy(p, res.data)
		if n < len(res.data) {
			// p shrank between calls; keep the remainder for the next Read.
			rest := res.data[n:]
			ch := make(chan readResult, 1)
			ch <- readResult{data: rest, err: res.err}
			c.pending = ch
			return n, nil
		}
		return n, res.err
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}

// withEntropy configures clocks and randomness. By default guests get the
// real wall and monotonic clocks and cryptographic randomness; in
// deterministic mode they get wazy's fixed fake clocks and a ChaCha8
// stream seeded with seed, so every run observes exactly the same values.
func withEntropy(cfg wazy.ModuleConfig, deterministic bool, seed uint64) wazy.ModuleConfig {
	if !deterministic {
		return cfg.WithSysWalltime().WithSysNanotime().WithRandSource(crand.Reader)
	}
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	return cfg.WithRandSource(mrand.NewChaCha8(key))
}

// endedBefore reports whether ctx's deadline passed (or ctx was cancelled)
// no later than t.
func endedBefore(ctx context.Context, t time.Time) bool {
	if dl, ok := ctx.Deadline(); ok && !t.Before(dl) {
		return true
	}
	return errors.Is(ctx.Err(), context.Canceled)
}

// compileStripes serialises machine-code compilation of the same module
// (striped by key): concurrent compiles race on wazy's file cache, which
// fails on Windows when a cache file is renamed over one being written.
var compileStripes [64]sync.Mutex

// compile compiles code to machine code, through the persistent cache.
func (r *Runner) compile(ctx context.Context, rt wazy.Runtime, code []byte, key string) (wazy.CompiledModule, error) {
	if key == "" {
		key = strconv.FormatUint(uint64(crc32.ChecksumIEEE(code)), 16)
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	mu := &compileStripes[h.Sum32()%uint32(len(compileStripes))]
	mu.Lock()
	compiled, err := rt.CompileModule(ctx, code)
	if err != nil && runtime.GOOS == "windows" && strings.Contains(err.Error(), "rename") {
		// Another process stored the same cache entry concurrently; it is
		// complete now, so a second attempt loads it.
		compiled, err = rt.CompileModule(ctx, code)
	}
	mu.Unlock()
	if err != nil {
		if strings.Contains(err.Error(), "over limit of") {
			return nil, fmt.Errorf("%w: %v", errdefs.ErrMemoryLimit, err)
		}
		return nil, fmt.Errorf("wasm: compiling module: %w", err)
	}
	return compiled, nil
}
