# Public Go API

Import path: `github.com/alirezahematidev/ferrogo`. GoDoc on
[pkg.go.dev](https://pkg.go.dev/github.com/alirezahematidev/ferrogo) is
the reference; this page is the design overview.

## Runtime

```go
func New(opts ...Option) (*Runtime, error)   // cheap: needs no Rust toolchain
func (r *Runtime) Close() error              // cancels in-flight work, closes modules, releases resources

// Programs (fn main)
func (r *Runtime) Run(ctx context.Context, src Source, opts ...RunOption) (*Result, error)
func (r *Runtime) Build(ctx context.Context, src Source, kind ArtifactKind, opts ...CompileOption) (*Artifact, error)

// Libraries (compile once, call many times)
func (r *Runtime) Compile(ctx context.Context, src Source, opts ...CompileOption) (*Module, error)
func (r *Runtime) LoadModule(ctx context.Context, path string, opts ...CompileOption) (*Module, error) // prebuilt .wasm / .so / .dylib / .dll
func (r *Runtime) LoadWASM(ctx context.Context, wasm []byte, opts ...CompileOption) (*Module, error)  // e.g. from go:embed; no toolchain needed

// Introspection
func (r *Runtime) Toolchain(ctx context.Context) (*ToolchainInfo, error)
func (r *Runtime) Inspect(ctx context.Context) (*Report, error)
func (r *Runtime) CacheStats() (CacheStats, error)
func (r *Runtime) ClearCache() error
func CapabilitiesOf(b Backend) Capabilities
func Version() string // module version from the Go build info
```

A `Runtime` is safe for concurrent use and owns no global state. Several
runtimes in one process share nothing except the on-disk cache, which is
safe for concurrent processes.

### Sources

```go
type Source interface{ /* sealed */ }
type Code string      // Rust source text:  ferrogo.Code(`fn main() {}`)
type File string      // a single .rs file: ferrogo.File("main.rs")
type Fragments []Fragment // Rust assembled from pieces of other files, with source maps
type Project string   // a Cargo project:   ferrogo.Project("./tool") (requires Trusted)
```

| Spec name | FerroGo |
| --- | --- |
| `RunFile(p)` | `Run(ctx, ferrogo.File(p))` |
| `CompileFile(p)` | `Compile(ctx, ferrogo.File(p))` |
| `RunProject(d)` / `CompileProject(d)` | `Run(ctx, ferrogo.Project(d))` / `Compile(ctx, ferrogo.Project(d))` |
| `UnloadModule(m)` | `m.Close()` |

This keeps one method per operation.

## Modules and values

```go
func (m *Module) Call(ctx context.Context, name string, args ...Value) (Value, error)
func (m *Module) CallRaw(ctx context.Context, name string, sig Signature, args ...Value) (Value, error) // plain extern "C" scalars
func (m *Module) Functions(ctx context.Context) ([]Function, error) // {Name, Signature}
func (m *Module) Info() ModuleInfo                                  // backend, target, path, cache key
func (m *Module) Close() error

type Value struct{ /* immutable tagged union */ }
func Void() Value
func Bool(bool) Value
func Int32(int32) Value
func Int64(int64) Value
func Uint32(uint32) Value
func Uint64(uint64) Value
func Float32(float32) Value
func Float64(float64) Value
func String(string) Value
func Bytes([]byte) Value
func JSON(v any) (Value, error)
func RawJSON([]byte) (Value, error)

func (v Value) Kind() Kind
func (v Value) AsInt32() (int32, error)  // … one accessor per kind
func (v Value) AsString() (string, error)
func (v Value) Decode(dst any) error     // JSON value → Go value
func (v Value) String() string           // fmt.Stringer, e.g. "i32(30)"
```

`Call` takes a context. On WASM, the context (together with
`WithTimeout`) interrupts a running call. A native-library call can only be
cancelled before it starts, which `Capabilities.Interruptible` reports.
Argument kinds must match the Rust parameter types exactly. See
[abi.md](abi.md) for the type table and the semantics.

## Options

Options are typed by where they may be used. This is checked at compile
time.

| Interface | Accepted by |
| --- | --- |
| `Option` | `New` |
| `RunOption` | `Run` |
| `CompileOption` | `Build`, `Compile`, `LoadModule`, `LoadWASM` |
| `SharedOption` | all of the above. Set a default in `New`, override per call. |

| Option | Kind | Notes |
| --- | --- | --- |
| `WithBackend(b)` | shared | `BackendAuto` (default, means WASM), `BackendWASM`, `BackendNativeProcess`, `BackendNativeLibrary` |
| `WithTrust(level)` | runtime | `Untrusted` (default: WASM only) or `Trusted` (native backends allowed) |
| `WithCache(bool)`, `WithCacheDir(dir)`, `WithCacheMaxBytes(n)` | runtime | persistent artifact cache (default on, 2 GiB) |
| `WithToolchain(ToolchainConfig)` | runtime | explicit rustc/cargo paths, rustup toolchain, isolated homes |
| `WithMaxConcurrentBuilds(n)`, `WithLogger(l)` | runtime | build semaphore, structured logging |
| `WithConfigFile(path)` | runtime | strict `ferrogo.toml`; later options override it |
| `WithEdition`, `WithProfile`, `WithOptLevel`, `WithRustFlags` | shared | build settings |
| `WithBuildEnv`, `WithCompileTimeout`, `WithBuildOutput` | shared | build environment, timeout, progress stream |
| `WithDependencies(d...)` | shared | Cargo dependencies for `Code`/`File`/`Fragments` sources |
| `WithDependencyPolicy(p)` | runtime | which dependencies may be used; default none |
| `WithTimeout(d)` | shared | `Run` wall time, and per call on WASM modules |
| `WithMaxOutput(n)`, `WithMemoryLimit(n)` | shared | limits; unenforceable limits fail with `ErrUnsupported` |
| `WithWASMMemoryLimit(n)`, `WithMount(Mount)` | shared | WASM guest memory default, directory grants |
| `WithHostFunction(name, fn)` | shared | Go functions callable from WASM via `ferrogo::import!` |
| `WithWASMInterruption(bool)` | shared | `false` (Trusted only): faster WASM that timeouts cannot stop |
| `WithSandbox(Sandbox)` | shared | OS sandbox for native processes and native-process workers |
| `WithWASMDeterminism(seed)` | shared | fixed clocks and seeded randomness for WASM |
| `WithCompilerSandbox(bool)` | runtime | OS sandbox for cargo/rustc/build scripts |
| `WithEnv(map)`, `WithInheritEnv(names...)`, `WithDir(dir)`, `WithGracePeriod(d)` | shared | execution environment |
| `WithStdin(r)`, `WithStdout(w)`, `WithStderr(w)`, `WithArgs(a...)` | run | per-run streams and arguments |

## Results and errors

`Run` returns a non-nil `*Result` (stdout, stderr, exit code, backend,
artifact, duration, resource usage) whenever the program started, even
alongside an error.

| Error | Match with |
| --- | --- |
| Rust toolchain missing or too old | `errors.Is(err, ErrToolchainNotFound)` |
| target missing | `errors.Is(err, ErrTargetNotInstalled)`, `errors.As(err, **TargetError)` (gives `.Fix`) |
| compilation failed | `errors.Is(err, ErrCompilation)`, `errors.As(err, **CompileError)` (diagnostics) |
| program exited non-zero | `errors.As(err, **ExitError)` |
| Rust panic (program or call) | `errors.As(err, **PanicError)` |
| Rust function returned `Err` | `errors.As(err, **CallError)` |
| time limit | `errors.Is(err, ErrTimeout)` |
| output limit | `errors.Is(err, ErrOutputLimit)` |
| memory limit | `errors.Is(err, ErrMemoryLimit)` |
| backend cannot do it | `errors.Is(err, ErrUnsupported)`, `errors.As(err, **UnsupportedError)` |
| trust policy forbids backend | `errors.Is(err, ErrUntrusted)` |
| argument/result type mismatch, foreign ABI | `errors.Is(err, ErrABIMismatch)` |
| unknown function | `errors.Is(err, ErrFunctionNotFound)` |
| runtime or module closed | `errors.Is(err, ErrClosed)` |
| caller cancelled | `errors.Is(err, context.Canceled)` |
