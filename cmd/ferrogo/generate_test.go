package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGenerateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/calc\n\ngo 1.25\n\nrequire github.com/alirezahematidev/ferrogo v0.0.0\n\nreplace github.com/alirezahematidev/ferrogo => "+repo+"\n")
	calc := `package main

import (
	"context"
	"fmt"
)

/*ferrogo
pub fn add(a: i32, b: i32) -> i32 { a + b }

pub fn greet(name: String) -> Result<String, String> {
    if name.is_empty() { return Err("empty".into()); }
    Ok(format!("hi {name}"))
}

pub fn tag(doc: ferrogo::Json) -> ferrogo::Json { ferrogo::Json(format!("[{}]", doc.0)) }
*/

func main() {
	ctx := context.Background()
	sum, err := Add(ctx, 2, 40)
	fmt.Println(sum, err)
	g, err := Greet(ctx, "go")
	fmt.Println(g, err)
	_, err = Greet(ctx, "")
	fmt.Println(err)
	j, err := Tag(ctx, map[string]int{"n": 1})
	fmt.Println(string(j), err)
}
`
	write("calc.go", calc)
	goEnv := append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	goCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", args...)
		cmd.Dir, cmd.Env = dir, goEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go %v: %v\n%s%s", args, err, out, stderr.Bytes())
		}
		return string(out)
	}
	goCmd("mod", "tidy")

	cacheDir := filepath.Join(t.TempDir(), "cache")
	gen := func(extra ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"generate", "--cache-dir", cacheDir, dir}, extra...), nil, &stdout, &stderr)
		return code, stderr.String()
	}
	if code, errOut := gen(); code != 0 {
		t.Fatalf("generate failed:\n%s", errOut)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "ferrogo_generated.go"))
	firstWASM, _ := os.ReadFile(filepath.Join(dir, "ferrogo_generated.wasm"))

	// The generated package builds and runs in an ordinary module.
	want := "42 <nil>\nhi go <nil>\nferrogo: greet returned an error: empty\n[{\"n\":1}] <nil>\n"
	if got := goCmd("run", "."); got != want {
		t.Errorf("program output:\n%s\nwant:\n%s", got, want)
	}

	// Output is reproducible.
	if code, errOut := gen(); code != 0 {
		t.Fatal(errOut)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "ferrogo_generated.go"))
	secondWASM, _ := os.ReadFile(filepath.Join(dir, "ferrogo_generated.wasm"))
	if !bytes.Equal(first, second) || !bytes.Equal(firstWASM, secondWASM) {
		t.Error("generate is not reproducible")
	}
	if code, _ := gen("-check"); code != 0 {
		t.Error("-check reports fresh output as stale")
	}

	// Editing the Rust makes -check fail, and compile errors point at the .go file.
	broken := strings.Replace(calc, "a + b }", "a + \"b\" }", 1)
	write("calc.go", broken)
	if code, _ := gen("-check"); code != 1 {
		t.Error("-check did not detect stale output")
	}
	code, errOut := gen()
	if code != 1 || !strings.Contains(errOut, filepath.ToSlash(filepath.Join(dir, "calc.go"))+":9:") {
		t.Errorf("compile error not mapped to calc.go line 9 (code %d):\n%s", code, errOut)
	}
}
