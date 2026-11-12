package ferrogo_test

import (
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

// testWASMBackend is the WASM engine the TestWASM* suite runs on: wasm by
// default; TestWASMSuiteOnWazy re-runs the suite with it set to wazy.
var testWASMBackend = func() ferrogo.Backend {
	if os.Getenv("FERROGO_TEST_WASM_BACKEND") == "wazy" {
		return ferrogo.BackendWazy
	}
	return ferrogo.BackendWASM
}()

// TestWASMSuiteOnWazy runs every TestWASM* and host-function test again on
// the wazy backend, in a child test process.
func TestWASMSuiteOnWazy(t *testing.T) {
	if testWASMBackend == ferrogo.BackendWazy {
		t.Skip("already running on wazy")
	}
	wasmRuntime(t) // same skips as the suite
	cmd := exec.Command(os.Args[0], "-test.run=^(TestWASM|TestHostFunctions)", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "FERROGO_TEST_WASM_BACKEND=wazy")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("suite on wazy: %v\n%s", err, out)
	}
	t.Logf("suite on wazy passed:\n%s", out)
}

// wasmRuntime returns a runtime with default settings (BackendAuto = WASM,
// Untrusted), skipping when the toolchain or the wasm32-wasip1 target is
// missing.
func wasmRuntime(t *testing.T, opts ...ferrogo.Option) *ferrogo.Runtime {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	base := []ferrogo.Option{ferrogo.WithCacheDir(sharedCache)}
	if testWASMBackend != ferrogo.BackendWASM {
		base = append(base, ferrogo.WithBackend(testWASMBackend))
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
	tc, err := rt.Toolchain(context.Background())
	if err != nil {
		t.Skipf("no toolchain: %v", err)
	}
	if !tc.HasTarget("wasm32-wasip1") {
		t.Skip("wasm32-wasip1 target not installed (rustup target add wasm32-wasip1)")
	}
	return rt
}

func TestWASMIsTheDefaultAndNeedsNoTrust(t *testing.T) {
	rt := wasmRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`
		fn main() {
			println!("Hello from Rust!");
		}
	`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "Hello from Rust!\n" || res.Backend != testWASMBackend || res.Artifact.Target != "wasm32-wasip1" {
		t.Errorf("result = %+v", res)
	}
	if res.Usage.PeakMemoryBytes == 0 {
		t.Error("guest memory size not reported")
	}
}

func TestWASMStdinEnvArgsAndIsolation(t *testing.T) {
	rt := wasmRuntime(t)
	t.Setenv("FERROGO_HOST_SECRET", "secret")
	res, err := rt.Run(context.Background(), ferrogo.Code(`
use std::io::Read;
fn main() {
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input).unwrap();
    let args: Vec<String> = std::env::args().skip(1).collect();
    println!("stdin={}", input.trim().to_uppercase());
    println!("args={}", args.join("|"));
    println!("granted={}", std::env::var("GRANTED").unwrap_or_default());
    println!("secret_visible={}", std::env::var("FERROGO_HOST_SECRET").is_ok());
    println!("fs={}", std::fs::read_to_string("/etc/passwd").is_ok());
    println!("cwd_fs={}", std::fs::read_dir(".").is_ok());
    println!("net={}", std::net::TcpStream::connect("127.0.0.1:80").is_ok());
    println!("thread={}", std::thread::Builder::new().spawn(|| ()).is_ok());
    std::process::exit(3);
}`),
		ferrogo.WithStdin(strings.NewReader("grüße 世界\n")),
		ferrogo.WithArgs("a b", "ü"),
		ferrogo.WithEnv(map[string]string{"GRANTED": "yes"}),
	)
	var exitErr *ferrogo.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	want := "stdin=GRÜSSE 世界\nargs=a b|ü\ngranted=yes\nsecret_visible=false\nfs=false\ncwd_fs=false\nnet=false\nthread=false\n"
	if res.Stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", res.Stdout, want)
	}
}

func TestWASMPanic(t *testing.T) {
	rt := wasmRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code("fn main() {\n    println!(\"before\");\n    let v: Vec<u8> = vec![];\n    let _ = v[1];\n}\n"))
	var exitErr *ferrogo.ExitError
	if !errors.As(err, &exitErr) || exitErr.Trap != "unreachable" {
		t.Fatalf("err = %v, want unreachable trap", err)
	}
	var p *ferrogo.PanicError
	if !errors.As(err, &p) || p.File != "main.rs" || p.Line != 4 || !strings.Contains(p.Message, "index out of bounds") {
		t.Errorf("panic = %+v", p)
	}
	if res.Stdout != "before\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestWASMInfiniteLoopTimeout(t *testing.T) {
	rt := wasmRuntime(t)
	code := ferrogo.Code(`fn main() { println!("spinning"); let mut x: u64 = 0; loop { x = x.wrapping_add(1); std::hint::black_box(x); } }`)
	if _, err := rt.Build(context.Background(), code, ferrogo.ArtifactProgram); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := rt.Run(context.Background(), code, ferrogo.WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ferrogo.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("interruption took %s", elapsed)
	}
	if res.Stdout != "spinning\n" {
		t.Errorf("partial stdout = %q", res.Stdout)
	}
}

func TestWASMSleepIsInterruptible(t *testing.T) {
	rt := wasmRuntime(t)
	code := ferrogo.Code(`fn main() { std::thread::sleep(std::time::Duration::from_secs(120)); println!("woke"); }`)
	if _, err := rt.Build(context.Background(), code, ferrogo.ArtifactProgram); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := rt.Run(context.Background(), code, ferrogo.WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ferrogo.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("sleep outlived its timeout: %s", elapsed)
	}

	// Short sleeps really sleep.
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() { let t = std::time::Instant::now(); std::thread::sleep(std::time::Duration::from_millis(50)); println!("{}", t.elapsed().as_millis() >= 50); }`))
	if err != nil || res.Stdout != "true\n" {
		t.Errorf("sleep: %v %q", err, res.Stdout)
	}
}

func TestWASMBlockedStdinIsInterruptible(t *testing.T) {
	rt := wasmRuntime(t)
	code := ferrogo.Code(`use std::io::Read; fn main() { let mut s = String::new(); std::io::stdin().read_to_string(&mut s).unwrap(); }`)
	if _, err := rt.Build(context.Background(), code, ferrogo.ArtifactProgram); err != nil {
		t.Fatal(err)
	}
	pr, pw := ioPipe(t)
	defer pw.Close()
	start := time.Now()
	_, err := rt.Run(context.Background(), code, ferrogo.WithStdin(pr), ferrogo.WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ferrogo.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("blocked read outlived its timeout: %s", elapsed)
	}
}

func ioPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, w
}

func TestWASMMemoryLimit(t *testing.T) {
	rt := wasmRuntime(t)
	code := ferrogo.Code(`fn main() {
    let mut chunks: Vec<Vec<u8>> = Vec::new();
    loop { chunks.push(vec![1u8; 1 << 20]); }
}`)
	res, err := rt.Run(context.Background(), code, ferrogo.WithMemoryLimit(32<<20))
	if !errors.Is(err, ferrogo.ErrMemoryLimit) {
		t.Fatalf("err = %v, want ErrMemoryLimit", err)
	}
	if res == nil || res.Usage.PeakMemoryBytes > 32<<20 {
		t.Errorf("result = %+v", res)
	}
	// Within the limit, the same allocation pattern succeeds.
	res, err = rt.Run(context.Background(), ferrogo.Code(`fn main() { let v = vec![7u8; 8 << 20]; println!("{}", v.len()); }`),
		ferrogo.WithWASMMemoryLimit(64<<20))
	if err != nil || res.Stdout != "8388608\n" {
		t.Errorf("allocation within limit: %v %q", err, res.Stdout)
	}
}

func TestWASMOutputLimit(t *testing.T) {
	rt := wasmRuntime(t)
	res, err := rt.Run(context.Background(), ferrogo.Code(`fn main() { loop { println!("flood"); } }`), ferrogo.WithMaxOutput(16<<10))
	if !errors.Is(err, ferrogo.ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
	if got := len(res.Stdout) + len(res.Stderr); got != 16<<10 {
		t.Errorf("captured %d bytes", got)
	}
}

func TestWASMMounts(t *testing.T) {
	rt := wasmRuntime(t)
	ro, rw := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "input.txt"), []byte("from host"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := rt.Run(context.Background(), ferrogo.Code(`
fn main() {
    let input = std::fs::read_to_string("/in/input.txt").unwrap();
    println!("read={input}");
    println!("ro_write={}", std::fs::write("/in/evil.txt", "x").is_ok());
    std::fs::write("/out/result.txt", input.to_uppercase()).unwrap();
    println!("escape={}", std::fs::read_to_string("/in/../../etc/passwd").is_ok());
}`),
		ferrogo.WithMount(ferrogo.Mount{HostPath: ro, GuestPath: "/in", ReadOnly: true}),
		ferrogo.WithMount(ferrogo.Mount{HostPath: rw, GuestPath: "/out"}),
	)
	if err != nil {
		t.Fatalf("%v\n%s", err, res.Stderr)
	}
	if res.Stdout != "read=from host\nro_write=false\nescape=false\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if data, err := os.ReadFile(filepath.Join(rw, "result.txt")); err != nil || string(data) != "FROM HOST" {
		t.Errorf("write through mount: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(ro, "evil.txt")); !os.IsNotExist(err) {
		t.Error("read-only mount was written")
	}
}

func TestWASMRejectsNativeOnlyOptions(t *testing.T) {
	rt := wasmRuntime(t)
	_, err := rt.Run(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.WithDir(t.TempDir()))
	var ue *ferrogo.UnsupportedError
	if !errors.As(err, &ue) || ue.Feature != "working directories" {
		t.Errorf("WithDir on WASM: err = %v", err)
	}
	native := nativeRuntime(t)
	_, err = native.Run(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.WithMount(ferrogo.Mount{HostPath: t.TempDir(), GuestPath: "/x"}))
	if !errors.As(err, &ue) || ue.Feature != "filesystem mounts" {
		t.Errorf("WithMount on native: err = %v", err)
	}
}

func TestWASMConcurrentRuns(t *testing.T) {
	rt := wasmRuntime(t)
	code := ferrogo.Code(`fn main() { let n: u64 = std::env::args().nth(1).unwrap().parse().unwrap(); println!("{}", n * n); }`)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arg := strings.Repeat("1", 1+i%5)
			res, err := rt.Run(context.Background(), code, ferrogo.WithArgs(arg))
			if err == nil {
				var n uint64
				for range arg {
					n = n*10 + 1
				}
				if want := strings.TrimSpace(res.Stdout); want != formatUint(n*n) {
					err = errors.New("wrong result " + want + " for " + arg)
				}
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

func formatUint(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestWASMCancellation(t *testing.T) {
	rt := wasmRuntime(t)
	code := ferrogo.Code(`fn main() { loop { std::hint::black_box(1); } }`)
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

const spinOrFib = `fn main() {
    if std::env::args().nth(1).as_deref() == Some("spin") {
        loop { std::hint::black_box(()); }
    }
    fn fib(n: u64) -> u64 { if n < 2 { n } else { fib(n - 1) + fib(n - 2) } }
    println!("{}", fib(std::hint::black_box(30)));
}`

func TestWASMInterruptionOptOut(t *testing.T) {
	ctx := context.Background()
	untrusted := wasmRuntime(t)
	if _, err := untrusted.Run(ctx, ferrogo.Code(spinOrFib), ferrogo.WithWASMInterruption(false)); !errors.Is(err, ferrogo.ErrUntrusted) {
		t.Fatalf("untrusted opt-out: err = %v", err)
	}
	rt := wasmRuntime(t, ferrogo.WithTrust(ferrogo.Trusted))
	if testWASMBackend == ferrogo.BackendWazy {
		// wazy always checks for interruption, so the opt-out must fail loudly.
		if _, err := rt.Run(ctx, ferrogo.Code(spinOrFib), ferrogo.WithWASMInterruption(false)); !errors.Is(err, ferrogo.ErrUnsupported) {
			t.Fatalf("wazy opt-out: err = %v, want ErrUnsupported", err)
		}
		return
	}
	if _, err := rt.Run(ctx, ferrogo.Code(spinOrFib), ferrogo.WithWASMInterruption(false), ferrogo.WithTimeout(time.Second)); !errors.Is(err, ferrogo.ErrUnsupported) {
		t.Fatalf("explicit timeout without interruption: err = %v", err)
	}

	measure := func(opts ...ferrogo.RunOption) time.Duration {
		t.Helper()
		best := time.Duration(1 << 62)
		for range 3 {
			res, err := rt.Run(ctx, ferrogo.Code(spinOrFib), opts...)
			if err != nil || res.Stdout != "832040\n" {
				t.Fatalf("fib: %v %+v", err, res)
			}
			best = min(best, res.Duration)
		}
		return best
	}
	fast := measure(ferrogo.WithWASMInterruption(false))
	checked := measure()
	t.Logf("fib(30): %s with interruption checks, %s without", checked, fast)
	if fast*2 > checked {
		t.Errorf("opt-out is not faster: %s vs %s", fast, checked)
	}

	// Regression: the uninterruptible compilation above must not leak into
	// the interruptible cache, or this timeout would never fire.
	done := make(chan error, 1)
	go func() {
		_, err := rt.Run(ctx, ferrogo.Code(spinOrFib), ferrogo.WithArgs("spin"), ferrogo.WithTimeout(300*time.Millisecond))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ferrogo.ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timeout did not interrupt: code compiled without checks was reused")
	}
}
