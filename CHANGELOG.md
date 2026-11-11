# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/). Before v1.0.0 the public
API may change in minor releases; such changes are listed under
**Changed**, with migration notes.

## [Unreleased]

## [0.1.0] - 2026-11-11

First public release. All eight phases of the [roadmap](docs/roadmap.md)
are implemented. Open items are listed there, mainly an OS sandbox for
native processes on Windows.

### Added

- `Runtime` with `Run` (compile and execute `fn main` programs), `Build`
  (compile only), `Toolchain`, `Inspect`, `CacheStats` and `ClearCache`.
- Sources: `Code` (inline text) and `File` (single `.rs` file).
- **WASM backend (default):** `wasm32-wasip1` in wazero.
  - No host filesystem, network or environment unless granted (`WithMount`,
    `WithEnv`).
  - Memory limits (`WithWASMMemoryLimit`, `WithMemoryLimit`).
  - Interruption of infinite loops, sleeps and blocked stdin at the
    deadline.
  - Persistent compiled-code cache.
- **Native-process backend:** requires `WithTrust(Trusted)`.
  - Clean environment and a private working directory per run.
  - Process-group termination (SIGTERM, grace period, SIGKILL) on Unix.
  - CPU and peak-memory usage reporting.
- **Compilation engine:**
  - Toolchain detection (rustc ≥ 1.82, cargo, sysroot targets).
  - Generated Cargo projects with an allowlisted manifest (no injection).
  - A scrubbed compiler environment.
  - Structured rustc diagnostics re-mapped to your file names.
  - Compile timeouts, and coalescing of identical concurrent builds.
- **Artifact cache:**
  - SHA-256 keys over every build input (source, manifest, toolchain,
    target, flags, environment, Cargo config).
  - Atomic writes, verification on read, LRU size limit.
- Limits with finite defaults: execution timeout (30s), compile timeout
  (5m), output limit (4 MiB), WASM memory (256 MiB).
- Typed errors: `CompileError`, `ExitError`, `PanicError`, `TargetError`,
  `UnsupportedError`, and sentinels for `errors.Is`.
- **Compile once, call many times:** `Compile`, `LoadModule`, `LoadWASM`
  and `Module.Call`/`Functions`/`Close`.
  - Functions are exported with the `ferrogo::export!` macro, which is
    injected into library crates and needs no dependencies.
  - Supported types: `bool`, integers, floats, `String`, `Vec<u8>`,
    `ferrogo::Json` and `Result<T, E>`.
  - One envelope ABI ([docs/abi.md](docs/abi.md)) on both the WASM and the
    native-library backend.
  - Panics are contained and reported as `*PanicError`; `Err` results are
    reported as `*CallError`.
  - Raw `extern "C"` scalar exports are callable on WASM.
- **Inline Rust:** `/*ferrogo … */` blocks in `.go` files and
  `*.ferrogo.rs` files.
  - `ferrogo generate` writes typed Go wrappers and a `go:embed`-ed WASM
    module, so consumers need no toolchain.
  - Top-level `pub fn` items are exported automatically.
  - Diagnostics and panics map to `.go` lines.
  - Output is reproducible, and `-check` verifies it in CI.
- **Cargo dependencies** (`WithDependencies`), with a deny-by-default
  `DependencyPolicy`.
  - Registry, git and path dependencies; crate allowlists; network opt-in
    (otherwise builds are offline).
  - Locked, reproducible resolution; dependency builds shared across
    snippets.
- **`Project` sources** for existing Cargo projects (requires `Trusted`):
  in-place builds, `Cargo.lock`, custom profiles, incremental compilation.
- **Function calls on the native-process backend.**
  - Calls run in worker processes: they are interruptible, sandboxable and
    memory-limited.
  - Crashes are contained, and each worker has its own state.
- `Module.CallRaw` with a declared `Signature` for plain `extern "C"`
  functions. Declarations are checked against the module on WASM; native
  libraries use purego.
- `WithWASMDeterminism(seed)`: fixed clocks and seeded randomness.
- Host functions also work in WASM programs (`Run`).
- **Host functions:** Rust calls Go through `ferrogo::import!` and
  `WithHostFunction` (WASM). Errors and panics reach Rust as `Err`.
- `WithCompilerSandbox`: runs the toolchain under the OS sandbox,
  blocking `include_str!` reads of the home directory.
- Memory limits for native processes on Linux (`prlimit`) and Windows (Job
  Objects), plus process-tree termination on Windows.
- Generated wrappers carry the Rust doc comments.
- WASM calls are about 2× faster, and parallel calls about 3× faster.
- `WithSandbox`: an OS sandbox for native processes (macOS
  `sandbox-exec`, Linux bubblewrap) with no network, restricted writes and
  a hidden home directory.
- `WithWASMInterruption(false)` for trusted code: about 12× faster
  call-heavy WASM, using separate compilation caches.
- CLI: `call`, `inspect <artifact>`, `doctor --fix`, and config discovery
  in parent directories.
- `ferrogo abi` prints the ABI helper for vendoring into Cargo projects.
- `Fragments` source type: Rust assembled from pieces of other files, with
  source maps.
- **Native-library backend:** purego (Linux, macOS, NetBSD) or `syscall`
  (Windows), no cgo; requires `WithTrust(Trusted)`.
- `Value` type with typed constructors and `As*` accessors.
- `CapabilitiesOf` for run-time capability discovery. `BackendAuto` always
  resolves to WASM and never falls back to native.
- Strict `ferrogo.toml` configuration (`WithConfigFile`).
- `ferrogo` CLI: `run`, `build`, `compile`, `doctor`, `inspect`,
  `cache stats|clean`, `init` and `version`, with `--json` output.

[Unreleased]: https://github.com/alirezahematidev/ferrogo/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/alirezahematidev/ferrogo/releases/tag/v0.1.0
