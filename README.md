# FerroGo

[![Go Reference](https://pkg.go.dev/badge/github.com/alirezahematidev/ferrogo.svg)](https://pkg.go.dev/github.com/alirezahematidev/ferrogo)
[![CI](https://github.com/alirezahematidev/ferrogo/actions/workflows/ci.yml/badge.svg)](https://github.com/alirezahematidev/ferrogo/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/alirezahematidev/ferrogo)](https://goreportcard.com/report/github.com/alirezahematidev/ferrogo)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Compile and run Rust from Go: in a WebAssembly sandbox by default, or as
native processes when you trust the code.

```go
rt, _ := ferrogo.New()
defer rt.Close()

res, err := rt.Run(ctx, ferrogo.Code(`
    fn main() {
        println!("Hello from Rust!");
    }
`))
fmt.Print(res.Stdout) // Hello from Rust!
```

FerroGo drives your Rust toolchain for you. It generates a Cargo project, runs
`cargo` with structured arguments and a scrubbed environment, parses
diagnostics, caches artifacts by a SHA-256 fingerprint of every input, and
executes the result with timeouts, output limits and (for WASM) memory and
filesystem isolation.

> **Status: v0.1, pre-1.0.** The API may still change before v1.0.
>
> * **Platforms:** developed and tested on macOS arm64. The CI matrix covers
>   Linux (amd64 and arm64), macOS (arm64 and amd64) and Windows.
> * **Open items:** see the [roadmap](docs/roadmap.md). The main one is an
>   OS sandbox for native processes on Windows.

**What you can do**

| | |
| --- | --- |
| Run Rust programs | `rt.Run(ctx, ferrogo.Code(...))`: sandboxed WASM by default, or native processes |
| Compile once, call many times | `rt.Compile` + `mod.Call`: WASM, a native library in-process, or native worker processes |
| Write Rust inside Go files | `/*ferrogo … */` + `go generate`: typed wrappers, WASM embedded, no toolchain needed at run time |
| Call Go from Rust | `ferrogo::import!` + `WithHostFunction` |
| Use crates | `WithDependencies` under a deny-by-default `DependencyPolicy`, or an existing Cargo `Project` |

## Why

Rust code is sometimes the right tool inside a Go service: user-supplied
logic, a fast kernel, or an existing Rust snippet. Wiring that up by hand
means temporary projects, compiler invocation, artifact management, process
plumbing and sandboxing. FerroGo packages all of it behind one API, with
secure defaults.

## Requirements

* Go ≥ 1.25.
* Rust ≥ 1.82 (stable), installed with [rustup](https://rustup.rs). Only
  needed where code is *compiled*.
* For the default WASM backend: `rustup target add wasm32-wasip1`.
* For native backends: a C linker (`xcode-select --install`, `build-essential`,
  or the Visual Studio Build Tools).

Run `ferrogo doctor` to check everything.

## Installation

```sh
go get github.com/alirezahematidev/ferrogo@latest                   # library
go install github.com/alirezahematidev/ferrogo/cmd/ferrogo@latest   # CLI
```

## Quick start

* Library: [examples/basic](examples/basic/main.go) and
  [docs/getting-started.md](docs/getting-started.md).
* CLI:

```sh
ferrogo doctor                      # check toolchain, targets, backends
ferrogo run main.rs                 # WASM sandbox (default)
ferrogo run main.rs --backend native -- arg1 arg2
ferrogo build main.rs -o main.wasm  # compile only
ferrogo generate ./mypkg            # compile Rust embedded in Go files
ferrogo call lib.rs add 10 20       # call an exported function (args typed from the Rust signature)
ferrogo inspect lib.wasm            # list a module's exports
ferrogo doctor --fix                # install missing targets with rustup (explicit)
ferrogo run main.rs --json          # machine-readable result
ferrogo cache stats
ferrogo init                        # write ferrogo.toml
```

## Backends

| | WASM (default) | Native process | Native library |
| --- | --- | --- | --- |
| Status | ✅ | ✅ | ✅ (Linux, macOS, NetBSD, Windows) |
| Run programs / call functions | ✅ / ✅ | ✅ / ✅ (worker processes) | — / ✅ |
| Runs | `wasm32-wasip1` in [wazero](https://wazero.io) | host executable as a child process | `.so`/`.dylib`/`.dll` loaded in-process |
| Sandbox | **yes**: no FS/network/env unless granted | no (your user's privileges) | no (your process) |
| Allowed for untrusted code | yes | no: needs `WithTrust(Trusted)` | no: needs `WithTrust(Trusted)` |
| Timeouts interrupt running code | yes | yes (process-group kill) | no |
| Memory limit | yes | no (returns `ErrUnsupported`) | no |
| Threads, sockets | no | yes | yes |

`BackendAuto` always means WASM. FerroGo never silently falls back to a
native backend. `ferrogo.CapabilitiesOf(b)` reports capabilities at run time,
and `rt.Inspect(ctx)` reports what is usable on this machine. Details:
[docs/backends.md](docs/backends.md).

## Runtime compilation

```go
res, err := rt.Run(ctx, ferrogo.File("tool.rs"),
    ferrogo.WithStdin(strings.NewReader(input)),
    ferrogo.WithArgs("--fast"),
    ferrogo.WithEnv(map[string]string{"LEVEL": "3"}),
    ferrogo.WithTimeout(5*time.Second),
)
var ce *ferrogo.CompileError
switch {
case errors.As(err, &ce):
    for _, d := range ce.Errors() {
        fmt.Println(d.Location(), d.Message) // tool.rs:3:18 mismatched types
    }
case errors.Is(err, ferrogo.ErrTimeout):
    // res holds the partial output
}
```

* `Run` returns a `*Result` (stdout, stderr, exit code, timing, artifact
  info) whenever the program started, even alongside an error.
* Non-zero exits are `*ExitError`. Rust panics are `*PanicError`, with the
  file and line mapped to your names.
* `Build` compiles without running.

## Compile once, call many times

```go
mod, err := rt.Compile(ctx, ferrogo.Code(`
    ferrogo::export! {
        fn add(a: i32, b: i32) -> i32 { a + b }
        fn greet(name: String) -> Result<String, String> {
            if name.is_empty() { return Err("empty name".into()); }
            Ok(format!("Hello, {name}!"))
        }
    }
`))
defer mod.Close()

sum, err := mod.Call(ctx, "add", ferrogo.Int32(10), ferrogo.Int32(20)) // i32(30)
_, err = mod.Call(ctx, "greet", ferrogo.String(""))                    // *ferrogo.CallError{"empty name"}
```

* `ferrogo::export!` is injected into every library crate.
* Functions take and return:
  * `bool`, `i32`/`i64`/`u32`/`u64`, `f32`/`f64`
  * `String`, `Vec<u8>`
  * `ferrogo::Json` (structs via `encoding/json`)
  * `Result<T, E>`
* Panics become `*PanicError` on every backend and never crash your
  process.
* Plain `#[no_mangle] extern "C"` scalar functions are callable on WASM
  (signature read from the module) and, with a declared `Signature`, on
  native libraries (`CallRaw`).
* On the native-process backend, calls run in worker processes: they can
  be timed out, sandboxed and memory-limited, and crashes are contained.
* One envelope ABI, identical on WASM and native libraries:
  [docs/abi.md](docs/abi.md).
* `LoadModule(path)` and `LoadWASM(bytes)` load prebuilt artifacts, for
  example from `go:embed`, with no toolchain at run time. Example:
  [examples/functions](examples/functions/main.go).

## Inline Rust

```go
//go:generate go run github.com/alirezahematidev/ferrogo/cmd/ferrogo generate

/*ferrogo
pub fn fib(n: u32) -> u64 {
    let (mut a, mut b) = (0u64, 1u64);
    for _ in 0..n { (a, b) = (b, a + b); }
    a
}
*/

func main() {
    n, err := Fib(ctx, 90) // generated, typed wrapper
}
```

* `ferrogo generate` compiles the block to WASM, embeds it with `go:embed`,
  and writes typed wrappers.
* Programs using the package need no Rust toolchain.
* Errors point at your `.go` lines, and `generate -check` keeps CI honest.
* Details: [docs/inline-rust.md](docs/inline-rust.md). Example:
  [examples/inline](examples/inline/main.go).

## Calling Go from Rust (host functions)

```go
rt, _ := ferrogo.New(ferrogo.WithHostFunction("lookup_user",
    func(ctx context.Context, args []ferrogo.Value) (ferrogo.Value, error) {
        id, _ := args[0].AsUint64()
        return ferrogo.String(users[id]), nil
    }))
mod, _ := rt.Compile(ctx, ferrogo.Code(`
    ferrogo::import! { fn lookup_user(id: u64) -> String; }
    ferrogo::export! {
        fn greet(id: u64) -> Result<String, String> { Ok(format!("hi {}", lookup_user(id)?)) }
    }
`))
```

WASM code can call exactly the functions you register and nothing else.
Go errors and panics reach Rust as `Err`.

## Dependencies and Cargo projects

```go
rt, _ := ferrogo.New(ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{
    AllowRegistry: true, // crates.io; AllowGit / AllowPath are separate
    Network:       false, // default: --offline, using crates already in ~/.cargo
}))
res, err := rt.Run(ctx, ferrogo.Code(`
    use serde_json::json;
    fn main() { println!("{}", json!({"language": "Rust", "fast": true})); }
`), ferrogo.WithDependencies(ferrogo.Dependency{
    Name: "serde_json", Version: "1", Features: []string{"preserve_order"},
}))
```

* **Denied by default.** Dependencies run build scripts and proc-macros on
  your machine, so nothing is used unless a `DependencyPolicy` allows it:
  * registry, git and path dependencies are allowed separately;
  * crate allowlists are supported;
  * network access is a separate opt-in.
* **Reproducible.** The resolved `Cargo.lock` is stored per dependency set
  and is part of the cache key.
* **Fast.** Compiled dependencies are shared: a second snippet using
  `serde_json` builds in about 0.1 s instead of 1.7 s.
* **Cargo projects.** `ferrogo.Project("./tool")` (CLI:
  `ferrogo run ./tool`) builds an existing project in place, with:
  * multi-file crates
  * its own `Cargo.lock`
  * custom profiles
  * incremental compilation

  Projects require `WithTrust(Trusted)` because their build scripts run on
  your machine. Their declared dependencies are checked against the policy.

## Caching

Artifacts are stored in `os.UserCacheDir()/ferrogo` under a SHA-256 key over:

* source
* generated `Cargo.toml`
* full `rustc -vV` output and `cargo -V`
* target, backend and profile
* edition and flags
* explicit build environment
* the contents of Cargo config files
* the ABI helper version

Writes are atomic, entries are verified on read, and least-recently-used
entries are evicted beyond 2 GiB. Machine code compiled from WASM is cached
too. Use `WithCacheDir`, `WithCacheMaxBytes` and `WithCache(false)`, or the
CLI's `ferrogo cache stats|clean`.

## Security

Treat Rust source as untrusted; FerroGo does by default.

* The compiler's environment is scrubbed, so `env!()` cannot read your
  secrets.
* No shell is ever used.
* WASM guests get nothing they are not granted.
* Every limit is finite by default:
  * execution timeout: 30s
  * compile timeout: 5m
  * output: 4 MiB
  * WASM memory: 256 MiB

Native processes can additionally run under the OS sandbox with
`WithSandbox` (`sandbox-exec` on macOS, bubblewrap on Linux), with memory
limits on Linux and Windows.

**`include_str!` can read any file the compiler can read.** Enable
`WithCompilerSandbox(true)` to run the toolchain itself under the OS
sandbox. It blocks reading your home directory at compile time. Otherwise,
compile untrusted code in a container or VM. See [docs/security.md](docs/security.md).

## Performance

Measured on Apple M-series, Go 1.25.6, Rust 1.94.1, wazero 1.12:

| | WASM | Native process |
| --- | --- | --- |
| Cold compile (trivial program) | ~106 ms | ~129 ms |
| Warm run, cached artifact | **0.75 ms** | 2.0 ms |
| `fib(30)` incl. startup | 26 ms | 3.5 ms |
| `Module.Call` of `add(i32, i32)` | 1.0 µs (raw C export: 0.55 µs) | 0.7 µs (native library) |

The WASM compute gap comes from interruption checks, which cost 8.7× on
this call-heavy code. Those checks are what make timeouts work on
untrusted code. Details and methodology: [docs/performance.md](docs/performance.md).
Reproduce with `go test -run '^$' -bench . .`.

## Platform support

| | Library + CLI | WASM backend | Native process | Native library |
| --- | --- | --- | --- | --- |
| macOS arm64 | tested | tested | tested | tested |
| Linux amd64 / arm64 | CI configured | CI configured | CI configured | CI configured |
| macOS amd64, Windows amd64 | CI configured | CI configured | CI configured (Windows: no process-tree kill yet) | CI configured |
| FreeBSD, other | builds | builds | builds | not supported |

"Tested" means tested locally. "CI configured" means the GitHub Actions
matrix covers it but has not run yet. Unit tests (`go test -short ./...`)
need no Rust toolchain.

## Limitations

* No machine-code JIT: code is compiled ahead of time by rustc, so the
  first build of a snippet takes 0.1–1s.
* `File` sources are single files. Use a `Project` source for multi-file
  crates.
* Dependencies are built offline unless `DependencyPolicy.Network` allows
  downloads, and their build scripts run on the build machine. Use
  `WithCompilerSandbox` to confine them.
* Native processes have no memory limit on macOS, and no OS sandbox on
  Windows.
* Host functions (Rust → Go) and deterministic mode work on WASM only.
* `CallRaw` on native libraries is unavailable on Windows.
* WASM (wasip1) has no threads and no sockets.
* On macOS, a newly built native binary takes about 300 ms to start the
  first time (OS assessment).

## Documentation

* [Getting started](docs/getting-started.md)
* [Architecture and design decisions](docs/architecture.md)
* [Public API](docs/api.md)
* [ABI and wire format](docs/abi.md)
* [Backends](docs/backends.md)
* [Inline Rust](docs/inline-rust.md)
* [Configuration](docs/configuration.md)
* [Security](docs/security.md)
* [Performance](docs/performance.md)
* [Troubleshooting](docs/troubleshooting.md)
* [Roadmap](docs/roadmap.md)
* [Releasing](docs/releasing.md)
* [Changelog](CHANGELOG.md)

## Development

```sh
make test         # unit + integration tests (integration tests skip without rustc)
make race         # with the race detector
make lint         # gofmt, go vet for linux/darwin/windows, staticcheck
make examples     # build and run every example
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately,
as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
