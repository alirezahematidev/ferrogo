package ferrogo

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/compiler"
)

func TestParsePanic(t *testing.T) {
	stderr := "some output\nthread 'main' panicked at src/main.rs:3:5:\nindex out of bounds\nsecond line\nnote: run with `RUST_BACKTRACE=1` environment variable to display a backtrace\n"
	p := parsePanic(stderr, snippetLocator([]compiler.SourceFile{{Path: "main.rs", DisplayName: "calc.rs", LineOffset: 10}}))
	if p == nil {
		t.Fatal("panic not detected")
	}
	if p.Thread != "main" || p.File != "calc.rs" || p.Line != 13 || p.Column != 5 {
		t.Errorf("location = %+v", p)
	}
	if p.Message != "index out of bounds\nsecond line" {
		t.Errorf("message = %q", p.Message)
	}
	p = parsePanic("thread 'main' (30464079) panicked at src/main.rs:3:14:\nboom\n", snippetLocator([]compiler.SourceFile{{Path: "main.rs", DisplayName: "main.rs"}}))
	if p == nil || p.Thread != "main" || p.Line != 3 || p.Column != 14 || p.Message != "boom" {
		t.Errorf("thread-id format = %+v", p)
	}
	// Windows prints panic locations with backslashes.
	p = parsePanic("thread 'main' (1) panicked at src\\main.rs:4:14:\nboom\n", snippetLocator([]compiler.SourceFile{{Path: "main.rs", DisplayName: "prog.rs"}}))
	if p == nil || p.File != "prog.rs" || p.Line != 4 {
		t.Errorf("Windows panic path = %+v", p)
	}
	if got, _, _ := projectLocator("proj")(`src\main.rs`, 1, 1); got != "proj/src/main.rs" {
		t.Errorf("Windows project path = %q", got)
	}
	if parsePanic("thread 'main' did not panic", nil) != nil {
		t.Error("false positive")
	}
	p = parsePanic("thread '<unnamed>' panicked at /rustc/abc/library/core/src/x.rs:1:2:\nboom", nil)
	if p == nil || p.Thread != "<unnamed>" || p.File != "/rustc/abc/library/core/src/x.rs" || p.Message != "boom" {
		t.Errorf("std-location panic = %+v", p)
	}
}

func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`version = 1
backend = "native"
trust = "trusted"
cache_dir = "cache"
[toolchain]
toolchain = "stable"
rustc = "bin/rustc"
[compiler]
edition = "2024"
profile = "debug"
opt_level = "s"
rustflags = ["-C", "debug-assertions=on"]
timeout = "90s"
env = { FOO = "bar" }
[execution]
timeout = "10s"
max_output_size = 1048576
inherit_env = ["LANG"]
`)
	cfg := defaultConfig()
	if err := WithConfigFile(path).applyRuntime(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.backend != BackendNativeProcess || cfg.trust != Trusted || cfg.edition != "2024" ||
		cfg.profile != ProfileDebug || cfg.optLevel != "s" || cfg.compileTimeout != 90*time.Second ||
		cfg.timeout != 10*time.Second || cfg.maxOutput != 1<<20 || cfg.buildEnv["FOO"] != "bar" ||
		len(cfg.rustFlags) != 2 || cfg.inheritEnv[0] != "LANG" || cfg.toolchain.Toolchain != "stable" {
		t.Errorf("config not applied: %+v", cfg)
	}
	if cfg.cacheDir != filepath.Join(dir, "cache") || cfg.toolchain.Rustc != filepath.Join(dir, "bin/rustc") {
		t.Errorf("relative paths not resolved: cache=%q rustc=%q", cfg.cacheDir, cfg.toolchain.Rustc)
	}

	bad := map[string]string{
		"unknown key":     "version = 1\nbakend = \"wasm\"\n",
		"unknown section": "version = 1\n[wasmm]\nmemory_limit = 1\n",
		"missing version": "backend = \"wasm\"\n",
		"future version":  "version = 2\n",
		"bad backend":     "version = 1\nbackend = \"jvm\"\n",
		"bad duration":    "version = 1\n[execution]\ntimeout = \"soon\"\n",
		"bad edition":     "version = 1\n[compiler]\nedition = \"2022\"\n",
		"negative limit":  "version = 1\n[execution]\nmax_output_size = -1\n",
		"syntax":          "version = 1\nbackend = \n",
	}
	for name, content := range bad {
		write(content)
		cfg := defaultConfig()
		if err := WithConfigFile(path).applyRuntime(&cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigTemplateIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(ConfigTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	if err := WithConfigFile(path).applyRuntime(&cfg); err != nil {
		t.Fatalf("template rejected: %v", err)
	}
}

func TestOptionValidation(t *testing.T) {
	bad := []Option{
		WithBackend("jvm"), WithTrust(TrustLevel(7)), WithCacheDir(""), WithCacheMaxBytes(-1),
		WithMaxConcurrentBuilds(0), WithLogger(nil), WithEdition("2020"), WithProfile("Fast!"),
		WithOptLevel("4"), WithRustFlags("a\x1fb"), WithBuildEnv(map[string]string{"A=B": "c"}),
		WithCompileTimeout(-1), WithTimeout(-1), WithMaxOutput(-1), WithMemoryLimit(-1),
		WithEnv(map[string]string{"": "x"}), WithInheritEnv("A=B"), WithGracePeriod(0),
	}
	for i, o := range bad {
		cfg := defaultConfig()
		if err := o.applyRuntime(&cfg); err == nil {
			t.Errorf("option %d accepted an invalid value", i)
		}
	}
	cfg := defaultConfig()
	if err := WithArgs("a\x00").applyRun(&cfg); err == nil {
		t.Error("NUL argument accepted")
	}
}

func TestCloneIsDeep(t *testing.T) {
	cfg := defaultConfig()
	cfg.env = map[string]string{"A": "1"}
	cfg.rustFlags = []string{"x"}
	cp := cfg.clone()
	cp.env["A"] = "2"
	cp.rustFlags[0] = "y"
	if cfg.env["A"] != "1" || cfg.rustFlags[0] != "x" {
		t.Error("per-call options leaked into runtime defaults")
	}
}

func TestBackendParsingAndCapabilities(t *testing.T) {
	for in, want := range map[string]Backend{"": BackendAuto, "WASM": BackendWASM, "Wazy": BackendWazy, "native": BackendNativeProcess, "dylib": BackendNativeLibrary} {
		if got, err := ParseBackend(in); err != nil || got != want {
			t.Errorf("ParseBackend(%q) = %q, %v", in, got, err)
		}
	}
	auto, wasm := CapabilitiesOf(BackendAuto), CapabilitiesOf(BackendWASM)
	auto.Backend = wasm.Backend
	if auto != wasm {
		t.Error("auto must report the capabilities of the backend it resolves to")
	}
	for _, b := range []Backend{BackendNativeProcess, BackendNativeLibrary} {
		c := CapabilitiesOf(b)
		if !c.RequiresTrust || c.Sandboxed {
			t.Errorf("%s must require trust and must not claim to be sandboxed", b)
		}
	}
	if v := Version(); v == "" {
		t.Error("empty version")
	}
}
