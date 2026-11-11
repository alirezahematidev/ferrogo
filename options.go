package ferrogo

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Defaults. Every limit is finite by default; pass 0 to the corresponding
// option to disable it explicitly.
const (
	DefaultTimeout        = 30 * time.Second
	DefaultCompileTimeout = 5 * time.Minute
	DefaultMaxOutput      = 4 << 20 // bytes, stdout+stderr combined
	DefaultGracePeriod    = 2 * time.Second
	DefaultCacheMaxBytes  = 2 << 30
	DefaultEdition        = "2021"
	// DefaultWASMMemoryLimit is the WASM guest memory limit unless
	// WithWASMMemoryLimit or WithMemoryLimit says otherwise.
	DefaultWASMMemoryLimit = 256 << 20
)

// Profile is a build profile.
type Profile string

// Build profiles.
const (
	ProfileRelease Profile = "release"
	ProfileDebug   Profile = "debug"
)

var profilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ToolchainConfig selects the Rust toolchain. Zero values mean auto-detection.
type ToolchainConfig struct {
	// Rustc is the rustc executable (default: rustc on PATH).
	Rustc string
	// Cargo is the cargo executable (default: cargo next to the sysroot, then PATH).
	Cargo string
	// Toolchain is a rustup toolchain name such as "stable" or "1.94.1".
	Toolchain string
	// RustupHome and CargoHome isolate rustup and cargo state, e.g. for a
	// per-project toolchain that needs no system-wide installation.
	RustupHome string
	CargoHome  string
}

// config is the single source of truth for settings. Options, the config
// file and the CLI all modify it through Option values.
type config struct {
	backend   Backend
	trust     TrustLevel
	cache     bool
	cacheDir  string
	cacheMax  int64
	toolchain ToolchainConfig
	maxBuilds int
	logger    *slog.Logger

	edition        string
	profile        Profile
	optLevel       string
	rustFlags      []string
	buildEnv       map[string]string
	compileTimeout time.Duration
	buildOutput    io.Writer

	timeout     time.Duration
	timeoutSet  bool // WithTimeout was given explicitly
	maxOutput   int64
	memoryLimit int64
	env         map[string]string
	inheritEnv  []string
	dir         string
	grace       time.Duration
	mounts      []Mount
	wasmMemory  int64

	deps      []Dependency
	depPolicy DependencyPolicy

	wasmUninterruptible bool
	sandbox             *Sandbox
	compilerSandbox     bool
	hostFuncs           map[string]HostFunc // replaced, never mutated in place
	deterministic       bool
	seed                uint64

	// Per-run only.
	stdin          io.Reader
	stdout, stderr io.Writer
	args           []string
}

func defaultConfig() config {
	return config{
		backend:        BackendAuto,
		trust:          Untrusted,
		cache:          true,
		cacheMax:       DefaultCacheMaxBytes,
		maxBuilds:      max(1, runtime.GOMAXPROCS(0)/2),
		logger:         slog.New(slog.DiscardHandler),
		edition:        DefaultEdition,
		profile:        ProfileRelease,
		compileTimeout: DefaultCompileTimeout,
		timeout:        DefaultTimeout,
		maxOutput:      DefaultMaxOutput,
		grace:          DefaultGracePeriod,
		wasmMemory:     DefaultWASMMemoryLimit,
	}
}

// clone returns a copy that shares no mutable state with c.
func (c config) clone() config {
	c.rustFlags = slices.Clone(c.rustFlags)
	c.buildEnv = maps.Clone(c.buildEnv)
	c.env = maps.Clone(c.env)
	c.inheritEnv = slices.Clone(c.inheritEnv)
	c.args = slices.Clone(c.args)
	c.mounts = slices.Clone(c.mounts)
	c.deps = slices.Clone(c.deps)
	return c
}

// Option configures a Runtime. Pass options to New.
type Option interface{ applyRuntime(*config) error }

// RunOption configures a single Run call.
type RunOption interface{ applyRun(*config) error }

// CompileOption configures a single Build call.
type CompileOption interface{ applyCompile(*config) error }

// SharedOption is accepted by New (as a runtime default) and by Run and
// Build (as a per-call override).
type SharedOption interface {
	Option
	RunOption
	CompileOption
}

type runtimeOption func(*config) error

func (f runtimeOption) applyRuntime(c *config) error { return f(c) }

type sharedOption func(*config) error

func (f sharedOption) applyRuntime(c *config) error { return f(c) }
func (f sharedOption) applyRun(c *config) error     { return f(c) }
func (f sharedOption) applyCompile(c *config) error { return f(c) }

type runOnlyOption func(*config) error

func (f runOnlyOption) applyRun(c *config) error { return f(c) }

// WithBackend selects the execution backend. The default is BackendAuto.
func WithBackend(b Backend) SharedOption {
	return sharedOption(func(c *config) error {
		if !b.valid() {
			return fmt.Errorf("ferrogo: unknown backend %q", string(b))
		}
		c.backend = b
		return nil
	})
}

// WithTrust sets the trust policy. The default, Untrusted, only allows the
// sandboxed WASM backend. Trusted also allows native backends, which run
// code with the host's privileges. See docs/security.md.
func WithTrust(level TrustLevel) Option {
	return runtimeOption(func(c *config) error {
		if level != Untrusted && level != Trusted {
			return fmt.Errorf("ferrogo: invalid trust level %d", int(level))
		}
		c.trust = level
		return nil
	})
}

// WithCache enables or disables the persistent artifact cache (default
// enabled). When disabled, every build compiles from scratch and artifacts
// live in a temporary directory removed by Runtime.Close.
func WithCache(enabled bool) Option {
	return runtimeOption(func(c *config) error { c.cache = enabled; return nil })
}

// WithCacheDir sets the cache directory (default: os.UserCacheDir()/ferrogo).
func WithCacheDir(dir string) Option {
	return runtimeOption(func(c *config) error {
		if dir == "" {
			return errors.New("ferrogo: empty cache directory")
		}
		c.cacheDir = dir
		return nil
	})
}

// WithCacheMaxBytes bounds the total artifact size in the cache; least
// recently used artifacts are evicted beyond it. 0 means unlimited.
func WithCacheMaxBytes(n int64) Option {
	return runtimeOption(func(c *config) error {
		if n < 0 {
			return errors.New("ferrogo: negative cache size limit")
		}
		c.cacheMax = n
		return nil
	})
}

// WithToolchain selects the Rust toolchain explicitly.
func WithToolchain(tc ToolchainConfig) Option {
	return runtimeOption(func(c *config) error { c.toolchain = tc; return nil })
}

// WithMaxConcurrentBuilds bounds simultaneous compilations
// (default GOMAXPROCS/2, minimum 1).
func WithMaxConcurrentBuilds(n int) Option {
	return runtimeOption(func(c *config) error {
		if n < 1 {
			return errors.New("ferrogo: max concurrent builds must be at least 1")
		}
		c.maxBuilds = n
		return nil
	})
}

// WithLogger sets a structured logger for diagnostics about FerroGo itself
// (cache maintenance, cargo invocations at debug level). Default: discard.
func WithLogger(l *slog.Logger) Option {
	return runtimeOption(func(c *config) error {
		if l == nil {
			return errors.New("ferrogo: nil logger")
		}
		c.logger = l
		return nil
	})
}

// WithEdition sets the Rust edition ("2015", "2018", "2021" or "2024").
// The default is 2021, because edition 2024 rejects bare #[no_mangle].
func WithEdition(edition string) SharedOption {
	return sharedOption(func(c *config) error {
		if !slices.Contains([]string{"2015", "2018", "2021", "2024"}, edition) {
			return fmt.Errorf("ferrogo: invalid edition %q", edition)
		}
		c.edition = edition
		return nil
	})
}

// WithProfile selects the build profile (default ProfileRelease). Project
// sources may also use custom profiles defined in their Cargo.toml.
func WithProfile(p Profile) SharedOption {
	return sharedOption(func(c *config) error {
		if !profilePattern.MatchString(string(p)) {
			return fmt.Errorf("ferrogo: invalid profile %q", string(p))
		}
		c.profile = p
		return nil
	})
}

// WithOptLevel overrides the optimisation level: "0"–"3", "s" or "z".
// Empty restores the profile default.
func WithOptLevel(level string) SharedOption {
	return sharedOption(func(c *config) error {
		if !slices.Contains([]string{"", "0", "1", "2", "3", "s", "z"}, level) {
			return fmt.Errorf("ferrogo: invalid opt-level %q", level)
		}
		c.optLevel = level
		return nil
	})
}

// WithRustFlags appends rustc flags (each element is one argument; nothing
// is split on spaces). They are part of the cache key.
func WithRustFlags(flags ...string) SharedOption {
	return sharedOption(func(c *config) error {
		for _, f := range flags {
			if strings.ContainsRune(f, '\x1f') || strings.ContainsRune(f, 0) {
				return fmt.Errorf("ferrogo: invalid rustc flag %q", f)
			}
		}
		c.rustFlags = append(c.rustFlags, flags...)
		return nil
	})
}

// WithBuildEnv adds environment variables visible to the compiler and build
// scripts. The host environment is otherwise scrubbed during builds.
func WithBuildEnv(env map[string]string) SharedOption {
	return sharedOption(func(c *config) error {
		if err := validateEnv(env); err != nil {
			return err
		}
		if c.buildEnv == nil {
			c.buildEnv = map[string]string{}
		}
		maps.Copy(c.buildEnv, env)
		return nil
	})
}

// WithCompileTimeout bounds compilation time (default 5m; 0 disables).
func WithCompileTimeout(d time.Duration) SharedOption {
	return sharedOption(func(c *config) error {
		if d < 0 {
			return errors.New("ferrogo: negative compile timeout")
		}
		c.compileTimeout = d
		return nil
	})
}

// WithBuildOutput streams cargo's progress messages ("Compiling ...") to w.
func WithBuildOutput(w io.Writer) SharedOption {
	return sharedOption(func(c *config) error { c.buildOutput = w; return nil })
}

// WithTimeout bounds execution wall time of Run and of each Module.Call on
// the WASM backend (default 30s; 0 disables). Native-library calls cannot
// be interrupted, so an explicit timeout makes Compile fail with
// ErrUnsupported on that backend.
func WithTimeout(d time.Duration) SharedOption {
	return sharedOption(func(c *config) error {
		if d < 0 {
			return errors.New("ferrogo: negative timeout")
		}
		c.timeout, c.timeoutSet = d, true
		return nil
	})
}

// WithMaxOutput bounds combined stdout+stderr bytes (default 4 MiB; 0
// disables). A program exceeding it is terminated with ErrOutputLimit.
func WithMaxOutput(n int64) SharedOption {
	return sharedOption(func(c *config) error {
		if n < 0 {
			return errors.New("ferrogo: negative output limit")
		}
		c.maxOutput = n
		return nil
	})
}

// WithMemoryLimit bounds guest memory in bytes. Backends that cannot enforce
// it fail with ErrUnsupported rather than ignoring it. 0 means the backend
// default.
func WithMemoryLimit(n int64) SharedOption {
	return sharedOption(func(c *config) error {
		if n < 0 {
			return errors.New("ferrogo: negative memory limit")
		}
		c.memoryLimit = n
		return nil
	})
}

// WithEnv adds environment variables for executed code. Executed code sees
// no host environment variables unless granted here or by WithInheritEnv.
func WithEnv(env map[string]string) SharedOption {
	return sharedOption(func(c *config) error {
		if err := validateEnv(env); err != nil {
			return err
		}
		if c.env == nil {
			c.env = map[string]string{}
		}
		maps.Copy(c.env, env)
		return nil
	})
}

// WithInheritEnv passes the named host environment variables (if set) to
// executed code.
func WithInheritEnv(names ...string) SharedOption {
	return sharedOption(func(c *config) error {
		for _, n := range names {
			if n == "" || strings.ContainsAny(n, "=\x00") {
				return fmt.Errorf("ferrogo: invalid environment variable name %q", n)
			}
		}
		c.inheritEnv = append(c.inheritEnv, names...)
		return nil
	})
}

// WithDir sets the working directory of executed programs. By default each
// run gets a fresh private temporary directory.
func WithDir(dir string) SharedOption {
	return sharedOption(func(c *config) error { c.dir = dir; return nil })
}

// WithGracePeriod sets how long a program may take to exit after a graceful
// termination request before it is killed (default 2s).
func WithGracePeriod(d time.Duration) SharedOption {
	return sharedOption(func(c *config) error {
		if d <= 0 {
			return errors.New("ferrogo: grace period must be positive")
		}
		c.grace = d
		return nil
	})
}

// WithWASMMemoryLimit sets the default WASM guest memory limit in bytes
// (default 256 MiB, rounded down to 64 KiB pages, at most 4 GiB). Unlike
// WithMemoryLimit it only applies to the WASM backend.
func WithWASMMemoryLimit(n int64) SharedOption {
	return sharedOption(func(c *config) error {
		if n < 64<<10 {
			return errors.New("ferrogo: WASM memory limit must be at least 64 KiB")
		}
		c.wasmMemory = n
		return nil
	})
}

// WithWASMInterruption controls the checks that let timeouts and
// cancellation stop running WASM code (default enabled). Disabling them
// makes call-heavy code up to ~9x faster (docs/performance.md) but running
// code can then only finish on its own: it requires WithTrust(Trusted), and
// an explicit WithTimeout is rejected with ErrUnsupported. Compiled code is
// cached separately for each setting.
func WithWASMInterruption(enabled bool) SharedOption {
	return sharedOption(func(c *config) error { c.wasmUninterruptible = !enabled; return nil })
}

// Sandbox is an OS-level isolation policy for the native-process backend
// (see WithSandbox).
type Sandbox struct {
	// AllowNetwork permits network access.
	AllowNetwork bool
	// Writable lists host directories the program may write, besides its
	// private working directory.
	Writable []string
	// Readable lists directories inside the user's home directory the
	// program may read; file contents in the home directory are otherwise
	// hidden.
	Readable []string
}

// WithSandbox runs native processes under the operating system's sandbox:
// sandbox-exec on macOS, bubblewrap on Linux. The program then has no
// network (unless allowed), cannot write outside its working directory and
// the Writable directories, and cannot read the user's home directory
// (except Readable). Where no sandbox is available, Run fails with
// ErrUnsupported instead of running unsandboxed. The WASM backend is always
// sandboxed and rejects this option (use WithMount).
func WithSandbox(s Sandbox) SharedOption {
	return sharedOption(func(c *config) error {
		cp := Sandbox{AllowNetwork: s.AllowNetwork}
		for _, list := range []struct {
			in  []string
			out *[]string
		}{{s.Writable, &cp.Writable}, {s.Readable, &cp.Readable}} {
			for _, p := range list.in {
				abs, err := filepath.Abs(p)
				if err != nil {
					return fmt.Errorf("ferrogo: sandbox path %q: %w", p, err)
				}
				*list.out = append(*list.out, abs)
			}
		}
		c.sandbox = &cp
		return nil
	})
}

// WithCompilerSandbox runs the Rust toolchain itself (cargo, rustc, build
// scripts, proc-macros, the linker) under the OS sandbox: sandbox-exec on
// macOS, bubblewrap on Linux. Compilation then cannot read the user's home
// directory (beyond the toolchain, Cargo home, FerroGo cache and the
// build's inputs), cannot write elsewhere, and has no network unless the
// DependencyPolicy allows downloads. This closes the include_str!/
// include_bytes! exfiltration path for untrusted source. Where no sandbox is
// available, builds fail with ErrUnsupported.
func WithCompilerSandbox(enabled bool) Option {
	return runtimeOption(func(c *config) error { c.compilerSandbox = enabled; return nil })
}

// WithWASMDeterminism gives WASM code fixed, fake wall and monotonic clocks
// and a pseudo-random source (ChaCha8) seeded with seed, so repeated runs
// of the same code with the same inputs behave identically: useful for
// tests, replays and consensus-style workloads. Sleeps still take real time
// (and remain interruptible). It does not apply to native backends.
func WithWASMDeterminism(seed uint64) SharedOption {
	return sharedOption(func(c *config) error { c.deterministic, c.seed = true, seed; return nil })
}

// Mount grants sandboxed code access to a host directory.
type Mount struct {
	// HostPath is the host directory to expose.
	HostPath string
	// GuestPath is where the guest sees it, e.g. "/data".
	GuestPath string
	// ReadOnly forbids writes when true.
	ReadOnly bool
}

// WithMount grants sandboxed (WASM) code access to a host directory. WASM
// code sees no filesystem at all without mounts. Native backends have full
// filesystem access, so they reject mounts with ErrUnsupported rather than
// pretend to restrict access.
func WithMount(m Mount) SharedOption {
	return sharedOption(func(c *config) error {
		if m.HostPath == "" || m.GuestPath == "" || !strings.HasPrefix(m.GuestPath, "/") {
			return fmt.Errorf("ferrogo: invalid mount %+v: need a host path and an absolute guest path", m)
		}
		c.mounts = append(c.mounts, m)
		return nil
	})
}

// WithStdin connects r to the program's standard input.
func WithStdin(r io.Reader) RunOption {
	return runOnlyOption(func(c *config) error { c.stdin = r; return nil })
}

// WithStdout streams the program's standard output to w as it is produced,
// in addition to capturing it in Result.Stdout.
func WithStdout(w io.Writer) RunOption {
	return runOnlyOption(func(c *config) error { c.stdout = w; return nil })
}

// WithStderr streams the program's standard error to w as it is produced,
// in addition to capturing it in Result.Stderr.
func WithStderr(w io.Writer) RunOption {
	return runOnlyOption(func(c *config) error { c.stderr = w; return nil })
}

// WithArgs sets the program's command-line arguments (after argv[0]).
func WithArgs(args ...string) RunOption {
	return runOnlyOption(func(c *config) error {
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return errors.New("ferrogo: argument contains NUL byte")
			}
		}
		c.args = append(c.args, args...)
		return nil
	})
}

func validateEnv(env map[string]string) error {
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return fmt.Errorf("ferrogo: invalid environment variable %q", k)
		}
	}
	return nil
}
