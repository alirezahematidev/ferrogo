package toolchain

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"1.94.1", Version{1, 94, 1, ""}, true},
		{"1.96.0-nightly", Version{1, 96, 0, "nightly"}, true},
		{"1.95.0-beta.3", Version{1, 95, 0, "beta.3"}, true},
		{"1.94", Version{}, false},
		{"one.two.three", Version{}, false},
		{"", Version{}, false},
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
	if !(Version{1, 81, 9, ""}).Less(MinVersion) || (Version{1, 82, 0, ""}).Less(MinVersion) {
		t.Error("MinVersion comparison is wrong")
	}
}

func TestParseRustcVerbose(t *testing.T) {
	out := `rustc 1.94.1 (e408947bf 2026-03-25)
binary: rustc
commit-hash: e408947bfabc
commit-date: 2026-03-25
host: aarch64-apple-darwin
release: 1.94.1
LLVM version: 21.1.2
`
	var tc Toolchain
	if err := tc.parseRustcVerbose(out); err != nil {
		t.Fatal(err)
	}
	if tc.Version != (Version{1, 94, 1, ""}) || tc.Host != "aarch64-apple-darwin" ||
		tc.CommitHash != "e408947bfabc" || tc.LLVMVersion != "21.1.2" {
		t.Errorf("parsed %+v", tc)
	}
	if err := tc.parseRustcVerbose("garbage"); err == nil {
		t.Error("expected error for garbage input")
	}
}

func TestNativeTarget(t *testing.T) {
	cases := []struct{ host, goos, goarch, want string }{
		{"aarch64-apple-darwin", "darwin", "arm64", "aarch64-apple-darwin"},
		{"aarch64-apple-darwin", "darwin", "amd64", "x86_64-apple-darwin"}, // Go under Rosetta
		{"x86_64-unknown-linux-musl", "linux", "amd64", "x86_64-unknown-linux-musl"},
		{"x86_64-unknown-linux-gnu", "linux", "arm64", "aarch64-unknown-linux-gnu"},
		{"x86_64-pc-windows-gnu", "windows", "amd64", "x86_64-pc-windows-gnu"},
		{"x86_64-pc-windows-msvc", "windows", "arm64", "aarch64-pc-windows-msvc"},
	}
	for _, c := range cases {
		tc := Toolchain{Host: c.host}
		got, err := tc.NativeTarget(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("NativeTarget(%s on %s/%s) = %q, %v; want %q", c.host, c.goos, c.goarch, got, err, c.want)
		}
	}
	tc := Toolchain{Host: "x86_64-unknown-linux-gnu"}
	if _, err := tc.NativeTarget("plan9", "amd64"); !errors.Is(err, errdefs.ErrUnsupported) {
		t.Errorf("unsupported GOOS: err = %v", err)
	}
}

func TestBaseEnvScrubsSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "do-not-leak")
	t.Setenv("RUSTFLAGS", "-C target-cpu=native")
	tc := Toolchain{cfg: Config{Toolchain: "stable", CargoHome: "/opt/cargo"}, Sysroot: "/sysroot"}
	env := strings.Join(tc.BaseEnv(map[string]string{"EXTRA": "1"}), "\n")
	for _, bad := range []string{"AWS_SECRET_ACCESS_KEY", "RUSTFLAGS="} {
		if strings.Contains(env, bad) {
			t.Errorf("environment contains %s", bad)
		}
	}
	for _, want := range []string{"RUSTUP_TOOLCHAIN=stable", "CARGO_HOME=/opt/cargo", "EXTRA=1", filepath.Join("/sysroot", "bin")} {
		if !strings.Contains(env, want) {
			t.Errorf("environment lacks %s", want)
		}
	}
}

func TestDetectMissingRustc(t *testing.T) {
	_, err := Detect(context.Background(), Config{Rustc: filepath.Join(t.TempDir(), "no-rustc")})
	if !errors.Is(err, errdefs.ErrToolchainNotFound) {
		t.Fatalf("err = %v, want ErrToolchainNotFound", err)
	}
}

// TestDetectInstalled runs against the real toolchain when one is available.
func TestDetectInstalled(t *testing.T) {
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	tc, err := Detect(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if tc.Host == "" || tc.Sysroot == "" || tc.CargoVersion == "" || !tc.HasTarget(tc.Host) {
		t.Errorf("incomplete detection: %+v", tc)
	}
	if !filepath.IsAbs(tc.Rustc) || !filepath.IsAbs(tc.Cargo) {
		t.Errorf("paths must be absolute: rustc=%q cargo=%q", tc.Rustc, tc.Cargo)
	}
	native, err := tc.NativeTarget(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tc.Sysroot, "lib", "rustlib", native)); err != nil && native == tc.Host {
		t.Errorf("native target %s not in sysroot: %v", native, err)
	}
}
