# Performance

All numbers come from `go test -run '^$' -bench . -benchtime 20x .` on an
Apple M-series laptop (darwin/arm64), with Go 1.25.6, Rust 1.94.1 and
wazero 1.12.0. Treat them as orders of magnitude, not guarantees, and rerun
them on your hardware.

## Results

| Benchmark | WASM | Native process |
| --- | --- | --- |
| `BuildCold`: compile a small program, cache disabled | 106 ms | 129 ms |
| `RunWarm`: cached artifact, trivial work (`fib(1)`) | **0.75 ms** | 2.0 ms |
| `RunCompute`: cached artifact, `fib(30)` (≈1.6 M calls) | 26.4 ms | 3.5 ms |

## Function call overhead

`go test -run '^$' -bench ModuleCall -benchtime 2000x .` calls functions of
an already-loaded module:

| Call | WASM | Native library |
| --- | --- | --- |
| `add(i32, i32) -> i32` (envelope) | 1.0 µs | 0.7–1.1 µs |
| `greet(String) -> Result<String, String>` | 1.6 µs | 0.6–0.7 µs |
| `reverse(Vec<u8>)` with 64 KiB in and out | 256 µs | 19 µs |
| `calculate(i32, i32)` raw C export (no envelope) | 0.55 µs | n/a |

On the native-process backend each call is a pipe round-trip to a worker
process: about 10–17 µs per call, or 5.5 µs per call aggregate with 10
concurrent callers. That buys process isolation, interruptible calls and
crash containment.

An envelope call costs these steps:

1. encoding the arguments;
2. copying them into guest memory, or pinning them on native;
3. the call itself;
4. copying the result out;
5. `ferrogo_free`.

The raw C path skips all of that, so it is the fastest way to call scalar
WASM functions.

Three implementation details matter here:

1. **Call handles.** wazero builds a new call engine on every
   `ExportedFunction` lookup, and rebuilds its export map on every
   `ExportedFunctions` call. FerroGo caches both per module and per
   instance: an envelope call went from 6.3 µs and 39 KB to 2.0 µs.
2. **Watcher goroutines.** With interruption checks on, wazero starts one
   goroutine per guest call. FerroGo keeps the out-slot and input buffer
   inside each instance, so an envelope call makes two guest calls instead
   of six: 2.0 µs → 1.0 µs, and 3× better parallel throughput.

### Concurrency

`BenchmarkModuleCallParallel` (10 goroutines, one module):

| Backend | Aggregate time per call |
| --- | --- |
| Native library | 0.15 µs |
| WASM | 0.8 µs (previously 2.5 µs, before the guest-call reduction above) |

WASM instances are pooled per module: each concurrent call gets its own
instance and memory.

## Where the time goes

* **Cold compilation** is rustc and LLVM. Release builds (the default) are
  slower to compile than `WithProfile(ferrogo.ProfileDebug)`. Every later run
  of identical inputs is a cache hit.
* **A warm WASM run** costs, in order:
  1. a SHA-256 integrity check of the cached artifact;
  2. creating a wazero runtime;
  3. loading machine code from wazero's file-backed compilation cache;
  4. instantiating the module.

  This is cheaper than spawning a process.
* **A warm native run** costs a process spawn. On macOS, the **first**
  execution of each newly built binary also takes about 300 ms while the OS
  assesses it. This happens once per artifact.

## The cost of interruptibility on WASM

FerroGo enables wazero's `WithCloseOnContextDone`. It inserts termination
checks at function entries and loop back-edges, and those checks are what
let a timeout or a cancelled context stop an infinite loop in untrusted
code.

On call-heavy code the checks are expensive. Measured with the same
`fib(30)` module:

| wazero configuration | time per run |
| --- | --- |
| `WithCloseOnContextDone(true)` (FerroGo) | 25.5 ms |
| `WithCloseOnContextDone(false)` | 2.9 ms |

Without the checks, WASM is close to native speed. They are on by default,
because a timeout that cannot stop guest code is not a security control.

For trusted code, `WithWASMInterruption(false)` removes them. In
`TestWASMInterruptionOptOut`, `fib(30)` takes 24.4 ms with the checks and
2.0 ms without. Compiled code is cached separately per setting; see
[security.md](security.md).

Loop-heavy code with few calls pays much less.

## Guidance

* Reuse one `Runtime`. It caches toolchain detection, which costs about
  30 ms of process spawns on first use.
* Keep the cache enabled. It is persistent across processes, so a service
  restart keeps warm artifacts and WASM machine code.
* Use the native-process backend for long-running CPU-bound work you trust.
* Use WASM for many short runs and for any untrusted code.
* `WithMaxConcurrentBuilds` bounds parallel `cargo` invocations (default
  `GOMAXPROCS/2`). Identical concurrent builds are coalesced into one.
