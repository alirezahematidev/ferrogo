// Package toolchain discovers the Rust toolchain (rustc, cargo, sysroot and
// installed targets) and builds the scrubbed environment used to run it.
package toolchain

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/process"
)

// MinVersion is the oldest supported rustc release. 1.82 is the first release
// that accepts `#[unsafe(...)]` attribute syntax in every edition, which the
// FerroGo ABI helper relies on.
var MinVersion = Version{Major: 1, Minor: 82}

// Config selects a toolchain explicitly. Zero values mean auto-detection.
type Config struct {
	// Rustc is the rustc executable. Default: "rustc" from PATH.
	Rustc string
	// Cargo is the cargo executable. Default: the cargo next to the
	// detected sysroot, falling back to "cargo" from PATH.
	Cargo string
	// Toolchain is a rustup toolchain name ("stable", "1.94.1", ...), passed
	// as RUSTUP_TOOLCHAIN.
	Toolchain string
	// RustupHome and CargoHome override RUSTUP_HOME and CARGO_HOME.
	RustupHome, CargoHome string
}

// Version is a parsed rustc release.
type Version struct {
	Major, Minor, Patch int
	// Pre is the pre-release suffix, e.g. "nightly" or "beta.3".
	Pre string
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Less reports whether v is older than o, ignoring pre-release suffixes.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// Toolchain is a detected Rust toolchain.
type Toolchain struct {
	// Rustc and Cargo are the executables used for builds. When the sysroot
	// contains them they are the real binaries, bypassing rustup proxies, so
	// every build uses exactly the toolchain that was detected.
	Rustc, Cargo string
	// Rustup is the rustup executable, or empty if rustup is not installed.
	Rustup string

	Version     Version
	CommitHash  string
	CommitDate  string
	Host        string
	LLVMVersion string
	// RustcVerbose is the complete `rustc -vV` output (part of cache keys).
	RustcVerbose string
	// CargoVersion is the complete `cargo -V` output (part of cache keys).
	CargoVersion string
	Sysroot      string
	// Targets lists the target triples with an installed standard library.
	Targets []string

	cfg Config
}

// Detect locates and validates a Rust toolchain.
func Detect(ctx context.Context, cfg Config) (*Toolchain, error) {
	rustc, err := lookExecutable(cfg.Rustc, "rustc")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errdefs.ErrToolchainNotFound, err)
	}
	t := &Toolchain{cfg: cfg}
	base := t.BaseEnv(nil)

	verbose, err := capture(ctx, rustc, base, "-vV")
	if err != nil {
		return nil, fmt.Errorf("%w: running %s -vV: %v", errdefs.ErrToolchainNotFound, rustc, err)
	}
	if err := t.parseRustcVerbose(verbose); err != nil {
		return nil, fmt.Errorf("%w: %v", errdefs.ErrToolchainNotFound, err)
	}
	if t.Version.Less(MinVersion) {
		return nil, fmt.Errorf("%w: rustc %s is older than the minimum supported %s; run `rustup update stable`",
			errdefs.ErrToolchainNotFound, t.Version, MinVersion)
	}

	sysroot, err := capture(ctx, rustc, base, "--print", "sysroot")
	if err != nil {
		return nil, fmt.Errorf("%w: running %s --print sysroot: %v", errdefs.ErrToolchainNotFound, rustc, err)
	}
	t.Sysroot = strings.TrimSpace(sysroot)

	// Prefer the real binaries inside the sysroot over rustup proxies.
	t.Rustc = rustc
	if real := filepath.Join(t.Sysroot, "bin", exeName("rustc")); isFile(real) {
		t.Rustc = real
	}
	switch {
	case cfg.Cargo != "":
		if t.Cargo, err = lookExecutable(cfg.Cargo, ""); err != nil {
			return nil, fmt.Errorf("%w: %v", errdefs.ErrToolchainNotFound, err)
		}
	case isFile(filepath.Join(t.Sysroot, "bin", exeName("cargo"))):
		t.Cargo = filepath.Join(t.Sysroot, "bin", exeName("cargo"))
	default:
		if t.Cargo, err = lookExecutable("", "cargo"); err != nil {
			return nil, fmt.Errorf("%w: cargo not found: %v", errdefs.ErrToolchainNotFound, err)
		}
	}
	cargoVersion, err := capture(ctx, t.Cargo, t.BaseEnv(nil), "-V")
	if err != nil {
		return nil, fmt.Errorf("%w: running %s -V: %v", errdefs.ErrToolchainNotFound, t.Cargo, err)
	}
	t.CargoVersion = strings.TrimSpace(cargoVersion)

	if t.Targets, err = installedTargets(t.Sysroot); err != nil {
		return nil, fmt.Errorf("%w: listing targets in %s: %v", errdefs.ErrToolchainNotFound, t.Sysroot, err)
	}
	if path, err := exec.LookPath("rustup"); err == nil {
		t.Rustup = path
	}
	return t, nil
}

// HasTarget reports whether the standard library for triple is installed.
func (t *Toolchain) HasTarget(triple string) bool {
	return slices.Contains(t.Targets, triple)
}

// TargetFix returns a human-readable remediation for a missing target.
func (t *Toolchain) TargetFix(triple string) string {
	if t.Rustup == "" {
		return fmt.Sprintf("install the standard library for %s into %s (rustup not found)", triple, t.Sysroot)
	}
	if t.cfg.Toolchain != "" {
		return fmt.Sprintf("rustup target add --toolchain %s %s", t.cfg.Toolchain, triple)
	}
	return "rustup target add " + triple
}

func (t *Toolchain) parseRustcVerbose(out string) error {
	t.RustcVerbose = strings.TrimSpace(out)
	sc := bufio.NewScanner(strings.NewReader(out))
	var release string
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "release":
			release = value
		case "commit-hash":
			t.CommitHash = value
		case "commit-date":
			t.CommitDate = value
		case "host":
			t.Host = value
		case "LLVM version":
			t.LLVMVersion = value
		}
	}
	if release == "" || t.Host == "" {
		return fmt.Errorf("unrecognised `rustc -vV` output: %q", out)
	}
	v, err := ParseVersion(release)
	if err != nil {
		return err
	}
	t.Version = v
	return nil
}

// ParseVersion parses a rustc release string such as "1.94.1" or "1.96.0-nightly".
func ParseVersion(s string) (Version, error) {
	core, pre, _ := strings.Cut(strings.TrimSpace(s), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid rustc version %q", s)
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("invalid rustc version %q", s)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// installedTargets lists target triples whose standard library is present in
// the sysroot. This works with and without rustup.
func installedTargets(sysroot string) ([]string, error) {
	root := filepath.Join(sysroot, "lib", "rustlib")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "etc" || e.Name() == "src" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(root, e.Name(), "lib")); err == nil && fi.IsDir() {
			targets = append(targets, e.Name())
		}
	}
	slices.Sort(targets)
	return targets, nil
}

// NativeTarget returns the Rust target triple for code that must run inside
// (or next to) a Go process built for goos/goarch. When the toolchain's host
// has the same architecture and OS, the host triple is used so its ABI
// variant (gnu, musl, msvc) is preserved.
func (t *Toolchain) NativeTarget(goos, goarch string) (string, error) {
	arch, ok := map[string]string{
		"amd64": "x86_64", "arm64": "aarch64", "386": "i686",
		"riscv64": "riscv64gc", "ppc64le": "powerpc64le", "s390x": "s390x", "loong64": "loongarch64",
	}[goarch]
	if !ok {
		return "", fmt.Errorf("%w: no Rust target mapping for GOARCH=%s", errdefs.ErrUnsupported, goarch)
	}
	osSuffix, ok := map[string]string{
		"darwin": "apple-darwin", "linux": "unknown-linux-gnu", "windows": "pc-windows-msvc",
		"freebsd": "unknown-freebsd", "netbsd": "unknown-netbsd", "illumos": "unknown-illumos",
	}[goos]
	if !ok {
		return "", fmt.Errorf("%w: no Rust target mapping for GOOS=%s", errdefs.ErrUnsupported, goos)
	}
	hostArch, _, _ := strings.Cut(t.Host, "-")
	if hostArch == arch && hostMatchesOS(t.Host, goos) {
		return t.Host, nil
	}
	return arch + "-" + osSuffix, nil
}

func hostMatchesOS(host, goos string) bool {
	switch goos {
	case "darwin":
		return strings.Contains(host, "-apple-darwin")
	case "linux":
		return strings.Contains(host, "-linux-")
	case "windows":
		return strings.Contains(host, "-windows-")
	default:
		return strings.Contains(host, goos)
	}
}

// BaseEnv returns the scrubbed environment for toolchain processes: an
// allowlist of host variables needed to locate tools, linkers and SDKs, plus
// the configured rustup/cargo settings, plus extra (which wins). Secrets in
// the host environment are therefore invisible to `env!()` at compile time.
func (t *Toolchain) BaseEnv(extra map[string]string) []string {
	env := map[string]string{}
	allowed := allowedEnvUnix
	if runtime.GOOS == "windows" {
		allowed = allowedEnvWindows
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if isAllowed(k, allowed) {
			env[k] = v
		}
	}
	if t.cfg.Toolchain != "" {
		env["RUSTUP_TOOLCHAIN"] = t.cfg.Toolchain
	}
	if t.cfg.RustupHome != "" {
		env["RUSTUP_HOME"] = t.cfg.RustupHome
	}
	if t.cfg.CargoHome != "" {
		env["CARGO_HOME"] = t.cfg.CargoHome
	}
	// Make the detected toolchain's binaries win for build scripts that
	// invoke `rustc` or `cargo` by name.
	if t.Sysroot != "" {
		pathKey := "PATH"
		for k := range env {
			if strings.EqualFold(k, "PATH") {
				pathKey = k
			}
		}
		bin := filepath.Join(t.Sysroot, "bin")
		if cur := env[pathKey]; cur != "" {
			env[pathKey] = bin + string(os.PathListSeparator) + cur
		} else {
			env[pathKey] = bin
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// CargoHome returns the effective CARGO_HOME directory.
func (t *Toolchain) CargoHome() string {
	if t.cfg.CargoHome != "" {
		return t.cfg.CargoHome
	}
	if v := os.Getenv("CARGO_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cargo")
}

var allowedEnvUnix = []string{
	"PATH", "HOME", "USER", "LOGNAME", "TMPDIR",
	"RUSTUP_HOME", "CARGO_HOME", "RUSTUP_TOOLCHAIN",
	// Linker and SDK discovery.
	"CC", "CXX", "AR", "SDKROOT", "DEVELOPER_DIR", "MACOSX_DEPLOYMENT_TARGET",
	// Network settings; only used when the dependency policy allows fetching.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

var allowedEnvWindows = []string{
	"PATH", "PATHEXT", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC",
	"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
	"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432",
	"COMMONPROGRAMFILES", "COMMONPROGRAMFILES(X86)",
	"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS",
	"RUSTUP_HOME", "CARGO_HOME", "RUSTUP_TOOLCHAIN",
	// MSVC linker discovery (Developer Command Prompt variables).
	"INCLUDE", "LIB", "LIBPATH", "VSINSTALLDIR", "VCINSTALLDIR", "VCTOOLSINSTALLDIR",
	"WINDOWSSDKDIR", "WINDOWSSDKVERSION", "UNIVERSALCRTSDKDIR", "UCRTVERSION",
	"CC", "CXX", "AR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
}

func isAllowed(key string, allowed []string) bool {
	if runtime.GOOS == "windows" {
		return slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, key) })
	}
	return slices.Contains(allowed, key)
}

func capture(ctx context.Context, path string, env []string, args ...string) (string, error) {
	res, err := process.Run(ctx, process.Spec{
		Path: path, Args: args, Env: env,
		Timeout: 60 * time.Second, MaxOutput: 1 << 20,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit status %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return string(res.Stdout), nil
}

// lookExecutable resolves an explicitly configured path, or a default name on PATH.
func lookExecutable(configured, name string) (string, error) {
	target := configured
	if target == "" {
		target = name
	}
	path, err := exec.LookPath(target)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) && configured == "" {
			return "", fmt.Errorf("%s not found on PATH; install Rust from https://rustup.rs", name)
		}
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
