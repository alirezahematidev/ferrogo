package ferrogo_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

// sharedCache keeps integration builds warm across tests in one run.
var sharedCache string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ferrogo-test-cache-")
	if err != nil {
		panic(err)
	}
	sharedCache = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// nativeRuntime returns a runtime on the native-process backend, skipping
// when no Rust toolchain is installed.
func nativeRuntime(t *testing.T, opts ...ferrogo.Option) *ferrogo.Runtime {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	base := []ferrogo.Option{
		ferrogo.WithCacheDir(sharedCache),
		ferrogo.WithTrust(ferrogo.Trusted),
		ferrogo.WithBackend(ferrogo.BackendNativeProcess),
		ferrogo.WithProfile(ferrogo.ProfileDebug), // faster builds in tests
	}
	rt, err := ferrogo.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rt
}

func TestRunHelloWorld(t *testing.T) {
	rt := nativeRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`
		fn main() {
			println!("Hello from Rust!");
		}
	`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "Hello from Rust!\n" || res.ExitCode != 0 {
		t.Errorf("stdout=%q exit=%d", res.Stdout, res.ExitCode)
	}
	if res.Backend != ferrogo.BackendNativeProcess || res.Artifact == nil || res.Artifact.Key == "" {
		t.Errorf("result metadata = %+v", res)
	}
}

func TestRunReleaseProfileAndCacheReuse(t *testing.T) {
	rt := nativeRuntime(t, ferrogo.WithProfile(ferrogo.ProfileRelease))
	code := ferrogo.Code(`fn main() { let v: u64 = (1..=20).product(); println!("{v}"); }`)
	first, err := rt.Run(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.Run(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(second.Stdout) != "2432902008176640000" {
		t.Errorf("stdout = %q", second.Stdout)
	}
	if !second.Artifact.Cached || second.Artifact.Path != first.Artifact.Path {
		t.Errorf("second run was not served from cache: %+v", second.Artifact)
	}
}

func TestRunStdinEnvArgsAndUnicode(t *testing.T) {
	rt := nativeRuntime(t)
	t.Setenv("FERROGO_INHERITED", "from-host")
	t.Setenv("FERROGO_NOT_GRANTED", "secret")
	res, err := rt.Run(context.Background(), ferrogo.Code(`
use std::io::Read;
fn main() {
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input).unwrap();
    let args: Vec<String> = std::env::args().skip(1).collect();
    println!("stdin={}", input.to_uppercase());
    println!("args={}", args.join("|"));
    println!("greeting={}", std::env::var("GREETING").unwrap_or_default());
    println!("inherited={}", std::env::var("FERROGO_INHERITED").unwrap_or_default());
    println!("leaked={}", std::env::var("FERROGO_NOT_GRANTED").is_ok());
    eprintln!("stderr: ✓");
}`),
		ferrogo.WithStdin(strings.NewReader("grüße, 世界")),
		ferrogo.WithArgs("a b", "$(rm -rf /)", "ü"),
		ferrogo.WithEnv(map[string]string{"GREETING": "hallo"}),
		ferrogo.WithInheritEnv("FERROGO_INHERITED"),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "stdin=GRÜSSE, 世界\nargs=a b|$(rm -rf /)|ü\ngreeting=hallo\ninherited=from-host\nleaked=false\n"
	if res.Stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", res.Stdout, want)
	}
	if res.Stderr != "stderr: ✓\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunStreamsOutput(t *testing.T) {
	rt := nativeRuntime(t)
	var live bytes.Buffer
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() { for i in 0..3 { println!("line {i}"); } }`),
		ferrogo.WithStdout(&live))
	if err != nil {
		t.Fatal(err)
	}
	if live.String() != res.Stdout || res.Stdout != "line 0\nline 1\nline 2\n" {
		t.Errorf("live=%q captured=%q", live.String(), res.Stdout)
	}
}

func TestRunExitCodeAndPanic(t *testing.T) {
	rt := nativeRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() { std::process::exit(7); }`))
	var exitErr *ferrogo.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 || res.ExitCode != 7 || exitErr.Panic != nil {
		t.Fatalf("err = %v, res = %+v", err, res)
	}

	res, err = rt.Run(context.Background(), ferrogo.Code("fn main() {\n    let v: Vec<i32> = Vec::new();\n    println!(\"before\");\n    let _ = v[3];\n}\n"))
	var p *ferrogo.PanicError
	if !errors.As(err, &p) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
	if p.File != "main.rs" || p.Line != 4 || !strings.Contains(p.Message, "index out of bounds") {
		t.Errorf("panic = %+v", p)
	}
	if res == nil || res.Stdout != "before\n" || res.ExitCode != 101 {
		t.Errorf("partial result = %+v", res)
	}
}

func TestRunCompileError(t *testing.T) {
	rt := nativeRuntime(t)
	_, err := rt.Run(context.Background(), ferrogo.Code("fn main() {\n    let x: i32 = \"text\";\n}\n"))
	if !errors.Is(err, ferrogo.ErrCompilation) {
		t.Fatalf("err = %v, want ErrCompilation", err)
	}
	var ce *ferrogo.CompileError
	if !errors.As(err, &ce) {
		t.Fatal("not a *CompileError")
	}
	errs := ce.Errors()
	if ce.Reason != "compile" || len(errs) != 1 || errs[0].Code != "E0308" || errs[0].Location() != "main.rs:2:18" {
		t.Errorf("diagnostics = %+v", ce.Diagnostics)
	}
	if !strings.Contains(ce.Rendered(), "--> main.rs:2:18") {
		t.Errorf("rendered = %s", ce.Rendered())
	}
}

func TestRunFileUsesPathInDiagnostics(t *testing.T) {
	rt := nativeRuntime(t)
	path := filepath.Join(t.TempDir(), "broken.rs")
	if err := os.WriteFile(path, []byte("fn main() { undefined_fn(); }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := rt.Run(context.Background(), ferrogo.File(path))
	var ce *ferrogo.CompileError
	if !errors.As(err, &ce) || len(ce.Errors()) == 0 || ce.Errors()[0].Spans[0].File != filepath.ToSlash(path) {
		t.Fatalf("err = %v", err)
	}
	if _, err := rt.Run(context.Background(), ferrogo.File(filepath.Join(t.TempDir(), "missing.rs"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v", err)
	}
}

func TestRunTimeoutStopsInfiniteLoop(t *testing.T) {
	rt := nativeRuntime(t)
	code := ferrogo.Code(`fn main() {
    if std::env::args().nth(1).as_deref() == Some("warm") { return; }
    println!("started");
    let mut x: u64 = 0;
    loop { x = x.wrapping_add(1); std::hint::black_box(x); }
}`)
	// Compile and execute once first: the timeout must measure the loop, not
	// compilation or the OS's first-launch checks (~300ms on macOS).
	if _, err := rt.Run(context.Background(), code, ferrogo.WithArgs("warm")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := rt.Run(context.Background(), code, ferrogo.WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ferrogo.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("termination took %s", elapsed)
	}
	if res == nil || res.Stdout != "started\n" {
		t.Errorf("partial output lost: %+v", res)
	}
}

func TestRunCancellation(t *testing.T) {
	rt := nativeRuntime(t)
	code := ferrogo.Code(`fn main() { std::thread::sleep(std::time::Duration::from_secs(60)); }`)
	if _, err := rt.Build(context.Background(), code, ferrogo.ArtifactProgram); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := rt.Run(ctx, code, ferrogo.WithTimeout(0))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunOutputLimit(t *testing.T) {
	rt := nativeRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() { loop { println!("flood"); } }`),
		ferrogo.WithMaxOutput(32<<10))
	if !errors.Is(err, ferrogo.ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
	if got := len(res.Stdout) + len(res.Stderr); got != 32<<10 {
		t.Errorf("captured %d bytes", got)
	}
}

func TestRunLargeOutput(t *testing.T) {
	rt := nativeRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() { let line = "x".repeat(1023); for _ in 0..2048 { println!("{line}"); } }`),
		ferrogo.WithMaxOutput(4<<20))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 2<<20 {
		t.Errorf("stdout = %d bytes, want %d", len(res.Stdout), 2<<20)
	}
}

func TestTrustPolicy(t *testing.T) {
	rt, err := ferrogo.New(ferrogo.WithCacheDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for _, b := range []ferrogo.Backend{ferrogo.BackendNativeProcess, ferrogo.BackendNativeLibrary} {
		_, err := rt.Run(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.WithBackend(b))
		if !errors.Is(err, ferrogo.ErrUntrusted) {
			t.Errorf("%s without trust: err = %v, want ErrUntrusted", b, err)
		}
	}
}

func TestUnsupportedIsExplicit(t *testing.T) {
	rt := nativeRuntime(t)
	_, err := rt.Run(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.WithMemoryLimit(64<<20))
	var ue *ferrogo.UnsupportedError
	if !ferrogo.CapabilitiesOf(ferrogo.BackendNativeProcess).MemoryLimit && (!errors.As(err, &ue) || ue.Feature != "memory limits") {
		t.Errorf("memory limit on native process: err = %v, want UnsupportedError", err)
	}
	_, err = rt.Run(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.WithBackend(ferrogo.BackendNativeLibrary))
	if !errors.Is(err, ferrogo.ErrUnsupported) {
		t.Errorf("Run on native-library: err = %v, want ErrUnsupported", err)
	}
}

func TestConcurrentRuns(t *testing.T) {
	rt := nativeRuntime(t)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Three distinct programs, each run four times concurrently.
			code := ferrogo.Code("fn main() { println!(\"program " + string(rune('a'+i%3)) + "\"); }")
			res, err := rt.Run(context.Background(), code)
			if err == nil && res.Stdout != "program "+string(rune('a'+i%3))+"\n" {
				err = errors.New("wrong output: " + res.Stdout)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestCloseRejectsNewWorkAndCancelsRunning(t *testing.T) {
	rt := nativeRuntime(t)
	code := ferrogo.Code(`fn main() { std::thread::sleep(std::time::Duration::from_secs(60)); }`)
	if _, err := rt.Build(context.Background(), code, ferrogo.ArtifactProgram); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), code, ferrogo.WithTimeout(0))
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, ferrogo.ErrClosed) {
		t.Errorf("in-flight run: err = %v, want ErrClosed", err)
	}
	if _, err := rt.Run(context.Background(), code); !errors.Is(err, ferrogo.ErrClosed) {
		t.Errorf("run after close: err = %v, want ErrClosed", err)
	}
}

func TestCacheDisabledUsesTemporaryStore(t *testing.T) {
	rt := nativeRuntime(t, ferrogo.WithCache(false))
	code := ferrogo.Code(`fn main() { println!("uncached"); }`)
	a, err := rt.Run(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rt.Run(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if a.Artifact.Cached || b.Artifact.Cached {
		t.Error("artifact reused with caching disabled")
	}
	stats, err := rt.CacheStats()
	if err != nil || stats.Enabled {
		t.Errorf("stats = %+v, %v", stats, err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stats.Dir); !os.IsNotExist(err) {
		t.Errorf("temporary cache %s not removed: %v", stats.Dir, err)
	}
}

func TestInspect(t *testing.T) {
	rt := nativeRuntime(t)
	rep, err := rt.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Toolchain == nil || rep.Toolchain.Version == "" {
		t.Fatalf("toolchain missing: %s", rep.ToolchainError)
	}
	for _, b := range rep.Backends {
		if b.Backend == ferrogo.BackendNativeProcess && !b.Usable {
			t.Errorf("native-process should be usable: %v", b.Problems)
		}
		if b.Backend == ferrogo.BackendNativeLibrary && b.Usable != ferrogo.CapabilitiesOf(b.Backend).Call {
			t.Errorf("native-library usable=%v disagrees with its capabilities: %v", b.Usable, b.Problems)
		}
	}
}

func TestBuildLibrary(t *testing.T) {
	rt := nativeRuntime(t)
	art, err := rt.Build(context.Background(), ferrogo.Code(`
		#[no_mangle]
		pub extern "C" fn calculate(a: i32, b: i32) -> i32 { a + b }
	`), ferrogo.ArtifactLibrary, ferrogo.WithBackend(ferrogo.BackendNativeLibrary))
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Ext(art.Path)
	if ext != ".so" && ext != ".dylib" && ext != ".dll" {
		t.Errorf("library artifact %s", art.Path)
	}
}

func TestNativeMemoryLimit(t *testing.T) {
	if !ferrogo.CapabilitiesOf(ferrogo.BackendNativeProcess).MemoryLimit {
		t.Skip("native memory limits are not supported on this platform")
	}
	rt := nativeRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() {
    let mut v: Vec<Vec<u8>> = Vec::new();
    for _ in 0..1024 { v.push(vec![1u8; 1 << 20]); }
    println!("{}", v.len());
}`), ferrogo.WithMemoryLimit(128<<20))
	if errors.Is(err, ferrogo.ErrUnsupported) {
		t.Skipf("memory limit tool unavailable: %v", err)
	}
	if !errors.Is(err, ferrogo.ErrMemoryLimit) {
		t.Fatalf("err = %v (stdout %q), want ErrMemoryLimit", err, res.Stdout)
	}
}
