package ferrogo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/alirezahematidev/ferrogo/internal/abi"
	"github.com/alirezahematidev/ferrogo/internal/backend"
	"github.com/alirezahematidev/ferrogo/internal/backend/dylib"
	"github.com/alirezahematidev/ferrogo/internal/backend/native"
	"github.com/alirezahematidev/ferrogo/internal/backend/wasm"
	"github.com/alirezahematidev/ferrogo/internal/cache"
	"github.com/alirezahematidev/ferrogo/internal/compiler"
	"github.com/alirezahematidev/ferrogo/internal/toolchain"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// wasmTarget is the default WebAssembly target (see docs/architecture.md §4.1).
const wasmTarget = "wasm32-wasip1"

// Runtime compiles and executes Rust code. It is safe for concurrent use.
// Create one with New and release it with Close.
type Runtime struct {
	cfg     config
	cache   *cache.Cache
	tempDir string // set when caching is disabled

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	ops    sync.WaitGroup

	tcMu sync.Mutex
	comp *compiler.Compiler

	runners map[Backend]backend.Runner
	wasm    *wasm.Runner
	modules map[*Module]struct{} // open modules, closed by Close
}

// New creates a Runtime. It does not require a Rust toolchain: the toolchain
// is detected on first use.
func New(opts ...Option) (*Runtime, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		if o == nil {
			return nil, errors.New("ferrogo: nil option")
		}
		if err := o.applyRuntime(&cfg); err != nil {
			return nil, err
		}
	}
	r := &Runtime{
		cfg:     cfg,
		runners: map[Backend]backend.Runner{BackendNativeProcess: native.Runner{}},
		modules: map[*Module]struct{}{},
	}

	dir := cfg.cacheDir
	if !cfg.cache {
		tmp, err := os.MkdirTemp("", "ferrogo-")
		if err != nil {
			return nil, fmt.Errorf("ferrogo: %w", err)
		}
		r.tempDir, dir = tmp, tmp
	} else if dir == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("ferrogo: no cache directory (use WithCacheDir): %w", err)
		}
		dir = filepath.Join(userCache, "ferrogo")
	}
	c, err := cache.Open(dir, cfg.cacheMax)
	if err != nil {
		r.removeTemp()
		return nil, fmt.Errorf("ferrogo: %w", err)
	}
	r.cache = c
	// Machine code compiled from WebAssembly is cached alongside artifacts.
	wazeroDir := ""
	if cfg.cache {
		if wazeroDir, err = c.Subdir("wazero"); err != nil {
			r.removeTemp()
			return nil, fmt.Errorf("ferrogo: %w", err)
		}
	}
	wasmRunner, err := wasm.New(wazeroDir)
	if err != nil {
		r.removeTemp()
		return nil, fmt.Errorf("ferrogo: %w", err)
	}
	r.runners[BackendWASM] = wasmRunner
	r.wasm = wasmRunner
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// Close cancels in-flight operations, waits for them to return, and releases
// all resources. It is safe to call more than once.
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	r.cancel()
	r.ops.Wait()
	var errs []error
	r.mu.Lock()
	open := make([]*Module, 0, len(r.modules))
	for m := range r.modules {
		open = append(open, m)
	}
	r.mu.Unlock()
	for _, m := range open {
		errs = append(errs, m.Close())
	}
	for _, b := range r.runners {
		errs = append(errs, b.Close())
	}
	errs = append(errs, r.removeTemp())
	return errors.Join(errs...)
}

func (r *Runtime) removeTemp() error {
	if r.tempDir == "" {
		return nil
	}
	if err := os.RemoveAll(r.tempDir); err != nil {
		return fmt.Errorf("ferrogo: removing temporary directory: %w", err)
	}
	return nil
}

// begin registers an operation. The returned context is cancelled when
// either ctx or the Runtime is done.
func (r *Runtime) begin(ctx context.Context) (context.Context, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, ErrClosed
	}
	r.ops.Add(1)
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	return opCtx, func() { stop(); cancel(); r.ops.Done() }, nil
}

// closedErr converts a cancellation caused by Close into ErrClosed.
func (r *Runtime) closedErr(caller context.Context, err error) error {
	if err != nil && r.ctx.Err() != nil && caller.Err() == nil && errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return err
}

// compiler detects the toolchain on first use.
func (r *Runtime) compiler(ctx context.Context) (*compiler.Compiler, error) {
	r.tcMu.Lock()
	defer r.tcMu.Unlock()
	if r.comp != nil {
		return r.comp, nil
	}
	tc, err := toolchain.Detect(ctx, toolchain.Config(r.cfg.toolchain))
	if err != nil {
		return nil, err
	}
	comp := compiler.New(tc, r.cache, r.cfg.maxBuilds, r.cfg.logger)
	if r.cfg.compilerSandbox {
		if err := comp.EnableSandbox(); err != nil {
			return nil, err
		}
	}
	r.comp = comp
	return r.comp, nil
}

// resolveBackend applies BackendAuto and the trust policy.
func (r *Runtime) resolveBackend(b Backend) (Backend, error) {
	if b == BackendAuto {
		b = BackendWASM
	}
	if b.native() && r.cfg.trust != Trusted {
		return "", fmt.Errorf("%w: the %s backend runs code without a sandbox; "+
			"use ferrogo.WithTrust(ferrogo.Trusted) only for code you trust", ErrUntrusted, b)
	}
	return b, nil
}

// target returns the target triple for backend b and artifact kind.
func target(tc *toolchain.Toolchain, b Backend) (string, error) {
	if b == BackendWASM {
		return wasmTarget, nil
	}
	return tc.NativeTarget(runtime.GOOS, runtime.GOARCH)
}

// Build compiles src into an artifact for the configured backend without
// executing it. Build does not execute code, so the trust policy does not
// apply; it governs Run.
func (r *Runtime) Build(ctx context.Context, src Source, kind ArtifactKind, opts ...CompileOption) (*Artifact, error) {
	opCtx, done, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	cfg := r.cfg.clone()
	for _, o := range opts {
		if o == nil {
			return nil, errors.New("ferrogo: nil option")
		}
		if err := o.applyCompile(&cfg); err != nil {
			return nil, err
		}
	}
	b := cfg.backend
	if b == BackendAuto {
		b = BackendWASM
	}
	art, _, err := r.build(opCtx, &cfg, src, kind, b)
	return art, r.closedErr(ctx, err)
}

// build compiles src for backend b (already resolved).
func (r *Runtime) build(ctx context.Context, cfg *config, src Source, kind ArtifactKind, b Backend) (*Artifact, locator, error) {
	if src == nil {
		return nil, nil, errors.New("ferrogo: nil source")
	}
	if p, ok := src.(Project); ok {
		return r.buildProject(ctx, cfg, p, kind, b)
	}
	if cfg.profile != ProfileRelease && cfg.profile != ProfileDebug {
		return nil, nil, fmt.Errorf("ferrogo: custom profile %q needs a Project source that defines it", cfg.profile)
	}
	deps := make([]compiler.Dependency, len(cfg.deps))
	for i, d := range cfg.deps {
		if err := r.cfg.depPolicy.check(d.Name, d.source()); err != nil {
			return nil, nil, err
		}
		deps[i] = toCompilerDep(d)
	}
	if kind != ArtifactProgram && kind != ArtifactLibrary {
		return nil, nil, fmt.Errorf("ferrogo: invalid artifact kind %q", kind)
	}
	sources, err := src.sources(kind)
	if err != nil {
		return nil, nil, err
	}
	// The ABI helper (ferrogo::export!/import!) is part of every library and
	// every WASM program, which can call host functions.
	if kind == ArtifactLibrary || (b == BackendWASM && cfg.edition != "2015") {
		if cfg.edition == "2015" {
			return nil, nil, errors.New("ferrogo: libraries require Rust edition 2018 or later")
		}
		// Make ferrogo::export! available (docs/abi.md §2). Appending keeps
		// user line numbers unchanged.
		sources[0].Content = abi.Inject(sources[0].Content)
		sources = append(sources, compiler.SourceFile{
			Path: abi.FileName, Content: abi.Source, DisplayName: abi.FileName + " (FerroGo ABI helper)",
		})
	}
	comp, err := r.compiler(ctx)
	if err != nil {
		return nil, nil, err
	}
	tc := comp.Toolchain()
	triple, err := target(tc, b)
	if err != nil {
		return nil, nil, err
	}
	if !tc.HasTarget(triple) {
		return nil, nil, &TargetError{Target: triple, Backend: b, Fix: tc.TargetFix(triple)}
	}
	ckind := compiler.KindProgram
	panicStrategy := ""
	if kind == ArtifactLibrary {
		ckind = compiler.KindLibrary
		if b == BackendNativeLibrary || b == BackendNativeProcess {
			// Panics must be catchable at the FFI boundary (docs/abi.md §7).
			panicStrategy = "unwind"
		}
	}
	a, err := comp.Build(ctx, compiler.Request{
		Sources: sources, Kind: ckind, Target: triple, Backend: string(b),
		Profile: string(cfg.profile), OptLevel: cfg.optLevel, Edition: cfg.edition,
		Panic: panicStrategy, RustFlags: cfg.rustFlags, Env: cfg.buildEnv,
		Timeout: cfg.compileTimeout, Progress: cfg.buildOutput, NoCache: !r.cfg.cache,
		Dependencies: deps, Network: r.cfg.depPolicy.Network,
	})
	if err != nil {
		var ce *compiler.Error
		if errors.As(err, &ce) {
			if ce.Reason == compiler.ReasonTarget {
				return nil, nil, &TargetError{Target: triple, Backend: b, Fix: tc.TargetFix(triple)}
			}
			return nil, nil, newCompileError(ce)
		}
		return nil, nil, err
	}
	return &Artifact{
		Path: a.Path, Kind: kind, Backend: b, Target: a.Target, Key: a.Key, Size: a.Size,
		Cached: a.Cached, BuildDuration: a.Duration, Warnings: convertDiagnostics(a.Warnings),
	}, snippetLocator(sources), nil
}

// Run compiles src as a program (fn main) and executes it.
//
// Run returns a non-nil *Result whenever the program started, even when it
// also returns an error: on a non-zero exit (*ExitError, possibly wrapping a
// *PanicError), a timeout (ErrTimeout) or an output-limit violation
// (ErrOutputLimit), the Result holds the output produced so far.
func (r *Runtime) Run(ctx context.Context, src Source, opts ...RunOption) (*Result, error) {
	opCtx, done, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := r.run(opCtx, src, opts)
	return res, r.closedErr(ctx, err)
}

func (r *Runtime) run(ctx context.Context, src Source, opts []RunOption) (*Result, error) {
	cfg := r.cfg.clone()
	for _, o := range opts {
		if o == nil {
			return nil, errors.New("ferrogo: nil option")
		}
		if err := o.applyRun(&cfg); err != nil {
			return nil, err
		}
	}
	b, err := r.resolveBackend(cfg.backend)
	if err != nil {
		return nil, err
	}
	caps := CapabilitiesOf(b)
	if !caps.Run {
		return nil, &UnsupportedError{Backend: b, Feature: "running programs",
			Hint: "not implemented in this version; see docs/roadmap.md"}
	}
	if cfg.memoryLimit > 0 && !caps.MemoryLimit {
		return nil, &UnsupportedError{Backend: b, Feature: "memory limits",
			Hint: "remove WithMemoryLimit or use a backend that enforces it"}
	}
	if cfg.dir != "" && !caps.WorkingDir {
		return nil, &UnsupportedError{Backend: b, Feature: "working directories",
			Hint: "grant directories with WithMount instead"}
	}
	if cfg.sandbox != nil && b != BackendNativeProcess {
		return nil, &UnsupportedError{Backend: b, Feature: "WithSandbox",
			Hint: "the WASM backend is always sandboxed; grant directories with WithMount"}
	}
	if cfg.deterministic && b != BackendWASM {
		return nil, &UnsupportedError{Backend: b, Feature: "deterministic execution", Hint: "use the wasm backend"}
	}
	if len(cfg.mounts) > 0 && !caps.FilesystemIsolation {
		return nil, &UnsupportedError{Backend: b, Feature: "filesystem mounts",
			Hint: "native code can already access the whole filesystem; mounts would not restrict it"}
	}
	runner, ok := r.runners[b]
	if !ok {
		return nil, &UnsupportedError{Backend: b, Feature: "running programs"}
	}

	art, loc, err := r.build(ctx, &cfg, src, ArtifactProgram, b)
	if err != nil {
		return nil, err
	}
	if err := r.checkInterruption(&cfg, b); err != nil {
		return nil, err
	}
	memLimit := cfg.memoryLimit
	if b == BackendWASM && memLimit == 0 {
		memLimit = cfg.wasmMemory
	}
	mounts := make([]backend.Mount, len(cfg.mounts))
	for i, m := range cfg.mounts {
		mounts[i] = backend.Mount(m)
	}
	timeout := cfg.timeout
	if cfg.wasmUninterruptible && b == BackendWASM {
		timeout = 0 // cannot be enforced; checkInterruption rejected explicit timeouts
	}
	out, err := runner.Run(ctx, backend.RunSpec{
		Artifact: art.Path, Args: cfg.args, Env: execEnv(&cfg), Dir: cfg.dir,
		Stdin: cfg.stdin, Stdout: cfg.stdout, Stderr: cfg.stderr,
		Timeout: timeout, GracePeriod: cfg.grace, MaxOutput: cfg.maxOutput,
		MemoryLimit: memLimit, Mounts: mounts, Uninterruptible: cfg.wasmUninterruptible && b == BackendWASM,
		Sandbox: (*backend.Sandbox)(cfg.sandbox), Host: hostDispatcher(cfg.hostFuncs),
		Deterministic: cfg.deterministic, Seed: cfg.seed,
	})
	if out == nil {
		return nil, err
	}
	res := &Result{
		Stdout: string(out.Stdout), Stderr: string(out.Stderr), ExitCode: out.ExitCode,
		Backend: b, Artifact: art, Duration: out.Duration,
		Usage: ResourceUsage{UserCPU: out.UserTime, SystemCPU: out.SystemTime, PeakMemoryBytes: out.PeakMemory},
	}
	if err != nil {
		return res, err
	}
	if out.ExitCode != 0 || out.Signal != "" || out.Trap != "" {
		return res, &ExitError{Code: out.ExitCode, Signal: out.Signal, Trap: out.Trap, Panic: parsePanic(res.Stderr, loc)}
	}
	return res, nil
}

// execEnv builds the complete environment for executed code.
func execEnv(cfg *config) []string {
	env := map[string]string{}
	for _, name := range cfg.inheritEnv {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for k, v := range cfg.env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// Compile compiles src as a library and loads it, so its exported functions
// can be called repeatedly with Module.Call. Functions are exported with
// the ferrogo::export! macro, which FerroGo makes available in every
// library crate:
//
//	m, err := rt.Compile(ctx, ferrogo.Code(`
//	    ferrogo::export! {
//	        fn add(a: i32, b: i32) -> i32 { a + b }
//	    }
//	`))
//
// The module stays loaded until Module.Close or Runtime.Close.
func (r *Runtime) Compile(ctx context.Context, src Source, opts ...CompileOption) (*Module, error) {
	opCtx, done, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	cfg, b, err := r.moduleConfig(opts)
	if err != nil {
		return nil, err
	}
	art, loc, err := r.build(opCtx, &cfg, src, ArtifactLibrary, b)
	if err != nil {
		return nil, r.closedErr(ctx, err)
	}
	info := ModuleInfo{Backend: b, Target: art.Target, Path: art.Path, Key: art.Key}
	m, err := r.load(opCtx, &cfg, b, art.Path, nil, info, loc)
	return m, r.closedErr(ctx, err)
}

// LoadModule loads a prebuilt library: a .wasm module (WASM backend) or a
// native shared library (native-library backend, which requires
// WithTrust(Trusted)). The kind is detected from the file contents.
func (r *Runtime) LoadModule(ctx context.Context, path string, opts ...CompileOption) (*Module, error) {
	opCtx, done, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	head := make([]byte, 4)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ferrogo: %w", err)
	}
	_, err = io.ReadFull(f, head)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errNotLibrary, path)
	}
	detected := BackendNativeLibrary
	if string(head) == "\x00asm" {
		detected = BackendWASM
	}
	cfg, b, err := r.moduleConfig(append([]CompileOption{WithBackend(detected)}, opts...))
	if err != nil {
		return nil, err
	}
	if b != detected && !(detected == BackendNativeLibrary && b == BackendNativeProcess) {
		return nil, fmt.Errorf("ferrogo: %s is a %s artifact but the %s backend was requested", path, detected, b)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("ferrogo: %w", err)
	}
	return r.load(opCtx, &cfg, b, abs, nil, ModuleInfo{Backend: b, Path: abs}, nil)
}

// LoadWASM loads a prebuilt WebAssembly library from memory, for example an
// artifact embedded with go:embed. It needs no Rust toolchain.
func (r *Runtime) LoadWASM(ctx context.Context, code []byte, opts ...CompileOption) (*Module, error) {
	opCtx, done, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	cfg, b, err := r.moduleConfig(append([]CompileOption{WithBackend(BackendWASM)}, opts...))
	if err != nil {
		return nil, err
	}
	if b != BackendWASM {
		return nil, fmt.Errorf("ferrogo: LoadWASM cannot use the %s backend", b)
	}
	return r.load(opCtx, &cfg, b, "", code, ModuleInfo{Backend: b}, nil)
}

// moduleConfig applies per-call options and checks that the backend can
// load modules with them.
func (r *Runtime) moduleConfig(opts []CompileOption) (config, Backend, error) {
	cfg := r.cfg.clone()
	for _, o := range opts {
		if o == nil {
			return cfg, "", errors.New("ferrogo: nil option")
		}
		if err := o.applyCompile(&cfg); err != nil {
			return cfg, "", err
		}
	}
	b, err := r.resolveBackend(cfg.backend)
	if err != nil {
		return cfg, "", err
	}
	if !CapabilitiesOf(b).Call {
		return cfg, "", &UnsupportedError{Backend: b, Feature: "calling exported functions",
			Hint: "use the wasm or native-library backend"}
	}
	if err := r.checkInterruption(&cfg, b); err != nil {
		return cfg, "", err
	}
	if cfg.sandbox != nil && b != BackendNativeProcess {
		return cfg, "", &UnsupportedError{Backend: b, Feature: "WithSandbox",
			Hint: "only native processes use the OS sandbox (WASM is always sandboxed)"}
	}
	if cfg.deterministic && b != BackendWASM {
		return cfg, "", &UnsupportedError{Backend: b, Feature: "deterministic execution", Hint: "use the wasm backend"}
	}
	if b == BackendNativeProcess {
		caps := CapabilitiesOf(b)
		switch {
		case cfg.memoryLimit > 0 && !caps.MemoryLimit:
			return cfg, "", &UnsupportedError{Backend: b, Feature: "memory limits"}
		case len(cfg.mounts) > 0:
			return cfg, "", &UnsupportedError{Backend: b, Feature: "filesystem mounts", Hint: "use WithSandbox"}
		case len(cfg.hostFuncs) > 0:
			return cfg, "", &UnsupportedError{Backend: b, Feature: "host functions", Hint: "use the wasm backend for Rust-to-Go calls"}
		}
	}
	if b == BackendNativeLibrary {
		unsupported := func(feature, hint string) (config, Backend, error) {
			return cfg, "", &UnsupportedError{Backend: b, Feature: feature, Hint: hint}
		}
		switch {
		case cfg.timeoutSet && cfg.timeout > 0:
			return unsupported("call timeouts", "native calls cannot be interrupted; pass WithTimeout(0) or use the wasm backend")
		case cfg.memoryLimit > 0:
			return unsupported("memory limits", "native code shares the Go process's memory")
		case len(cfg.mounts) > 0:
			return unsupported("filesystem mounts", "native code can access the whole filesystem")
		case len(cfg.env) > 0 || len(cfg.inheritEnv) > 0:
			return unsupported("per-module environment variables", "native code sees the Go process's environment")
		case len(cfg.hostFuncs) > 0:
			return unsupported("host functions", "use the wasm backend for Rust-to-Go calls")
		}
	}
	return cfg, b, nil
}

// load opens a library artifact (from path, or code for WASM).
func (r *Runtime) load(ctx context.Context, cfg *config, b Backend, path string, code []byte, info ModuleInfo, loc locator) (*Module, error) {
	var impl backend.Module
	switch b {
	case BackendWASM:
		if code == nil {
			var err error
			if code, err = os.ReadFile(path); err != nil {
				return nil, fmt.Errorf("ferrogo: %w", err)
			}
		}
		memLimit := cfg.memoryLimit
		if memLimit == 0 {
			memLimit = cfg.wasmMemory
		}
		mounts := make([]backend.Mount, len(cfg.mounts))
		for i, mt := range cfg.mounts {
			mounts[i] = backend.Mount(mt)
		}
		callTimeout := cfg.timeout
		if cfg.wasmUninterruptible {
			callTimeout = 0 // cannot be enforced; see checkInterruption
		}
		m, err := r.wasm.LoadModule(ctx, code, backend.ModuleSpec{
			Env: execEnv(cfg), Mounts: mounts, MemoryLimit: memLimit,
			CallTimeout: callTimeout, MaxPayload: wire.DefaultMaxPayload,
			Uninterruptible: cfg.wasmUninterruptible, Host: hostDispatcher(cfg.hostFuncs),
			Deterministic: cfg.deterministic, Seed: cfg.seed,
		})
		if err != nil {
			return nil, err
		}
		impl = m
	case BackendNativeLibrary:
		lib, err := dylib.Open(path, wire.DefaultMaxPayload)
		if err != nil {
			return nil, err
		}
		impl = lib
	case BackendNativeProcess:
		worker, err := r.workerArtifact(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("ferrogo: building the worker program: %w", err)
		}
		m, err := native.OpenWorker(ctx, native.WorkerSpec{
			Worker: worker, Library: path, Env: execEnv(cfg), CallTimeout: cfg.timeout,
			MemoryLimit: cfg.memoryLimit, Sandbox: (*backend.Sandbox)(cfg.sandbox), MaxPayload: wire.DefaultMaxPayload,
		})
		if err != nil {
			return nil, err
		}
		impl = m
	default:
		return nil, &UnsupportedError{Backend: b, Feature: "calling exported functions"}
	}
	m := newModule(r, impl, info, loc, wire.DefaultMaxPayload)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		// Close raced with this load; do not leak the module.
		return nil, errors.Join(ErrClosed, impl.Close())
	}
	r.modules[m] = struct{}{}
	return m, nil
}

func (r *Runtime) forgetModule(m *Module) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.modules, m)
}

// buildProject builds a Cargo project in place (see Project).
func (r *Runtime) buildProject(ctx context.Context, cfg *config, p Project, kind ArtifactKind, b Backend) (*Artifact, locator, error) {
	if r.cfg.trust != Trusted {
		return nil, nil, fmt.Errorf("%w: building a Cargo project runs its build script and dependencies' build code on this machine; "+
			"use ferrogo.WithTrust(ferrogo.Trusted) for projects you trust", ErrUntrusted)
	}
	// WithDependencies applies to Code, File and Fragments sources only; a
	// project declares its dependencies in its Cargo.toml (checked below).
	dir, err := filepath.Abs(string(p))
	if err != nil {
		return nil, nil, fmt.Errorf("ferrogo: %w", err)
	}
	comp, err := r.compiler(ctx)
	if err != nil {
		return nil, nil, err
	}
	tc := comp.Toolchain()
	triple, err := target(tc, b)
	if err != nil {
		return nil, nil, err
	}
	if !tc.HasTarget(triple) {
		return nil, nil, &TargetError{Target: triple, Backend: b, Fix: tc.TargetFix(triple)}
	}
	info, err := comp.InspectProject(ctx, dir)
	if err != nil {
		var ce *compiler.Error
		if errors.As(err, &ce) {
			return nil, nil, newCompileError(ce)
		}
		return nil, nil, err
	}
	for _, d := range info.Dependencies {
		if d.Kind == "dev" {
			continue // dev-dependencies are not built
		}
		if err := r.cfg.depPolicy.check(d.Name, d.Source); err != nil {
			return nil, nil, fmt.Errorf("project %s: %w", info.Name, err)
		}
	}
	ckind := compiler.KindProgram
	if kind == ArtifactLibrary {
		ckind = compiler.KindLibrary
	}
	a, err := comp.BuildProject(ctx, compiler.ProjectRequest{
		Dir: dir, DisplayDir: filepath.ToSlash(string(p)), Kind: ckind, Target: triple, Backend: string(b),
		Profile: string(cfg.profile), RustFlags: cfg.rustFlags, Env: cfg.buildEnv,
		Timeout: cfg.compileTimeout, Progress: cfg.buildOutput, Network: r.cfg.depPolicy.Network,
	}, info)
	if err != nil {
		var ce *compiler.Error
		if errors.As(err, &ce) {
			if ce.Reason == compiler.ReasonTarget {
				return nil, nil, &TargetError{Target: triple, Backend: b, Fix: tc.TargetFix(triple)}
			}
			return nil, nil, newCompileError(ce)
		}
		return nil, nil, err
	}
	return &Artifact{
		Path: a.Path, Kind: kind, Backend: b, Target: a.Target, Key: a.Key, Size: a.Size,
		Cached: a.Cached, BuildDuration: a.Duration, Warnings: convertDiagnostics(a.Warnings),
	}, projectLocator(filepath.ToSlash(string(p))), nil
}

// checkInterruption validates WithWASMInterruption(false).
func (r *Runtime) checkInterruption(cfg *config, b Backend) error {
	if !cfg.wasmUninterruptible || b != BackendWASM {
		return nil
	}
	if r.cfg.trust != Trusted {
		return fmt.Errorf("%w: WithWASMInterruption(false) lets guest code run forever; it requires WithTrust(Trusted)", ErrUntrusted)
	}
	if cfg.timeoutSet && cfg.timeout > 0 {
		return &UnsupportedError{Backend: b, Feature: "timeouts without interruption checks",
			Hint: "remove WithTimeout or keep WithWASMInterruption(true)"}
	}
	return nil
}

// workerArtifact builds (once; then from the cache) the generic worker
// program that serves native-process modules. It uses default build
// settings, independent of the caller's dependencies and flags.
func (r *Runtime) workerArtifact(ctx context.Context, cfg *config) (string, error) {
	wcfg := defaultConfig()
	wcfg.compileTimeout = cfg.compileTimeout
	wcfg.buildOutput = cfg.buildOutput
	art, _, err := r.build(ctx, &wcfg, Code(abi.WorkerSource), ArtifactProgram, BackendNativeProcess)
	if err != nil {
		return "", err
	}
	return art.Path, nil
}
