package ferrogo_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alirezahematidev/ferrogo"
)

const sandboxProbe = `use std::time::Duration;
fn main() {
    let env = |k: &str| std::env::var(k).unwrap();
    println!("read_secret={}", std::fs::read_to_string(env("SECRET")).is_ok());
    println!("write_home={}", std::fs::write(format!("{}/probe.txt", env("HOMEDIR")), "x").is_ok());
    println!("write_cwd={}", std::fs::write("ok.txt", "x").is_ok());
    println!("write_granted={}", std::fs::write(format!("{}/g.txt", env("GRANTED")), "x").is_ok());
    println!("write_other={}", std::fs::write(format!("{}/o.txt", env("OTHER")), "x").is_ok());
    let addr = env("ADDR").parse().unwrap();
    println!("net={}", std::net::TcpStream::connect_timeout(&addr, Duration::from_secs(2)).is_ok());
}`

func TestNativeSandbox(t *testing.T) {
	rt := nativeRuntime(t)
	ctx := context.Background()

	cacheHome, err := os.UserCacheDir() // inside the home directory
	if err != nil {
		t.Skip(err)
	}
	secretDir, err := os.MkdirTemp(cacheHome, "ferrogo-sandbox-test-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(secretDir)
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	granted, other := t.TempDir(), t.TempDir()
	env := ferrogo.WithEnv(map[string]string{
		"SECRET": secret, "HOMEDIR": secretDir, "GRANTED": granted, "OTHER": other, "ADDR": ln.Addr().String(),
	})

	// Control run: without the sandbox every probe succeeds.
	res, err := rt.Run(ctx, ferrogo.Code(sandboxProbe), env)
	if err != nil {
		t.Fatalf("%v\n%s", err, res.Stderr)
	}
	if res.Stdout != "read_secret=true\nwrite_home=true\nwrite_cwd=true\nwrite_granted=true\nwrite_other=true\nnet=true\n" {
		t.Fatalf("control run:\n%s", res.Stdout)
	}

	res, err = rt.Run(ctx, ferrogo.Code(sandboxProbe), env, ferrogo.WithSandbox(ferrogo.Sandbox{Writable: []string{granted}}))
	if errors.Is(err, ferrogo.ErrUnsupported) {
		t.Skipf("no OS sandbox on this machine: %v", err)
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, res.Stderr)
	}
	want := "read_secret=false\nwrite_home=false\nwrite_cwd=true\nwrite_granted=true\nwrite_other=false\nnet=false\n"
	if res.Stdout != want {
		t.Errorf("sandboxed run:\n%s\nwant:\n%s", res.Stdout, want)
	}

	res, err = rt.Run(ctx, ferrogo.Code(sandboxProbe), env,
		ferrogo.WithSandbox(ferrogo.Sandbox{AllowNetwork: true, Readable: []string{secretDir}}))
	if err != nil || !strings.Contains(res.Stdout, "read_secret=true") || !strings.Contains(res.Stdout, "net=true") {
		t.Errorf("sandbox grants not applied: %v\n%s", err, res.Stdout)
	}

	wasm := wasmRuntime(t)
	if _, err := wasm.Run(ctx, ferrogo.Code(`fn main() {}`), ferrogo.WithSandbox(ferrogo.Sandbox{})); !errors.Is(err, ferrogo.ErrUnsupported) {
		t.Errorf("WithSandbox on WASM: err = %v", err)
	}
}

func TestCompilerSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	cacheHome, err := os.UserCacheDir() // inside the home directory
	if err != nil {
		t.Skip(err)
	}
	secretDir, err := os.MkdirTemp(cacheHome, "ferrogo-compiler-sandbox-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(secretDir)
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	exfiltrate := ferrogo.Code(`const S: &str = include_str!("` + filepath.ToSlash(secret) + `"); fn main() { print!("{S}"); }`)
	ctx := context.Background()

	// Without the compiler sandbox, include_str! reads the file: the threat.
	plain := wasmRuntime(t)
	res, err := plain.Run(ctx, exfiltrate)
	if err != nil || res.Stdout != "PRIVATE KEY" {
		t.Fatalf("control run: %v %+v", err, res)
	}

	// Same cache as the control run: its artifact (which embeds the secret)
	// must not be served to a sandboxed runtime.
	sandboxed := wasmRuntime(t, ferrogo.WithCompilerSandbox(true), ferrogo.WithTrust(ferrogo.Trusted))
	_, err = sandboxed.Run(ctx, exfiltrate)
	if errors.Is(err, ferrogo.ErrUnsupported) {
		t.Skipf("no OS sandbox: %v", err)
	}
	var ce *ferrogo.CompileError
	if !errors.As(err, &ce) || !strings.Contains(ce.Rendered(), "couldn't read") {
		t.Fatalf("include_str! of a home-directory secret was not blocked: %v", err)
	}

	// Ordinary builds still work under the sandbox: WASM, native (linker),
	// and offline dependencies from the Cargo home.
	for _, b := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeProcess} {
		res, err := sandboxed.Run(ctx, ferrogo.Code(`fn main() { println!("ok"); }`), ferrogo.WithBackend(b))
		if err != nil || res.Stdout != "ok\n" {
			t.Errorf("%s build under the compiler sandbox: %v", b, err)
		}
	}
	home, _ := os.UserHomeDir()
	if m, _ := filepath.Glob(filepath.Join(home, ".cargo", "registry", "cache", "*", "serde_json-1.*.crate")); len(m) > 0 {
		deps := wasmRuntime(t, ferrogo.WithCompilerSandbox(true), ferrogo.WithCache(false),
			ferrogo.WithDependencyPolicy(ferrogo.DependencyPolicy{AllowRegistry: true}))
		res, err := deps.Run(ctx, ferrogo.Code(serdeProgram), ferrogo.WithDependencies(serdeDep))
		if err != nil || !strings.Contains(res.Stdout, "Rust") {
			var ce *ferrogo.CompileError
			if errors.As(err, &ce) {
				t.Fatalf("dependency build under the sandbox: %v\n%s", err, ce.Output)
			}
			t.Fatalf("dependency build under the sandbox: %v", err)
		}
	}
}
