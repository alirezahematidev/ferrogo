package ferrogo

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
)

// modulePath is this module's import path.
const modulePath = "github.com/alirezahematidev/ferrogo"

// Version returns the version of the FerroGo module linked into the
// running binary, as recorded by the Go toolchain: the release tag (for
// example "v0.1.0") when FerroGo is a dependency or was installed with
// `go install ...@version`, a pseudo-version for untagged commits, or
// "(devel)" when the version is unknown.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	if info.Main.Path == modulePath && info.Main.Version != "" {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			if dep.Replace != nil && dep.Replace.Version != "" {
				return dep.Replace.Version
			}
			if dep.Version != "" {
				return dep.Version
			}
		}
	}
	return "(devel)"
}

// ToolchainInfo describes the detected Rust toolchain.
type ToolchainInfo struct {
	// Rustc and Cargo are the executables used for builds.
	Rustc string `json:"rustc"`
	Cargo string `json:"cargo"`
	// Rustup is the rustup executable, or empty if rustup is not installed.
	Rustup       string   `json:"rustup,omitempty"`
	Version      string   `json:"version"`
	CommitHash   string   `json:"commit_hash"`
	CommitDate   string   `json:"commit_date"`
	Host         string   `json:"host"`
	LLVMVersion  string   `json:"llvm_version"`
	CargoVersion string   `json:"cargo_version"`
	Sysroot      string   `json:"sysroot"`
	Targets      []string `json:"targets"`
}

// HasTarget reports whether the standard library for triple is installed.
func (t *ToolchainInfo) HasTarget(triple string) bool { return slices.Contains(t.Targets, triple) }

// Toolchain detects (once per Runtime) and describes the Rust toolchain.
func (r *Runtime) Toolchain(ctx context.Context) (*ToolchainInfo, error) {
	opCtx, done, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	comp, err := r.compiler(opCtx)
	if err != nil {
		return nil, r.closedErr(ctx, err)
	}
	tc := comp.Toolchain()
	return &ToolchainInfo{
		Rustc: tc.Rustc, Cargo: tc.Cargo, Rustup: tc.Rustup, Version: tc.Version.String(),
		CommitHash: tc.CommitHash, CommitDate: tc.CommitDate, Host: tc.Host, LLVMVersion: tc.LLVMVersion,
		CargoVersion: tc.CargoVersion, Sysroot: tc.Sysroot, Targets: slices.Clone(tc.Targets),
	}, nil
}

// CacheStats describes the artifact cache.
type CacheStats struct {
	// Enabled is false when the runtime was created with WithCache(false).
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"`
	Entries int    `json:"entries"`
	// Bytes is the total artifact size; MaxBytes is the eviction threshold (0 = unlimited).
	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
	// OtherBytes counts build directories and runtime compilation caches.
	OtherBytes int64 `json:"other_bytes"`
}

// CacheStats reports cache usage.
func (r *Runtime) CacheStats() (CacheStats, error) {
	s, err := r.cache.Stats()
	if err != nil {
		return CacheStats{}, fmt.Errorf("ferrogo: %w", err)
	}
	return CacheStats{Enabled: r.cfg.cache, Dir: s.Dir, Entries: s.Entries, Bytes: s.Bytes,
		MaxBytes: s.MaxBytes, OtherBytes: s.OtherBytes}, nil
}

// ClearCache deletes every cached artifact and build directory. Builds
// running concurrently in other processes that share the cache may fail.
func (r *Runtime) ClearCache() error {
	if err := r.cache.Clear(); err != nil {
		return fmt.Errorf("ferrogo: %w", err)
	}
	return nil
}

// BackendStatus reports whether a backend is usable right now.
type BackendStatus struct {
	Backend Backend `json:"backend"`
	// Target is the Rust target triple the backend compiles for.
	Target string `json:"target,omitempty"`
	// Usable reports whether Run or Call can work with the current
	// toolchain, trust policy and FerroGo version.
	Usable bool `json:"usable"`
	// Problems explains why the backend is not usable, with remediation.
	Problems     []string     `json:"problems,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// Report is a snapshot of the runtime's environment, returned by Inspect.
type Report struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	// DefaultBackend is the configured backend; BackendAuto resolves to WASM.
	DefaultBackend Backend    `json:"default_backend"`
	Trust          string     `json:"trust"`
	Cache          CacheStats `json:"cache"`
	// Toolchain is nil when detection failed; ToolchainError says why.
	Toolchain      *ToolchainInfo  `json:"toolchain,omitempty"`
	ToolchainError string          `json:"toolchain_error,omitempty"`
	Backends       []BackendStatus `json:"backends"`
}

// Inspect reports the toolchain, cache and the status of every backend.
// Problems are reported in the Report rather than as errors; the error is
// non-nil only if the runtime is closed or the cache is unreadable.
func (r *Runtime) Inspect(ctx context.Context) (*Report, error) {
	stats, err := r.CacheStats()
	if err != nil {
		return nil, err
	}
	rep := &Report{
		Version: Version(), GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		DefaultBackend: r.cfg.backend, Trust: r.cfg.trust.String(), Cache: stats,
	}
	info, tcErr := r.Toolchain(ctx)
	if tcErr != nil {
		if r.isClosed() {
			return nil, ErrClosed
		}
		rep.ToolchainError = tcErr.Error()
	}
	rep.Toolchain = info
	for _, b := range Backends {
		st := BackendStatus{Backend: b, Capabilities: CapabilitiesOf(b)}
		if !st.Capabilities.Run && !st.Capabilities.Call {
			st.Problems = append(st.Problems, "not implemented in this version of FerroGo (see docs/roadmap.md)")
		}
		if b.native() && r.cfg.trust != Trusted {
			st.Problems = append(st.Problems, "requires WithTrust(Trusted): runs code without a sandbox")
		}
		if tcErr != nil {
			st.Problems = append(st.Problems, "no usable Rust toolchain")
		} else {
			r.tcMu.Lock()
			tc := r.comp.Toolchain()
			r.tcMu.Unlock()
			triple, err := target(tc, b)
			switch {
			case err != nil:
				st.Problems = append(st.Problems, err.Error())
			case !tc.HasTarget(triple):
				st.Target = triple
				st.Problems = append(st.Problems, fmt.Sprintf("target %s not installed; fix: %s", triple, tc.TargetFix(triple)))
			default:
				st.Target = triple
			}
		}
		st.Usable = len(st.Problems) == 0
		rep.Backends = append(rep.Backends, st)
	}
	return rep, nil
}

func (r *Runtime) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}
