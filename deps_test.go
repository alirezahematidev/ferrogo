package ferrogo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alirezahematidev/ferrogo"
)

// The spec's dependency example, built offline from the local Cargo cache.
const serdeProgram = `
use serde_json::json;

fn main() {
    let data = json!({
        "language": "Rust",
        "fast": true
    });
    println!("{}", data);
}
`

var serdeDep = ferrogo.Dependency{Name: "serde_json", Version: "1", Features: []string{"preserve_order"}}

func TestDependencyPolicyDeniesByDefault(t *testing.T) {
	rt := wasmRuntime(t)
	_, err := rt.Run(context.Background(), ferrogo.Code(serdeProgram), ferrogo.WithDependencies(serdeDep))
	if !errors.Is(err, ferrogo.ErrDependencyPolicy) || !strings.Contains(err.Error(), "AllowRegistry") {
		t.Fatalf("err = %v, want ErrDependencyPolicy with a hint", err)
	}
	strict := wasmRuntime(t, ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{AllowRegistry: true, Crates: []string{"itoa"}}))
	if _, err := strict.Run(context.Background(), ferrogo.Code(serdeProgram), ferrogo.WithDependencies(serdeDep)); !errors.Is(err, ferrogo.ErrDependencyPolicy) {
		t.Errorf("crate allowlist not enforced: %v", err)
	}
	for name, d := range map[string]ferrogo.Dependency{
		"bad name":    {Name: "serde json", Version: "1"},
		"two sources": {Name: "x", Version: "1", Git: "https://example.com/x.git"},
		"no source":   {Name: "x"},
		"injection":   {Name: "x", Version: "1\"\n[profile.release]"},
		"bad feature": {Name: "x", Version: "1", Features: []string{"a b"}},
		"reserved":    {Name: "ferrogo", Version: "1"},
		"ref, no git": {Name: "x", Version: "1", Rev: "abc"},
		"bad git url": {Name: "x", Git: "ftp://example.com/x"},
	} {
		if _, err := ferrogo.New(ferrogo.WithDependencies(d)); err == nil {
			t.Errorf("%s: invalid dependency accepted", name)
		}
	}
}

func TestRegistryDependencyOffline(t *testing.T) {
	home, _ := os.UserHomeDir()
	if matches, _ := filepath.Glob(filepath.Join(home, ".cargo", "registry", "cache", "*", "serde_json-1.*.crate")); len(matches) == 0 {
		t.Skip("serde_json is not in the local Cargo cache")
	}
	rt := wasmRuntime(t, ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{AllowRegistry: true}))
	ctx := context.Background()
	res, err := rt.Run(ctx, ferrogo.Code(serdeProgram), ferrogo.WithDependencies(serdeDep))
	if err != nil {
		var ce *ferrogo.CompileError
		if errors.As(err, &ce) {
			t.Fatalf("%v\n%s", err, ce.Rendered())
		}
		t.Fatal(err)
	}
	if res.Stdout != "{\"language\":\"Rust\",\"fast\":true}\n" {
		t.Errorf("stdout = %q (preserve_order keeps insertion order)", res.Stdout)
	}
	// A different snippet with the same dependencies reuses the resolved
	// lock and the compiled dependencies.
	res2, err := rt.Run(ctx, ferrogo.Code(`fn main() { println!("{}", serde_json::to_string(&vec![1, 2]).unwrap()); }`),
		ferrogo.WithDependencies(serdeDep))
	if err != nil || res2.Stdout != "[1,2]\n" {
		t.Fatalf("second snippet: %v %q", err, res2.Stdout)
	}
	t.Logf("first build (deps compiled): %s; second snippet (deps reused): %s", res.Artifact.BuildDuration, res2.Artifact.BuildDuration)
	if res2.Artifact.BuildDuration*2 > res.Artifact.BuildDuration {
		t.Errorf("dependencies were not reused: %s vs %s", res2.Artifact.BuildDuration, res.Artifact.BuildDuration)
	}
}

// writeCrate writes a minimal Cargo package.
func writeCrate(t *testing.T, dir, manifest string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPathDependency(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "greeting")
	writeCrate(t, lib, "[package]\nname = \"greeting\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[workspace]\n",
		map[string]string{"src/lib.rs": `pub fn hello(name: &str) -> String { format!("hello, {name}") }`})
	dep := ferrogo.Dependency{Name: "greeting", Path: lib}
	prog := ferrogo.Code(`fn main() { println!("{}", greeting::hello("deps")); }`)

	denied := wasmRuntime(t, ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{AllowRegistry: true}))
	if _, err := denied.Run(context.Background(), prog, ferrogo.WithDependencies(dep)); !errors.Is(err, ferrogo.ErrDependencyPolicy) {
		t.Fatalf("path dependency allowed without AllowPath: %v", err)
	}
	rt := wasmRuntime(t, ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{AllowPath: true}))
	res, err := rt.Run(context.Background(), prog, ferrogo.WithDependencies(dep))
	if err != nil || res.Stdout != "hello, deps\n" {
		t.Fatalf("run with path dependency: %v %+v", err, res)
	}
}

func TestProjectSources(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool")
	writeCrate(t, dir, `[package]
name = "tool"
version = "0.1.0"
edition = "2021"

[profile.fast]
inherits = "release"
opt-level = 1

[workspace]
`, map[string]string{"src/main.rs": "mod util;\nfn main() {\n    println!(\"{}\", util::answer());\n    if std::env::args().count() > 1 { panic!(\"asked to fail\"); }\n}\n",
		"src/util.rs": "pub fn answer() -> u32 { 42 }\n"})

	ctx := context.Background()
	untrusted := wasmRuntime(t)
	if _, err := untrusted.Run(ctx, ferrogo.Project(dir)); !errors.Is(err, ferrogo.ErrUntrusted) {
		t.Fatalf("project built without trust: %v", err)
	}

	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeProcess} {
		rt := nativeRuntime(t)
		res, err := rt.Run(ctx, ferrogo.Project(dir), ferrogo.WithBackend(backend), ferrogo.WithProfile("fast"))
		if err != nil || res.Stdout != "42\n" {
			t.Fatalf("%s: multi-file project run: %v %+v", backend, err, res)
		}
		_, err = rt.Run(ctx, ferrogo.Project(dir), ferrogo.WithBackend(backend), ferrogo.WithArgs("fail"))
		var p *ferrogo.PanicError
		if !errors.As(err, &p) || p.File != filepath.ToSlash(filepath.Join(dir, "src/main.rs")) || p.Line != 4 {
			t.Errorf("%s: panic location = %+v (err %v)", backend, p, err)
		}
	}

	// Diagnostics point into the project.
	if err := os.WriteFile(filepath.Join(dir, "src/util.rs"), []byte("pub fn answer() -> u32 { \"no\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := nativeRuntime(t)
	_, err := rt.Run(ctx, ferrogo.Project(dir))
	var ce *ferrogo.CompileError
	if !errors.As(err, &ce) || len(ce.Errors()) == 0 || ce.Errors()[0].Location() != filepath.ToSlash(filepath.Join(dir, "src/util.rs"))+":1:26" {
		t.Fatalf("project diagnostics: %v", err)
	}
}

func TestProjectDependencyPolicy(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "helper")
	writeCrate(t, lib, "[package]\nname = \"helper\"\nversion = \"0.1.0\"\nedition = \"2021\"\n", map[string]string{"src/lib.rs": "pub fn n() -> i32 { 7 }\n"})
	app := filepath.Join(root, "app")
	writeCrate(t, app, "[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nhelper = { path = \"../helper\" }\n\n[workspace]\n",
		map[string]string{"src/main.rs": "fn main() { println!(\"{}\", helper::n()); }\n"})
	ctx := context.Background()
	rt := nativeRuntime(t)
	if _, err := rt.Run(ctx, ferrogo.Project(app)); !errors.Is(err, ferrogo.ErrDependencyPolicy) {
		t.Fatalf("project dependency not checked: %v", err)
	}
	allowed := nativeRuntime(t, ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{AllowPath: true}))
	res, err := allowed.Run(ctx, ferrogo.Project(app))
	if err != nil || res.Stdout != "7\n" {
		t.Fatalf("project with path dependency: %v %+v", err, res)
	}
}
