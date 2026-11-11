package ferrogo

import "time"

// Result is the outcome of Run.
type Result struct {
	// Stdout and Stderr hold the captured output (bounded by WithMaxOutput).
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// ExitCode is the exit status, or -1 if the program was killed by a signal.
	ExitCode int `json:"exit_code"`
	// Backend is the backend that executed the program (never BackendAuto).
	Backend Backend `json:"backend"`
	// Artifact describes the compiled program.
	Artifact *Artifact `json:"artifact"`
	// Duration is the execution wall time, excluding compilation.
	Duration time.Duration `json:"duration_ns"`
	Usage    ResourceUsage `json:"usage"`
}

// ResourceUsage reports resources consumed by an execution. Zero means the
// backend cannot measure the value.
type ResourceUsage struct {
	UserCPU         time.Duration `json:"user_cpu_ns"`
	SystemCPU       time.Duration `json:"system_cpu_ns"`
	PeakMemoryBytes uint64        `json:"peak_memory_bytes"`
}

// ArtifactKind is the kind of compiled artifact.
type ArtifactKind string

const (
	// ArtifactProgram is an executable with fn main.
	ArtifactProgram ArtifactKind = "program"
	// ArtifactLibrary is a library exposing exported functions
	// (a cdylib: .wasm, .so, .dylib or .dll).
	ArtifactLibrary ArtifactKind = "library"
)

// Artifact is a compiled program or library.
type Artifact struct {
	// Path is the artifact file. It is read-only and lives in the cache (or,
	// with caching disabled, in a temporary directory removed by
	// Runtime.Close). Copy it if you need it elsewhere.
	Path    string       `json:"path"`
	Kind    ArtifactKind `json:"kind"`
	Backend Backend      `json:"backend"`
	Target  string       `json:"target"`
	// Key is the SHA-256 cache key of the build inputs.
	Key  string `json:"key"`
	Size int64  `json:"size"`
	// Cached reports whether the artifact was reused instead of built.
	Cached bool `json:"cached"`
	// BuildDuration is the compile time (zero when Cached).
	BuildDuration time.Duration `json:"build_duration_ns"`
	// Warnings are the compiler warnings of the build.
	Warnings []Diagnostic `json:"warnings,omitempty"`
}
