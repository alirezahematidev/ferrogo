# Troubleshooting

Start with `ferrogo doctor`. It checks rustc, cargo, the linker, the cache
and every backend, and prints a fix for each problem.

| Symptom | Error | Fix |
| --- | --- | --- |
| `rustc not found on PATH` | `ErrToolchainNotFound` | Install Rust from <https://rustup.rs>, or point `WithToolchain(ToolchainConfig{Rustc: …})` at it. |
| `rustc X is older than the minimum supported 1.82.0` | `ErrToolchainNotFound` | `rustup update stable` |
| `target wasm32-wasip1 … is not installed; fix: rustup target add wasm32-wasip1` | `ErrTargetNotInstalled` (`*TargetError.Fix`) | Run the printed command. FerroGo never installs targets on its own. |
| `forbidden by trust policy` | `ErrUntrusted` | Native backends need `WithTrust(ferrogo.Trusted)` (CLI: `--trust`, implied by `--backend native`). See [security.md](security.md) first. |
| `the native-process backend does not support memory limits` | `ErrUnsupported` | Remove `WithMemoryLimit` or use the WASM backend. FerroGo never silently ignores a limit. |
| `linking with cc failed` | `*CompileError`, `Reason == "link"` | Install a C toolchain: `xcode-select --install` (macOS), `build-essential` (Debian/Ubuntu), Visual Studio Build Tools (Windows). |
| `error: unsafe attribute used without unsafe` on `#[no_mangle]` | `*CompileError` | Edition 2024 requires `#[unsafe(no_mangle)]`. Use the default edition (2021) or update the attribute. |
| `env!("X")` fails at compile time | `*CompileError` | The compiler environment is scrubbed. Pass `WithBuildEnv(map[string]string{"X": …})`. |
| Program cannot see an environment variable | | Executed code gets an empty environment. Use `WithEnv` or `WithInheritEnv`. |
| `output limit exceeded` | `ErrOutputLimit` | Raise `WithMaxOutput` (CLI `--max-output`; `0` disables). |
| `time limit exceeded` | `ErrTimeout` | Raise `WithTimeout` / `WithCompileTimeout` (CLI `--timeout`, `--compile-timeout`). |
| `forbidden by dependency policy` | `ErrDependencyPolicy` | Allow the dependency's source with `WithDependencyPolicy(DependencyPolicy{AllowRegistry: true})` (or `AllowGit`/`AllowPath`); read docs/security.md first. |
| `crate is missing from the local Cargo cache` | `*CompileError`, `Reason == "dependency"` | Builds are offline by default. Set `DependencyPolicy{Network: true}`, or fetch the crate once with cargo. |
| `OS sandbox: bubblewrap cannot create a sandbox here` | `ErrUnsupported` | Install bubblewrap and allow unprivileged user namespaces (Ubuntu 24.04: AppArmor `userns` restrictions), or drop `WithSandbox`. |
| `ferrogo call lib.rs f -5` treats `-5` as a flag | | Put flags **before** the source: `ferrogo call --json lib.rs f -5`. Everything after the function name is an argument. |
| `Blocking waiting for file lock` in verbose output | | Another cargo process holds a lock on the same Cargo home. This is harmless and resolves when that process finishes. |

## Performance surprises

* **First compile is slow; later runs are instant.** Artifacts are cached
  by a SHA-256 fingerprint of every build input. `Result.Artifact.Cached`
  tells you which case you hit.
* **macOS: the first execution of a new native binary takes about 300 ms.**
  The OS assesses every newly created executable once. FerroGo cannot avoid
  this; subsequent runs of the cached artifact start in about 1 ms.
* **`release` vs `debug`.** Release builds (the default) compile more
  slowly and run faster. For quick iteration use `WithProfile(ferrogo.ProfileDebug)`
  (CLI `--profile debug`).

## Getting more detail

* CLI: `-v` streams cargo's progress, and `--json` prints structured
  results and diagnostics.
* Library: `WithLogger(slog.New(...))` logs cargo invocations at debug level.
  `WithBuildOutput(os.Stderr)` streams cargo's progress.
* `*CompileError.Rendered()` returns rustc's full output, with file names
  mapped to yours.
