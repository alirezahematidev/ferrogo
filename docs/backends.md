# Backends

Query capabilities at run time with `ferrogo.CapabilitiesOf(backend)`. Ask
what is usable on this machine (toolchain, targets, trust) with
`rt.Inspect(ctx)` or `ferrogo inspect`.

## WASM (`BackendWASM`, default)

**How it works.** Programs are compiled for `wasm32-wasip1`. Each `Run`:

1. creates a fresh wazero runtime with the memory limit applied;
2. compiles the module to machine code (served from a persistent
   compilation cache after the first time);
3. instantiates it with exactly the granted WASI capabilities, and runs
   `_start`.

A warm run takes about 0.75 ms.

**What guests get**

* stdin, stdout and stderr (captured and bounded)
* arguments
* environment variables you pass
* directories you mount, read-only or read-write
* wall and monotonic clocks
* sleep (interruptible)
* cryptographically secure randomness

Nothing else: no host environment, no filesystem outside mounts, no
sockets, no processes.

**Limits**

| Limit | Default | Option | On violation |
| --- | --- | --- | --- |
| Memory | 256 MiB | `WithWASMMemoryLimit` / `WithMemoryLimit` | `ErrMemoryLimit`: Rust's `memory allocation of N bytes failed` abort |
| Time | 30s | `WithTimeout` | `ErrTimeout`: infinite loops, sleeps and blocked stdin reads are interrupted |
| Output | 4 MiB | `WithMaxOutput` | `ErrOutputLimit` |

**Semantics to know**

* Rust panics abort on WASM (`panic=abort`), so the guest traps with
  `unreachable`. FerroGo reports `*ExitError{Trap: "unreachable"}` wrapping
  `*PanicError`. Use `errors.As(err, &panicErr)`, not exit code 101, to
  detect panics portably.
* There is no working directory. Use `WithMount`; `WithDir` returns
  `ErrUnsupported`.
* `std::thread::spawn` and `std::net` return errors: wasip1 has no threads
  or sockets.
* Each `Run` is a fresh instance: no state survives between runs.
* **Host functions:** `WithHostFunction` lets Rust call Go through
  `ferrogo::import!`. See [abi.md](abi.md) §10.
* **Deterministic mode:** `WithWASMDeterminism(seed)` gives fixed fake
  clocks and seeded randomness, so runs and calls repeat exactly. Native
  backends reject it.
* **Trusted speed mode:** `WithWASMInterruption(false)` (with `Trusted`)
  removes the interruption checks. `fib(30)` takes 2.0 ms instead of
  24 ms, but the code can no longer be stopped while it runs.

## wazy (`BackendWazy`, optional)

Same as WASM in every respect: same `wasm32-wasip1` artifacts (built and
cached once for both), same guest capabilities, limits, sandbox and trust
rules (allowed for untrusted code). Select it with
`WithBackend(ferrogo.BackendWazy)` or `--backend wazy`. The difference is
the engine: [wazy](https://github.com/samyfodil/wazy), a wazero fork whose
compiled code always checks for interruption (no watcher goroutine per
call). Consequences:

* `WithWASMInterruption(false)` returns `ErrUnsupported` (after
  `ErrUntrusted` for untrusted runtimes); it is never ignored.
* Machine code is cached in its own `wazy` cache subdirectory, never mixed
  with wazero's.
* `ferrogo generate --backend wazy` writes the same files as
  `--backend wasm`; generated code runs on whichever engine its `Runtime`
  uses.

### wazy vs wazero

| | wazero (`BackendWASM`) | wazy (`BackendWazy`) |
| --- | --- | --- |
| Default | yes | no, opt-in |
| Compile and call speed | baseline | faster; see [performance.md](performance.md#wazero-vs-wazy) |
| Interruption | watcher per call; can be turned off for trusted code | always compiled in; no opt-out |
| Memory | allocated per instance | linear-memory buffers come from a process-wide pool, so peak memory with many concurrent one-shot runs is up to about 2x wazero's; buffers are reused, not leaked |
| Dependency | wazero 1.x (stable API) | wazy pre-1.0 (API may change between minor versions) |
| Windows | any supported Go | build with Go 1.27.2 or newer (see [security.md](security.md)) |

## Native process (`BackendNativeProcess`)

**How it works.** Programs are compiled for the Go process's own platform
(`GOOS`/`GOARCH`; the toolchain's host ABI variant is kept) and executed
directly with `os/exec`. No shell is involved.

**Requires** `WithTrust(ferrogo.Trusted)`. The program runs with your user's
privileges.

**Function calls (`Compile` + `Module.Call`).** Calls run in a pool of
worker processes that load the compiled library ([abi.md](abi.md) §11).
You get native speed inside the call, and each call costs about 10–17 µs of
pipe round-trip. Calls also get everything a process boundary offers:

* timeouts and cancellation (the worker is killed and replaced)
* crash containment
* a clean environment (`WithEnv`)
* memory limits (Linux and Windows)
* `WithSandbox`

**What you get**

* the full standard library: threads, sockets, filesystem, processes
* native speed
* stdin, arguments, environment and working directory control
* process-group termination on Unix: `SIGTERM`, then `SIGKILL` after the
  grace period
* CPU time and peak RSS in `Result.Usage`

**Optional OS sandbox:** `WithSandbox` gives no network, writes only to the
working and granted directories, and no reads of the home directory. It
uses `sandbox-exec` on macOS and bubblewrap on Linux. See
[security.md](security.md).

**Memory limits:**

| Platform | Mechanism |
| --- | --- |
| Linux | `prlimit` (`RLIMIT_AS`) |
| Windows | Job Object; the whole process tree is also killed on termination |
| macOS | `ErrUnsupported`: not enforced by the OS |

## Native library (`BackendNativeLibrary`)

**How it works.** `Compile` builds a `cdylib` for the Go process's
platform, with `panic = "unwind"`, and loads it into the process:

* Linux, macOS and NetBSD: purego, so no cgo or C compiler is needed at run
  time.
* Windows: `syscall`.
* FreeBSD: not supported, because purego's cgo-free mode does not build
  there. `CapabilitiesOf` reports `Call: false`.

Every call goes through the envelope ABI ([abi.md](abi.md)), whose entry
points take only integers and pointers.

**Requires** `WithTrust(ferrogo.Trusted)`. There is **no isolation**: the
code runs inside your Go process with its privileges and its memory.

**What you get**

* native speed (about 1.4 µs per `add` call, including encoding)
* one shared state: Rust statics persist across calls
* concurrent calls, under Rust's `Send`/`Sync` rules
* panics caught with `catch_unwind` and reported as `*PanicError`

**What you cannot get**, and what FerroGo refuses rather than ignores:

* **Call timeouts.** A running native call cannot be interrupted. An
  explicit `WithTimeout` makes `Compile` fail with `ErrUnsupported`; the
  context is checked before each call.
* **Memory limits, mounts and a per-module environment.** These are
  rejected with `ErrUnsupported`.
* **Reliable unloading.** `Module.Close` releases FerroGo's handle and
  makes further calls fail with `ErrClosed`. macOS keeps Rust dylibs mapped,
  because `std` registers thread-local destructors.
* **Raw C calls.** Native symbols carry no types, so `#[no_mangle]`
  functions without `ferrogo::export!` are not callable; FerroGo never
  guesses a signature.
* **Hardened-runtime macOS apps** cannot `dlopen` unsigned libraries. Use
  WASM there.

## Choosing

| Situation | Backend |
| --- | --- |
| Code from users, plugins, the network | WASM |
| Your own code that needs threads, sockets, or full speed | Native process |
| Hot function calls into trusted Rust | Native library |
| Function calls on untrusted code | WASM (`Compile` + `Module.Call`) |
| Same as WASM on the wazy engine (always interruptible) | wazy (`BackendWazy`) |
| Portability (same artifact everywhere, embeddable) | WASM |
