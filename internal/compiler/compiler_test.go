package compiler

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/cache"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/process"
	"github.com/alirezahematidev/ferrogo/internal/toolchain"
)

func program(src string) Request {
	return Request{
		Sources: []SourceFile{{Path: "main.rs", Content: []byte(src), DisplayName: "hello.rs"}},
		Kind:    KindProgram, Target: "x86_64-unknown-linux-gnu", Backend: "native-process",
	}
}

func TestValidateRejectsInjection(t *testing.T) {
	cases := map[string]func(*Request){
		"edition":   func(r *Request) { r.Edition = "2021\"\n[dependencies]\nevil = \"1\"" },
		"opt-level": func(r *Request) { r.OptLevel = "3\nlto = true" },
		"profile":   func(r *Request) { r.Profile = "bench" },
		"panic":     func(r *Request) { r.Panic = "unwind\"" },
		"target":    func(r *Request) { r.Target = "x86_64\" --config" },
		"traversal": func(r *Request) { r.Sources = append(r.Sources, SourceFile{Path: "../../etc/x.rs"}) },
		"absolute":  func(r *Request) { r.Sources = append(r.Sources, SourceFile{Path: "/etc/x.rs"}) },
		"non-rust":  func(r *Request) { r.Sources = append(r.Sources, SourceFile{Path: "build.rs.txt"}) },
		"wrongroot": func(r *Request) { r.Kind = KindLibrary },
		"duplicate": func(r *Request) { r.Sources = append(r.Sources, r.Sources[0]) },
	}
	for name, mutate := range cases {
		r := program("fn main() {}")
		mutate(&r)
		if err := r.validate(); err == nil {
			t.Errorf("%s: invalid request accepted", name)
		}
	}
	ok := program("fn main() {}")
	ok.Sources = append(ok.Sources, SourceFile{Path: "util/mod.rs"})
	if err := ok.validate(); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
}

func TestManifest(t *testing.T) {
	r := program("fn main() {}")
	r.OptLevel = "z"
	m := r.manifest()
	for _, want := range []string{`edition = "2021"`, `[[bin]]`, `path = "src/main.rs"`, `[profile.release]`, `opt-level = "z"`, "\n[workspace]\n"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	lib := Request{Sources: []SourceFile{{Path: "lib.rs"}}, Kind: KindLibrary, Profile: ProfileDebug, OptLevel: "1", Panic: "unwind"}
	m = lib.manifest()
	for _, want := range []string{`crate-type = ["cdylib"]`, `[profile.dev]`, "opt-level = 1\n", `panic = "unwind"`} {
		if !strings.Contains(m, want) {
			t.Errorf("library manifest lacks %q:\n%s", want, m)
		}
	}
	ra, rb := program("a"), program("b")
	if a, b := ra.packageName(), rb.packageName(); a == b || !strings.HasPrefix(a, "fg_") {
		t.Errorf("package names %q %q", a, b)
	}
}

func TestParseCargoJSONFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cargo-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	mapper := newSourceMapper([]SourceFile{{Path: "main.rs", DisplayName: "calc.go", LineOffset: 10}})
	out := parseCargoJSON(data, mapper)
	if !out.finished || out.success {
		t.Fatalf("finished=%v success=%v", out.finished, out.success)
	}
	if len(out.diagnostics) != 1 {
		t.Fatalf("got %d diagnostics (noise not filtered?): %+v", len(out.diagnostics), out.diagnostics)
	}
	d := out.diagnostics[0]
	if d.Level != "error" || d.Code != "E0308" || d.Message != "mismatched types" {
		t.Errorf("diagnostic = %+v", d)
	}
	if loc := d.Location(); loc != "calc.go:12:18" {
		t.Errorf("location = %q, want calc.go:12:18 (re-mapped)", loc)
	}
	if !strings.Contains(d.Rendered, "--> calc.go:") || strings.Contains(d.Rendered, "src/main.rs") {
		t.Errorf("rendered not re-mapped:\n%s", d.Rendered)
	}
	if got := classify(out.diagnostics, ""); got != ReasonCompile {
		t.Errorf("classify = %s", got)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		diags  []Diagnostic
		stderr string
		want   Reason
	}{
		{[]Diagnostic{{Level: "error", Code: "E0463", Message: "can't find crate for `std`"}}, "", ReasonTarget},
		{[]Diagnostic{{Level: "error", Message: "linking with `cc` failed: exit status: 1"}}, "", ReasonLink},
		{nil, "error: no matching package named `serde_jsn` found", ReasonDependency},
		{nil, "error: failed to parse manifest at `/x/Cargo.toml`", ReasonManifest},
		{nil, "thread 'main' panicked", ReasonInternal},
		{[]Diagnostic{{Level: "warning", Message: "unused"}}, "", ReasonInternal},
	}
	for _, c := range cases {
		if got := classify(c.diags, c.stderr); got != c.want {
			t.Errorf("classify(%v, %q) = %s, want %s", c.diags, c.stderr, got, c.want)
		}
	}
	err := &Error{Reason: ReasonTimeout}
	if !errors.Is(err, errdefs.ErrCompilation) || !errors.Is(err, errdefs.ErrTimeout) {
		t.Error("timeout error does not match both sentinels")
	}
}

func fakeCompiler(t *testing.T) *Compiler {
	c, err := cache.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	tc := &toolchain.Toolchain{RustcVerbose: "rustc 1.94.1", CargoVersion: "cargo 1.94.1", Targets: []string{"x86_64-unknown-linux-gnu"}}
	return New(tc, c, 1, nil)
}

func TestKeySensitivity(t *testing.T) {
	c := fakeCompiler(t)
	base := program("fn main() {}")
	baseKey, _, err := c.key(base, base.manifest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	again, _, _ := c.key(base, base.manifest(), nil)
	if again != baseKey {
		t.Fatal("key is not deterministic")
	}
	mutations := map[string]func(*Request){
		"source":    func(r *Request) { r.Sources[0].Content = []byte("fn main(){ }") },
		"target":    func(r *Request) { r.Target = "aarch64-unknown-linux-gnu" },
		"backend":   func(r *Request) { r.Backend = "wasm" },
		"profile":   func(r *Request) { r.Profile = ProfileDebug },
		"opt":       func(r *Request) { r.OptLevel = "s" },
		"edition":   func(r *Request) { r.Edition = "2024" },
		"flags":     func(r *Request) { r.RustFlags = []string{"-Ctarget-cpu=native"} },
		"env":       func(r *Request) { r.Env = map[string]string{"FOO": "1"} },
		"panic":     func(r *Request) { r.Panic = "abort" },
		"extrafile": func(r *Request) { r.Sources = append(r.Sources, SourceFile{Path: "util.rs"}) },
	}
	for name, mutate := range mutations {
		r := program("fn main() {}")
		mutate(&r)
		k, _, err := c.key(r, r.manifest(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if k == baseKey {
			t.Errorf("changing %s does not change the key", name)
		}
	}
	// Display names only affect diagnostics, not the artifact.
	r := program("fn main() {}")
	r.Sources[0].DisplayName = "other.rs"
	if k, _, _ := c.key(r, r.manifest(), nil); k != baseKey {
		t.Error("display name changed the key")
	}
	// Sandboxed and unsandboxed builds must never share artifacts.
	c.sandboxed = true
	if k, _, _ := c.key(base, base.manifest(), nil); k == baseKey {
		t.Error("compiler sandbox does not change the key")
	}
	c.sandboxed = false
	// A different toolchain must never share artifacts.
	c.tc.RustcVerbose = "rustc 1.95.0"
	if k, _, _ := c.key(base, base.manifest(), nil); k == baseKey {
		t.Error("toolchain change does not change the key")
	}
}

func TestMissingTargetFailsFast(t *testing.T) {
	c := fakeCompiler(t)
	r := program("fn main() {}")
	r.Target = "wasm32-wasip1"
	_, err := c.Build(context.Background(), r)
	if !errors.Is(err, errdefs.ErrTargetNotInstalled) {
		t.Fatalf("err = %v, want ErrTargetNotInstalled", err)
	}
}

// --- integration tests against the installed toolchain ---

func realCompiler(t *testing.T) (*Compiler, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	tc, err := toolchain.Detect(context.Background(), toolchain.Config{})
	if err != nil {
		t.Skipf("no usable toolchain: %v", err)
	}
	c, err := cache.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	target, err := tc.NativeTarget(runtime.GOOS, runtime.GOARCH)
	if err != nil || !tc.HasTarget(target) {
		t.Skipf("native target unavailable: %v", err)
	}
	return New(tc, c, 4, nil), target
}

func TestBuildAndCacheIntegration(t *testing.T) {
	c, target := realCompiler(t)
	req := program("fn main() {\n    let unused = 1;\n    println!(\"built by ferrogo\");\n}\n")
	req.Target = target
	req.Profile = ProfileDebug
	ctx := context.Background()

	a, err := c.Build(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Cached || a.Size == 0 {
		t.Errorf("first build: %+v", a)
	}
	if len(a.Warnings) != 1 || a.Warnings[0].Location() != "hello.rs:2:9" {
		t.Errorf("warnings = %+v", a.Warnings)
	}
	res, err := process.Run(ctx, process.Spec{Path: a.Path})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "built by ferrogo" {
		t.Fatalf("running artifact: %v %q", err, res.Stdout)
	}

	b, err := c.Build(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Cached || b.Path != a.Path || len(b.Warnings) != 1 {
		t.Errorf("second build not served from cache with warnings: %+v", b)
	}
	work, _ := os.ReadDir(filepath.Join(c.cache.Dir(), "work"))
	if len(work) != 0 {
		t.Errorf("%d build directories leaked", len(work))
	}
}

func TestBuildCompileErrorIntegration(t *testing.T) {
	c, target := realCompiler(t)
	req := program("fn main() {\n    let x: i32 = \"nope\";\n}\n")
	req.Target = target
	req.Profile = ProfileDebug
	_, err := c.Build(context.Background(), req)
	var ce *Error
	if !errors.As(err, &ce) || ce.Reason != ReasonCompile {
		t.Fatalf("err = %v, want compile error", err)
	}
	errs := ce.Errors()
	if len(errs) != 1 || errs[0].Code != "E0308" || errs[0].Location() != "hello.rs:2:18" {
		t.Errorf("diagnostics = %+v", ce.Diagnostics)
	}
	if !strings.Contains(err.Error(), "hello.rs:2:18: mismatched types") {
		t.Errorf("error message = %q", err)
	}
}

func TestBuildSecretsNotVisibleIntegration(t *testing.T) {
	c, target := realCompiler(t)
	t.Setenv("FERROGO_TEST_SECRET", "hunter2")
	req := program(`fn main() { println!("{}", env!("FERROGO_TEST_SECRET")); }`)
	req.Target = target
	req.Profile = ProfileDebug
	_, err := c.Build(context.Background(), req)
	var ce *Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Error(), "FERROGO_TEST_SECRET") {
		t.Fatalf("host secret was visible to env!() at compile time: err = %v", err)
	}
}

func TestConcurrentIdenticalBuildsCoalesceIntegration(t *testing.T) {
	c, target := realCompiler(t)
	req := program("fn main() { println!(\"once\"); }")
	req.Target = target
	req.Profile = ProfileDebug
	var wg sync.WaitGroup
	paths := make([]string, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := c.Build(context.Background(), req)
			errs[i] = err
			if a != nil {
				paths[i] = a.Path
			}
		}()
	}
	wg.Wait()
	for i := range 8 {
		if errs[i] != nil || paths[i] != paths[0] {
			t.Fatalf("build %d: path=%q err=%v", i, paths[i], errs[i])
		}
	}
}

func TestBuildTimeoutIntegration(t *testing.T) {
	c, target := realCompiler(t)
	req := program("fn main() {}")
	req.Target = target
	req.Timeout = time.Millisecond
	_, err := c.Build(context.Background(), req)
	if !errors.Is(err, errdefs.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// TestWindowsPathsAreMapped feeds the real cargo fixture with the backslash
// paths rustc reports on Windows (src\main.rs).
func TestWindowsPathsAreMapped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cargo-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "src/main.rs", `src\\main.rs`)) // JSON-escaped backslash
	out := parseCargoJSON(data, newSourceMapper([]SourceFile{{Path: "main.rs", DisplayName: "calc.go", LineOffset: 10}}))
	if len(out.diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v", out.diagnostics)
	}
	d := out.diagnostics[0]
	if d.Location() != "calc.go:12:18" || !strings.Contains(d.Rendered, "--> calc.go:12:18") {
		t.Errorf("Windows path not mapped: %s\n%s", d.Location(), d.Rendered)
	}
	if f, l, _, ok := LocateGenerated([]SourceFile{{Path: "lib.rs", DisplayName: "x.go", LineOffset: 2}}, `src\lib.rs`, 3, 1); !ok || f != "x.go" || l != 5 {
		t.Errorf("LocateGenerated(src\\lib.rs) = %s:%d ok=%v", f, l, ok)
	}
}
