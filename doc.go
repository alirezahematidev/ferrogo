// Package ferrogo compiles Rust source code from Go and runs it: as a
// program, or as a library whose functions Go calls repeatedly.
//
// A Runtime drives the user's Rust toolchain (rustc and cargo), caches
// compiled artifacts by a SHA-256 fingerprint of every build input, and
// executes them on one of four backends:
//
//   - BackendWASM runs code in the wazero WebAssembly sandbox. It is the
//     default (BackendAuto) and, with BackendWazy, the only backend allowed for untrusted code:
//     guests see no files, network or environment unless granted, and
//     memory, time and output are limited.
//   - BackendWazy is the same sandbox on the wazy engine (a wazero fork that
//     is always interruptible); opt in with WithBackend(BackendWazy).
//   - BackendNativeProcess runs native executables, and serves library calls
//     from worker processes, optionally under the OS sandbox (WithSandbox).
//   - BackendNativeLibrary loads a native shared library into the Go process
//     for the fastest calls, with no isolation at all.
//
// Native backends require WithTrust(Trusted). CapabilitiesOf reports what
// each backend supports; Runtime.Inspect reports what is usable on this
// machine. FerroGo never falls back silently from WASM to a native backend,
// and never silently ignores a limit it cannot enforce.
//
// # Running a program
//
//	rt, err := ferrogo.New()
//	if err != nil {
//		return err
//	}
//	defer rt.Close()
//
//	res, err := rt.Run(ctx, ferrogo.Code(`fn main() { println!("Hello from Rust!"); }`))
//	if err != nil {
//		return err
//	}
//	fmt.Print(res.Stdout)
//
// # Calling Rust functions
//
// Compile builds a library and keeps it loaded. Functions are exported with
// the ferrogo::export! macro, which FerroGo makes available in every library:
//
//	mod, err := rt.Compile(ctx, ferrogo.Code(`
//	    ferrogo::export! {
//	        fn add(a: i32, b: i32) -> i32 { a + b }
//	    }
//	`))
//	sum, err := mod.Call(ctx, "add", ferrogo.Int32(10), ferrogo.Int32(20))
//
// Values cross the boundary through one tagged wire format on every backend:
// bool, integers, floats, String, Vec<u8>, JSON (for structs) and
// Result<T, E>. Rust can call back into Go through ferrogo::import! and
// WithHostFunction (WASM).
//
// # Inline Rust
//
// Rust written in /*ferrogo ... */ comments in .go files is compiled by
// `ferrogo generate` (run through go generate) into typed Go wrappers with
// the WebAssembly embedded, so programs using the package need no Rust
// toolchain. See docs/inline-rust.md in the repository.
//
// # Dependencies and security
//
// Cargo dependencies (WithDependencies) and Cargo projects (Project) are
// denied unless a DependencyPolicy allows them, because their build scripts
// run on the build machine. WithCompilerSandbox runs the toolchain itself
// under the OS sandbox. See docs/security.md for the full model.
//
// # Errors
//
// Failures are reported with sentinel errors (ErrCompilation, ErrTimeout,
// ErrUntrusted, ...) and typed errors that carry details (*CompileError with
// rustc diagnostics mapped to your file names, *ExitError, *PanicError,
// *CallError, *TargetError, *UnsupportedError). Match them with errors.Is
// and errors.As.
//
// FerroGo does not perform machine-code JIT compilation: code is compiled
// ahead of time by rustc and then executed or loaded.
package ferrogo
