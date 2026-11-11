// Package backend defines the contract between the public ferrogo package and
// its execution backends. Policy (trust, capability checks, option
// validation) is enforced by the caller; backends only execute.
package backend

import (
	"context"
	"io"
	"time"
)

// RunSpec describes one program execution.
type RunSpec struct {
	// Artifact is the path of the compiled program.
	Artifact string
	Args     []string
	// Env is the complete environment as KEY=VALUE pairs.
	Env []string
	// Dir is the working directory. Empty means a private temporary directory.
	Dir            string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Timeout        time.Duration
	GracePeriod    time.Duration
	// MaxOutput bounds combined stdout+stderr bytes. Zero means unlimited.
	MaxOutput int64
	// MemoryLimit bounds guest memory in bytes. Zero means the backend default.
	MemoryLimit int64
	// Mounts grants access to host directories (sandboxed backends only).
	Mounts []Mount
	// Uninterruptible disables WASM interruption checks (faster; timeouts
	// and cancellation can no longer stop running guest code).
	Uninterruptible bool
	// Sandbox, when set, runs a native process under OS isolation.
	Sandbox *Sandbox
	// Host dispatches ferrogo::import! calls (WASM only).
	Host HostDispatcher
	// Deterministic gives WASM code fake clocks and randomness seeded with Seed.
	Deterministic bool
	Seed          uint64
}

// Sandbox is an OS-level isolation policy for native processes.
type Sandbox struct {
	AllowNetwork bool
	// Writable directories (besides the private working directory).
	Writable []string
	// Readable directories inside the home directory, which is otherwise hidden.
	Readable []string
}

// Mount exposes a host directory to sandboxed guest code.
type Mount struct {
	HostPath  string
	GuestPath string
	ReadOnly  bool
}

// RunResult describes a finished execution.
type RunResult struct {
	Stdout, Stderr []byte
	ExitCode       int
	Signal         string
	Duration       time.Duration
	UserTime       time.Duration
	SystemTime     time.Duration
	PeakMemory     uint64
	// Trap describes a WebAssembly trap (e.g. "unreachable") that ended
	// execution, or is empty.
	Trap string
}

// Runner executes compiled programs.
type Runner interface {
	// Run executes spec. It returns a nil result only if execution never
	// started. Limit violations return the partial result together with an
	// error matching the corresponding errdefs sentinel.
	Run(ctx context.Context, spec RunSpec) (*RunResult, error)
	// Close releases backend resources.
	Close() error
}

// ModuleSpec configures a loaded module.
type ModuleSpec struct {
	// Env is the environment visible to the module (WASM only).
	Env []string
	// Mounts grants directory access (WASM only).
	Mounts []Mount
	// MemoryLimit bounds guest memory per instance (WASM only).
	MemoryLimit int64
	// CallTimeout bounds each call (WASM only; 0 = none).
	CallTimeout time.Duration
	// MaxPayload bounds result sizes in bytes.
	MaxPayload int
	// Uninterruptible disables WASM interruption checks (see RunSpec).
	Uninterruptible bool
	// Host dispatches calls from ferrogo::import! stubs (WASM only). The
	// arguments and result use the wire format; nil means no host functions.
	Host HostDispatcher
	// Deterministic gives WASM code fake clocks and randomness seeded with Seed.
	Deterministic bool
	Seed          uint64
}

// HostDispatcher handles a host-function call by name. args is an encoded
// argument list; it returns an encoded result value.
type HostDispatcher func(ctx context.Context, name string, args []byte) ([]byte, error)

// Module is a loaded library exposing FerroGo ABI functions. Implementations
// are safe for concurrent use.
type Module interface {
	// Call invokes ferrogo_v1_call_<name> with encoded arguments and returns
	// the call status and the encoded result or message. A guest panic is
	// reported as StatusPanic with "file:line:col\x1fmessage", on every
	// backend. err is non-nil only for host-side failures (missing
	// function, timeout, trap without panic, closed module).
	Call(ctx context.Context, name string, input []byte) (status uint32, output []byte, err error)
	// Functions lists the envelope-convention functions the module exports.
	Functions() []string
	// Signature returns the Rust signature text of an exported function.
	Signature(ctx context.Context, name string) (string, error)
	// Close releases the module. In-flight calls finish first.
	Close() error
}

// RawType is a WebAssembly core value type.
type RawType byte

// Core value types.
const (
	RawI32 RawType = 0x7f
	RawI64 RawType = 0x7e
	RawF32 RawType = 0x7d
	RawF64 RawType = 0x7c
)

// RawCaller is implemented by modules that can call plain
// `#[no_mangle] extern "C"` functions whose signature they can introspect.
type RawCaller interface {
	// RawSignature reports the parameter and result types of an export.
	RawSignature(name string) (params, results []RawType, ok bool)
	// CallRaw calls an export with core values (bit patterns).
	CallRaw(ctx context.Context, name string, args []uint64) ([]uint64, error)
}
