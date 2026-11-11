package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseInterspersedFlags(t *testing.T) {
	c := newCommon("run", "run", &bytes.Buffer{})
	pos, rest, err := c.parse([]string{"main.rs", "--backend", "native", "--json", "--", "--not-a-flag", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"main.rs"}) || !reflect.DeepEqual(rest, []string{"--not-a-flag", "x"}) {
		t.Errorf("positional=%v rest=%v", pos, rest)
	}
	if c.backend != "native" || !c.json {
		t.Errorf("flags not parsed: backend=%q json=%v", c.backend, c.json)
	}
	if _, _, err := newCommon("run", "run", &bytes.Buffer{}).parse([]string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestExplicitNativeBackendImpliesTrust(t *testing.T) {
	c := newCommon("run", "run", &bytes.Buffer{})
	if _, _, err := c.parse([]string{"--backend", "native"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.options(); err != nil {
		t.Fatal(err)
	}
	if !c.trust {
		t.Error("--backend native should imply --trust")
	}
}

func runCLI(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if code, out, _ := runCLI(t, t.TempDir()); code != 2 || !strings.Contains(out, "Usage:") {
		t.Errorf("no args: code=%d", code)
	}
	if code, _, errOut := runCLI(t, t.TempDir(), "frobnicate"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown command: code=%d", code)
	}
}

func TestInitWritesValidConfig(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := runCLI(t, dir, "init"); code != 0 {
		t.Fatalf("init failed: %s", errOut)
	}
	if code, _, _ := runCLI(t, dir, "init"); code != 1 {
		t.Error("init overwrote an existing file without --force")
	}
	// The written config must be accepted by commands that load it.
	if code, _, errOut := runCLI(t, dir, "cache", "--cache-dir", filepath.Join(dir, "c"), "stats"); code != 0 {
		t.Errorf("config rejected: %s", errOut)
	}
}

func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.rs", `fn main() { let a: Vec<String> = std::env::args().skip(1).collect(); println!("{}", a.join(",")); std::process::exit(3); }`)
	write("bad.rs", "fn main() {\n    nope();\n}\n")
	common := []string{"--backend", "native", "--profile", "debug", "--cache-dir", cacheDir}

	code, out, _ := runCLI(t, dir, append(append([]string{"run", "ok.rs"}, common...), "--", "a b", "c")...)
	if code != 3 || out != "a b,c\n" {
		t.Errorf("run: code=%d stdout=%q", code, out)
	}

	code, out, _ = runCLI(t, dir, append([]string{"run", "ok.rs", "--json"}, common...)...)
	var jr struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
		Error    *struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &jr); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if code != 3 || jr.ExitCode != 3 || jr.Error == nil || jr.Error.Kind != "exit" {
		t.Errorf("json run: code=%d %+v", code, jr)
	}

	code, _, errOut := runCLI(t, dir, append([]string{"run", "bad.rs"}, common...)...)
	if code != 1 || !strings.Contains(errOut, "--> bad.rs:2:5") {
		t.Errorf("compile error: code=%d stderr=%s", code, errOut)
	}

	code, _, errOut = runCLI(t, dir, append([]string{"build", "ok.rs", "-o", "ok-bin"}, common...)...)
	if code != 0 {
		t.Fatalf("build: %s", errOut)
	}
	if fi, err := os.Stat(filepath.Join(dir, "ok-bin")); err != nil || fi.Size() == 0 {
		t.Errorf("build output missing: %v", err)
	}
}

func TestCallAndInspect(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	dir := t.TempDir()
	lib := `ferrogo::export! {
    fn add(a: i32, b: i32) -> i32 { a + b }
    fn hexlen(data: Vec<u8>) -> u64 { data.len() as u64 }
    fn fail() -> Result<u32, String> { Err("nope".into()) }
}
`
	if err := os.WriteFile(filepath.Join(dir, "lib.rs"), []byte(lib), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := []string{"--cache-dir", filepath.Join(dir, "cache")}
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"call", "lib.rs", "add", "-5", "7"}, 0, "2\n"},
		{[]string{"call", "lib.rs", "hexlen", "deadbeef"}, 0, "4\n"},
		{[]string{"call", "lib.rs", "fail"}, 1, ""},
		{[]string{"call", "lib.rs", "add", "1"}, 1, ""},
		{[]string{"call", "lib.rs", "missing"}, 1, ""},
	}
	for _, c := range cases {
		args := append([]string{c.args[0]}, cache...) // flags precede the function name
		code, out, errOut := runCLI(t, dir, append(args, c.args[1:]...)...)
		if code != c.code || out != c.out {
			t.Errorf("%v: code=%d out=%q stderr=%s", c.args, code, out, errOut)
		}
	}
	if code, _, errOut := runCLI(t, dir, append([]string{"build", "lib.rs", "--lib", "-o", "lib.wasm"}, cache...)...); code != 0 {
		t.Fatal(errOut)
	}
	code, out, _ := runCLI(t, dir, append([]string{"inspect", "lib.wasm"}, cache...)...)
	if code != 0 || !strings.Contains(out, "fn add(a: i32, b: i32) -> i32") {
		t.Errorf("inspect artifact: code=%d out=%s", code, out)
	}
}

func TestConfigDiscoveryInParents(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ferrogo.toml"), []byte("version = 1\nbakend = \"typo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent's config is found (and its typo reported).
	code, _, errOut := runCLI(t, deep, "cache", "--cache-dir", filepath.Join(root, "c"), "stats")
	if code != 1 || !strings.Contains(errOut, "unknown keys: bakend") {
		t.Errorf("config in a parent directory not discovered: code=%d %s", code, errOut)
	}
}
