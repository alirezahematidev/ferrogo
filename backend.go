package ferrogo

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/alirezahematidev/ferrogo/internal/backend/dylib"
)

// Backend selects how compiled Rust code is executed.
type Backend string

// Execution backends.
const (
	// BackendAuto selects the safest backend that supports the operation.
	// Today that is always BackendWASM; Auto never resolves to a native
	// backend and never falls back silently.
	BackendAuto Backend = "auto"
	// BackendWASM runs code in the wazero WebAssembly sandbox (wasm32-wasip1).
	BackendWASM Backend = "wasm"
	// BackendWazy runs code in the wazy WebAssembly sandbox, a wazero fork
	// whose compiled code is always interruptible. Same sandbox and trust
	// rules as BackendWASM; WithWASMInterruption(false) is unsupported.
	BackendWazy Backend = "wazy"
	// BackendNativeProcess runs native executables as child processes.
	// Not a sandbox; requires WithTrust(Trusted).
	BackendNativeProcess Backend = "native-process"
	// BackendNativeLibrary loads native shared libraries into the Go process.
	// No isolation at all; requires WithTrust(Trusted).
	BackendNativeLibrary Backend = "native-library"
)

// Backends lists the concrete backends.
var Backends = []Backend{BackendWASM, BackendWazy, BackendNativeProcess, BackendNativeLibrary}

// ParseBackend parses a backend name. It accepts "auto", "wasm", "wazy",
// "native-process" (alias "native", "process"), and "native-library"
// (aliases "library", "shared", "dylib").
func ParseBackend(s string) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto", "":
		return BackendAuto, nil
	case "wasm", "webassembly":
		return BackendWASM, nil
	case "wazy":
		return BackendWazy, nil
	case "native-process", "native", "process":
		return BackendNativeProcess, nil
	case "native-library", "library", "shared", "dylib":
		return BackendNativeLibrary, nil
	}
	return "", fmt.Errorf("ferrogo: unknown backend %q (want auto, wasm, wazy, native-process or native-library)", s)
}

func (b Backend) valid() bool {
	switch b {
	case BackendAuto, BackendWASM, BackendWazy, BackendNativeProcess, BackendNativeLibrary:
		return true
	}
	return false
}

// isWASM reports whether b runs wasm32-wasip1 code in a WebAssembly engine.
func (b Backend) isWASM() bool { return b == BackendWASM || b == BackendWazy }

// artifactBackend is the backend artifacts are built and cached for: both
// WASM engines run the same wasm32-wasip1 file.
func (b Backend) artifactBackend() Backend {
	if b.isWASM() {
		return BackendWASM
	}
	return b
}

// native reports whether b executes native machine code outside a sandbox.
func (b Backend) native() bool {
	return b == BackendNativeProcess || b == BackendNativeLibrary
}

// TrustLevel declares how much the host trusts the Rust code it runs.
type TrustLevel int

const (
	// Untrusted (the default) allows only sandboxed backends (WASM).
	Untrusted TrustLevel = iota
	// Trusted additionally allows native backends, which run code with the
	// privileges of the host user (native process) or the host process
	// itself (native library).
	Trusted
)

func (t TrustLevel) String() string {
	switch t {
	case Untrusted:
		return "untrusted"
	case Trusted:
		return "trusted"
	}
	return fmt.Sprintf("TrustLevel(%d)", int(t))
}

// ParseTrustLevel parses "untrusted" or "trusted".
func ParseTrustLevel(s string) (TrustLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "untrusted", "":
		return Untrusted, nil
	case "trusted":
		return Trusted, nil
	}
	return 0, fmt.Errorf("ferrogo: unknown trust level %q (want untrusted or trusted)", s)
}

// Capabilities describes what a backend can do in this build of FerroGo on
// this platform. Use it instead of assuming every feature works everywhere.
type Capabilities struct {
	Backend Backend `json:"backend"`
	// Run: can execute programs with fn main.
	Run bool `json:"run"`
	// Call: can load modules and call exported functions.
	Call bool `json:"call"`
	// CaptureOutput, Stdin, Env, Args, WorkingDir: program I/O and environment control.
	CaptureOutput bool `json:"capture_output"`
	Stdin         bool `json:"stdin"`
	Env           bool `json:"env"`
	Args          bool `json:"args"`
	WorkingDir    bool `json:"working_dir"`
	// Interruptible: running code can be stopped by timeouts and cancellation.
	Interruptible bool `json:"interruptible"`
	// MemoryLimit: WithMemoryLimit is enforced.
	MemoryLimit bool `json:"memory_limit"`
	// FilesystemIsolation: code sees only explicitly granted directories.
	FilesystemIsolation bool `json:"filesystem_isolation"`
	// NetworkIsolation: code cannot open network connections.
	NetworkIsolation bool `json:"network_isolation"`
	// Threads: code may spawn OS threads.
	Threads bool `json:"threads"`
	// ProcessIsolation: code runs outside the Go process.
	ProcessIsolation bool `json:"process_isolation"`
	// Sandboxed: the backend is a security boundary for untrusted code.
	Sandboxed bool `json:"sandboxed"`
	// RequiresTrust: the backend is only allowed with WithTrust(Trusted).
	RequiresTrust bool `json:"requires_trust"`
	// OSSandbox: WithSandbox is implemented on this platform (it still needs
	// sandbox-exec or bubblewrap at run time).
	OSSandbox bool `json:"os_sandbox"`
}

// CapabilitiesOf reports the capabilities of b as implemented in this
// version of FerroGo. BackendAuto reports the capabilities of the backend it
// resolves to.
func CapabilitiesOf(b Backend) Capabilities {
	switch b {
	case BackendAuto:
		c := CapabilitiesOf(BackendWASM)
		c.Backend = BackendAuto
		return c
	case BackendNativeProcess:
		return Capabilities{
			Backend: b, Run: true, Call: true, CaptureOutput: true, Stdin: true, Env: true, Args: true,
			WorkingDir: true, Interruptible: true, Threads: true, ProcessIsolation: true,
			RequiresTrust: true, OSSandbox: runtime.GOOS == "darwin" || runtime.GOOS == "linux",
			// Linux: prlimit (address space); Windows: Job Object. macOS does
			// not enforce address-space limits.
			MemoryLimit: runtime.GOOS == "linux" || runtime.GOOS == "windows",
		}
	case BackendNativeLibrary:
		return Capabilities{Backend: b, Call: dylib.Supported, Threads: true, RequiresTrust: true}
	case BackendWASM, BackendWazy:
		return Capabilities{
			Backend: b, Run: true, Call: true, CaptureOutput: true, Stdin: true, Env: true, Args: true,
			Interruptible: true, MemoryLimit: true, FilesystemIsolation: true,
			NetworkIsolation: true, Sandboxed: true,
		}
	}
	return Capabilities{Backend: b}
}
