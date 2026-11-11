# Security model

FerroGo treats every Rust source it compiles as **untrusted** unless you say
otherwise. This page covers what FerroGo protects against, what it does not,
and how to configure it.

## Threats at compile time

Compiling Rust can read files and run code on the build machine:

| Vector | Effect | FerroGo mitigation |
| --- | --- | --- |
| `include_str!("/home/u/.ssh/id_rsa")`, `include_bytes!` | Embeds any file readable by the compiler into the artifact. The program can then print it. | **`WithCompilerSandbox(true)`** runs cargo, rustc, build scripts and the linker under the OS sandbox (`sandbox-exec` on macOS, bubblewrap on Linux). The home directory becomes unreadable except the toolchain, Cargo home, FerroGo cache and the build's inputs; writes are confined, and there is no network unless the dependency policy allows downloads. The cache key includes the setting, so artifacts built without the sandbox are never served to a sandboxed runtime. Verified by `TestCompilerSandbox`. Files outside the home directory (such as `/etc`) stay readable. |
| `env!("AWS_SECRET_ACCESS_KEY")` | Embeds a compiler environment variable. | The compiler runs with an **allowlisted environment**: `PATH`, `HOME`, temp directories, rustup/cargo homes, linker/SDK discovery variables and proxies. Other host variables are invisible. Add variables explicitly with `WithBuildEnv`. Covered by `TestBuildSecretsNotVisibleIntegration`. |
| `build.rs` scripts and proc-macros of dependencies | Arbitrary code execution on the host. | **Denied by default.** `DependencyPolicy` allows registry, git and path dependencies separately, with optional crate allowlists. Downloads need `Network: true`; otherwise builds are `--offline`. `Project` sources (which can have `build.rs`) require `WithTrust(Trusted)`, and their declared dependencies are checked against the policy. |
| Pathological code (exponential macro or type expansion) | Compiler hangs or exhausts memory. | Compile timeout (`WithCompileTimeout`, default 5 minutes). Cargo and all rustc children run in their own process group and are killed together. |
| Cargo configuration in ancestor directories or `$CARGO_HOME` | Changes flags, linkers or registries. | Config file contents are part of the cache key. `RUSTFLAGS` from the environment are ignored: FerroGo always sets `CARGO_ENCODED_RUSTFLAGS`, which overrides `build.rustflags`. |
| Malicious option values (`WithEdition("2021\"\n[dependencies]…")`) | TOML injection into the generated `Cargo.toml`. | Every value written to `Cargo.toml` is checked against an allowlist. No user string is interpolated into the manifest. |

All tool invocations pass arguments as an argv vector through `os/exec`.
**No shell is ever involved**, so arguments such as `$(rm -rf /)` are inert.
Covered by `TestRunArgumentsAreNotShellInterpreted` and
`TestRunStdinEnvArgsAndUnicode`.

## Threats at run time

| Backend | Isolation | Allowed by default |
| --- | --- | --- |
| WASM | Memory-safe sandbox. No filesystem, network or host environment unless granted (`WithMount`, `WithEnv`). Memory, time and output limits. | yes |
| Native process | OS process. Same user, same privileges, full filesystem and network. | **no**: requires `WithTrust(ferrogo.Trusted)` |
| Native library | None. Code runs inside your Go process, and calls cannot be interrupted. Unenforceable options (timeouts, memory limits, mounts, env) are rejected. | **no**: requires `WithTrust(ferrogo.Trusted)` |

`BackendAuto` always means WASM. FerroGo never falls back from WASM to a
native backend.

### WASM sandbox guarantees

Covered by `wasm_test.go`:

* Guests cannot see:
  * the host environment
  * the filesystem outside explicit mounts
  * paths that escape a mount through `..`
  * sockets (wasip1 has none)
  * threads
* Read-only mounts reject writes.
* Infinite loops, long sleeps and blocked stdin reads are interrupted at the
  deadline.
* Memory exhaustion stops at the guest limit (default 256 MiB) with
  `ErrMemoryLimit`. It never reaches the host.
* Output floods are cut off at `WithMaxOutput`.

### Native-process hardening (always on)

* **Environment:** empty by default. Grant variables with `WithEnv` or
  `WithInheritEnv`.
* **Working directory:** a fresh private temporary directory per run,
  removed afterwards, unless `WithDir` is set.
* **Process group:** on Unix, each run is its own process group. Timeouts,
  cancellation and output-limit violations send `SIGTERM` to the group, wait
  `WithGracePeriod` (default 2s), then send `SIGKILL` to the whole group, so
  forked children do not survive.
* **Limits:**
  * Execution timeout (`WithTimeout`, default 30s).
  * Output limit (`WithMaxOutput`, default 4 MiB combined). A program that
    exceeds it is terminated with `ErrOutputLimit`.
  * Stdin is the null device unless `WithStdin` is set.

### Optional OS sandbox for native processes: `WithSandbox`

`WithSandbox(ferrogo.Sandbox{…})` runs the program under the operating
system's sandbox:

* `sandbox-exec` (Seatbelt) on macOS;
* bubblewrap (`bwrap`) on Linux, with private mount, network, PID and IPC
  namespaces, and `--die-with-parent`.

Under the sandbox the program:

* has **no network**, unless `AllowNetwork` is set;
* **cannot write** outside its private working directory and the
  `Writable` directories;
* **cannot read file contents in the user's home directory**, except its
  own artifact and the `Readable` directories.

Where no sandbox is available (Windows, or Linux without a working
bubblewrap), `Run` fails with `ErrUnsupported`. It never runs unsandboxed.
`TestNativeSandbox` verifies each restriction against a control run.

### What native processes are *not* protected from

* **Without `WithSandbox`:** everything the user can do (files, network).
* **Even with `WithSandbox`:**
  * reading files outside the home directory that the user can read
  * CPU exhaustion until the timeout
* **Memory limits (`WithMemoryLimit`):**
  * **Linux:** `prlimit` sets `RLIMIT_AS` before the program starts. The
    limit is on virtual address space, so leave headroom for thread
    stacks.
  * **Windows:** a Job Object limit on the whole process tree.
  * **macOS:** returns `ErrUnsupported`, because macOS does not enforce
    address-space limits.
* **Process trees:**
  * **Unix:** killed as a process group.
  * **Windows:** killed through the Job Object (kill-on-close). A process
    created in the first instant of the program's life, before it is
    assigned to the job, could escape.
* **Windows:** no filesystem or network sandbox integration.

### Native function calls in a separate process

`Compile` on the native-process backend runs library code in worker
processes, not in your Go process. The trust model is the same as for
native programs: user privileges, unless `WithSandbox` is set. But a crash,
a runaway loop or memory exhaustion is contained in the worker. Prefer this
over the native-library backend whenever in-process speed is not essential.

### WASM interruption opt-out

`WithWASMInterruption(false)` removes the termination checks, which makes
call-heavy code about 12× faster. Running code then cannot be stopped by
timeouts or cancellation, so the option requires `WithTrust(Trusted)` and
rejects an explicit `WithTimeout`.

Compiled code is cached **separately** for each setting. wazero's cache key
does not include the setting, so a shared cache would let uninterruptible
code reach untrusted runs. This is covered by `TestWASMInterruptionOptOut`.

## Choosing settings

* **Code from end users or the network:** use the default trust policy
  (WASM only). Also compile inside a container or VM, because of
  `include_str!`.
* **Your own code or code you reviewed:** `WithTrust(ferrogo.Trusted)` with
  the native-process backend is appropriate.
* **Never** load native libraries built from untrusted source into your
  process.

## Reporting vulnerabilities

Please report security issues privately to the maintainers rather than in a
public issue.
