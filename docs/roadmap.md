# Roadmap

A phase is marked done only when its implementation, tests and documentation
work. Status is updated in the same change that completes the work.

Legend: ✅ done · 🚧 in progress · ⬜ not started

## Phase 1: Foundation ✅

* ✅ Module, layout, Makefile, CI workflow (the CI matrix is configured; it has not run yet)
* ✅ Public API: `Runtime`, sources (`Code`, `File`), options typed by scope, results, errors, capabilities
* ✅ `internal/process`: argv-only exec, explicit env, process-group termination, output limits, rusage
* ✅ `internal/toolchain`: rustc/cargo/sysroot/target detection, scrubbed build environment
* ✅ `internal/compiler`: Cargo project generation, cargo JSON driver, diagnostics, source re-mapping, build coalescing
* ✅ `internal/cache`: SHA-256 keys, atomic writes, verification on read, stats, clear, LRU trim
* ✅ Native process backend: `Run` with stdin/env/args/dir/timeout/cancel/limits
* ✅ `ferrogo.toml` config file (strict)
* ✅ CLI: `version`, `doctor`, `run`, `build`, `compile`, `inspect`, `cache stats|clean`, `init`, `--json`
* ✅ Unit and integration tests (race-clean; staticcheck-clean for linux/darwin/windows); docs

## Phase 2: WASM runtime ✅

* ✅ wazero runtime per run with a shared persistent compilation cache (0.75 ms warm run)
* ✅ `Run` on `wasm32-wasip1`: stdin/stdout/stderr, env, args, exit codes, traps, panics
* ✅ Memory limit, context interruption (loops, sleeps, blocked stdin), output limits, read-only/read-write mounts
* ✅ `BackendAuto` → WASM; capability matrix; native-only options rejected explicitly
* ✅ Integration tests: timeouts, infinite loops, memory bombs, FS/network/env denial, mount escapes, concurrency
* ✅ Custom host functions and configurable clocks/randomness (delivered in Phase 4)

## Phase 3: Native dynamic libraries ✅

* ✅ Rust ABI helper (`internal/abi/ferrogo.rs`): `ferrogo::export!` (macro_rules, no dependencies), alloc/free, panic capture; warning-free on editions 2018–2024, native and wasm32
* ✅ `internal/wire` codec, strict decoding, fuzz test
* ✅ `cdylib` builds (`panic=unwind`), purego loader (Linux, macOS, NetBSD) and `syscall` loader (Windows); FreeBSD reported unsupported
* ✅ Envelope calls, pinned-memory ownership, `Close` semantics, trust enforcement, refusal of unenforceable options
* ✅ Export discovery from symbol tables without executing code (ELF, Mach-O, PE)

## Phase 4: Modules and the JIT-style API ✅

* ✅ `Compile`, `LoadModule`, `LoadWASM`, `Module.Call`, `Module.Functions` on WASM (instance pool) and native library
* ✅ Raw C convention on WASM (spec example C)
* ✅ Per-call timeouts and interruption on WASM; panics contained on both backends
* ✅ Concurrency tests (`-race`), call-overhead benchmarks (cached wazero handles: 6.3 µs → 2.0 µs)
* ✅ Declared-signature raw calls on native libraries (`CallRaw`; purego, not on Windows)
* ✅ Host functions for WASM modules (`ferrogo::import!` + `WithHostFunction`); guest-call reduction: 1.0 µs/call, parallel calls scale
* ✅ Deterministic clock and randomness options (`WithWASMDeterminism`)
* ✅ Host functions in WASM programs (`Run`), not only modules
* ✅ Native-process worker protocol: `Compile`/`Call` on native processes, with timeouts, crash recovery, sandbox and memory limits

## Phase 5: Inline Rust ✅

* ✅ `/*ferrogo … */` comment blocks and `*.ferrogo.rs` files; public `Fragments` source with per-line source maps
* ✅ Auto-export of top-level `pub fn` (Rust lexer that keeps line numbers; adversarial tests)
* ✅ `ferrogo generate`: typed Go wrappers from the compiled module's real signatures, `go:embed` WASM, collision detection, `-prefix`/`-type`
* ✅ Reproducible output; `-check` (no toolchain needed) for CI
* ✅ Diagnostics and panics mapped to `.go` positions (`file:line:col:`); end-to-end test from a separate module; `examples/inline`
* ✅ Rust doc comments copied into GoDoc; generator version recorded so `-check` catches format changes

## Phase 6: Cargo and dependencies ✅

* ✅ `WithDependencies` (registry with version and features, alternative registries, path, git with branch/tag/rev); allowlisted values plus TOML escaping
* ✅ `DependencyPolicy`: deny by default, per-source allowance, crate allowlist, network opt-in (otherwise `--offline`)
* ✅ Lockfile resolved once per dependency set, stored, part of the cache key, builds `--locked`
* ✅ Shared target directory per dependency set with stable rustflags (second snippet with `serde_json`: 98 ms vs 1.7 s)
* ✅ `Project` sources: in-place builds, multi-file crates, own `Cargo.lock`, custom profiles, incremental compilation, `cargo metadata` policy checks, trust required
* ✅ Config file `[[dependencies]]` / `[dependency_policy]`; CLI accepts project directories; `ferrogo abi` for vendoring the helper

## Phase 7: CLI and developer experience ✅

* ✅ `run`, `build`, `compile`, `generate`, `call`, `inspect [artifact]`, `doctor [--fix]`, `cache stats|clean`, `init`, `abi`, `version`
* ✅ `--json` everywhere, colours (respecting `NO_COLOR`), `-v` cargo progress, Go-style `file:line:col:` diagnostics for `generate`
* ✅ Config discovery in parent directories; CLI accepts `.rs` files, project directories and artifacts
* ✅ `call` converts arguments from the function's real Rust signature; flags stop at the function name, so `-5` is an argument

## Phase 8: Production hardening ✅

* ✅ `WithSandbox` for native processes: `sandbox-exec` (macOS, tested) and bubblewrap (Linux, CI); never falls back to unsandboxed
* ✅ `Trusted`-only `WithWASMInterruption(false)` (12× faster call-heavy code), with separate compilation caches (security regression test)
* ✅ Benchmarks: cold build, warm run, compute, call overhead, raw calls, JSON values, parallel calls
* ✅ Release automation (GoReleaser, proxy warm-up), Dependabot, govulncheck, cross-platform CI matrix (configured; first run pending)
* ✅ Windows Job Objects: process-tree termination and memory limits (CI-tested only)
* ✅ Linux memory limits via `prlimit`; macOS reports `ErrUnsupported` (not enforced by the OS)
* ✅ WASM call throughput: 2 guest calls per envelope call instead of 6 (parallel 2.5 µs → 0.8 µs)
* ✅ `WithCompilerSandbox`: OS sandbox for the toolchain (blocks `include_str!` exfiltration; part of the cache key)

## Open items (after v0.1.0)

* ⬜ OS sandbox for native processes and the compiler on Windows (AppContainer or restricted tokens)
* ⬜ Race-free Job Object assignment on Windows (start the process suspended)
* ⬜ First green run of the CI matrix on Linux, Windows and macOS amd64; confirm bubblewrap availability on Ubuntu runners
* ⬜ `CallRaw` for native libraries on Windows
