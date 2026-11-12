package ferrogo_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

// libSource is the library used by the module tests. Line numbers matter:
// the panic in boom is on line 18.
const libSource = `use std::sync::atomic::{AtomicU64, Ordering};
static COUNTER: AtomicU64 = AtomicU64::new(0);

ferrogo::export! {
    /// Adds two numbers.
    fn add(a: i32, b: i32) -> i32 { a + b }
    fn greet(name: String) -> Result<String, String> {
        if name.is_empty() { return Err("empty name".into()); }
        Ok(format!("Hello, {name}!"))
    }
    fn describe(a: bool, b: i64, c: u32, d: u64, e: f32, f: f64, g: Vec<u8>) -> String {
        format!("{a} {b} {c} {d} {e} {f} {}", g.len())
    }
    fn reverse(data: Vec<u8>) -> Vec<u8> { let mut d = data; d.reverse(); d }
    fn wrap(doc: ferrogo::Json) -> ferrogo::Json { ferrogo::Json(format!("{{\"inner\":{}}}", doc.0)) }
    fn boom(n: u32) -> u32 {
        if n > 3 {
            panic!("n too big: {n}");
        }
        n
    }
    fn count() -> u64 { COUNTER.fetch_add(1, Ordering::SeqCst) + 1 }
    fn nothing() {}
    fn spin() { loop { std::hint::black_box(()); } }
}

#[no_mangle]
pub extern "C" fn calculate(a: i32, b: i32) -> i32 { a + b }

#[no_mangle]
pub extern "C" fn scale(x: f64) -> f64 { x * 2.5 }
`

type moduleCase struct {
	backend ferrogo.Backend
	rt      *ferrogo.Runtime
}

// moduleCases returns runtimes for every backend that can call functions here.
func moduleCases(t *testing.T) []moduleCase {
	t.Helper()
	cases := []moduleCase{{ferrogo.BackendWASM, wasmRuntime(t)}, {ferrogo.BackendWazy, wasmRuntime(t)}}
	for _, b := range []ferrogo.Backend{ferrogo.BackendNativeLibrary, ferrogo.BackendNativeProcess} {
		if ferrogo.CapabilitiesOf(b).Call {
			cases = append(cases, moduleCase{b, nativeRuntime(t)})
		}
	}
	return cases
}

func compileLib(t *testing.T, c moduleCase, opts ...ferrogo.CompileOption) *ferrogo.Module {
	t.Helper()
	m, err := c.rt.Compile(context.Background(), ferrogo.Code(libSource),
		append([]ferrogo.CompileOption{ferrogo.WithBackend(c.backend)}, opts...)...)
	if err != nil {
		var ce *ferrogo.CompileError
		if errors.As(err, &ce) {
			t.Fatalf("compile: %v\n%s", err, ce.Rendered())
		}
		t.Fatalf("compile: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func mustCall(t *testing.T, m *ferrogo.Module, name string, args ...ferrogo.Value) ferrogo.Value {
	t.Helper()
	v, err := m.Call(context.Background(), name, args...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

func TestModuleCalls(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			ctx := context.Background()

			if n, err := mustCall(t, m, "add", ferrogo.Int32(10), ferrogo.Int32(20)).AsInt32(); err != nil || n != 30 {
				t.Errorf("add = %d, %v", n, err)
			}
			if s, _ := mustCall(t, m, "greet", ferrogo.String("Gö")).AsString(); s != "Hello, Gö!" {
				t.Errorf("greet = %q", s)
			}
			_, err := m.Call(ctx, "greet", ferrogo.String(""))
			var callErr *ferrogo.CallError
			if !errors.As(err, &callErr) || callErr.Message != "empty name" || callErr.Function != "greet" {
				t.Errorf("Err result: %v", err)
			}
			got, _ := mustCall(t, m, "describe", ferrogo.Bool(true), ferrogo.Int64(-7), ferrogo.Uint32(4000000000),
				ferrogo.Uint64(1<<63), ferrogo.Float32(1.5), ferrogo.Float64(-0.25), ferrogo.Bytes([]byte{1, 2, 3})).AsString()
			if got != "true -7 4000000000 9223372036854775808 1.5 -0.25 3" {
				t.Errorf("describe = %q", got)
			}
			big := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18) // 1 MiB round trip
			rev, _ := mustCall(t, m, "reverse", ferrogo.Bytes(big)).AsBytes()
			if len(rev) != len(big) || rev[0] != 4 || rev[len(rev)-1] != 1 {
				t.Errorf("reverse: %d bytes", len(rev))
			}
			doc, err := ferrogo.JSON(map[string]int{"x": 1})
			if err != nil {
				t.Fatal(err)
			}
			var wrapped struct{ Inner map[string]int }
			if err := mustCall(t, m, "wrap", doc).Decode(&wrapped); err != nil || wrapped.Inner["x"] != 1 {
				t.Errorf("wrap: %+v %v", wrapped, err)
			}
			if v := mustCall(t, m, "nothing"); v.Kind() != ferrogo.KindVoid {
				t.Errorf("nothing returned %s", v)
			}
		})
	}
}

func TestModulePanicsAreContained(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			_, err := m.Call(context.Background(), "boom", ferrogo.Uint32(9))
			var p *ferrogo.PanicError
			if !errors.As(err, &p) {
				t.Fatalf("err = %v, want *PanicError", err)
			}
			if p.Message != "n too big: 9" || p.File != "lib.rs" || p.Line != 18 {
				t.Errorf("panic = %+v", p)
			}
			// The module keeps working after a panic.
			if n, _ := mustCall(t, m, "boom", ferrogo.Uint32(2)).AsUint32(); n != 2 {
				t.Errorf("after panic: %d", n)
			}
		})
	}
}

func TestModuleABIMismatches(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			ctx := context.Background()
			bad := map[string][]ferrogo.Value{
				"wrong type":   {ferrogo.Int64(1), ferrogo.Int32(2)},
				"too few":      {ferrogo.Int32(1)},
				"too many":     {ferrogo.Int32(1), ferrogo.Int32(2), ferrogo.Int32(3)},
				"invalid utf8": {ferrogo.String("\xff"), ferrogo.Int32(1)},
			}
			for name, args := range bad {
				if _, err := m.Call(ctx, "add", args...); !errors.Is(err, ferrogo.ErrABIMismatch) {
					t.Errorf("%s: err = %v, want ErrABIMismatch", name, err)
				}
			}
			if _, err := m.Call(ctx, "missing"); !errors.Is(err, ferrogo.ErrFunctionNotFound) {
				t.Errorf("missing function: err = %v", err)
			}
			if _, err := mustCall(t, m, "add", ferrogo.Int32(1), ferrogo.Int32(2)).AsString(); !errors.Is(err, ferrogo.ErrABIMismatch) {
				t.Errorf("wrong accessor: err = %v", err)
			}
		})
	}
}

func TestModuleFunctionsAndInfo(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			fns, err := m.Functions(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sigs := map[string]string{}
			for _, f := range fns {
				sigs[f.Name] = f.Signature
			}
			if sigs["add"] != "fn add(a: i32, b: i32) -> i32" || sigs["nothing"] != "fn nothing()" || len(fns) != 9 {
				t.Errorf("functions = %+v", fns)
			}
			if info := m.Info(); info.Backend != c.backend || info.Key == "" || info.Path == "" {
				t.Errorf("info = %+v", info)
			}
		})
	}
}

func TestModuleRawCCalls(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			v, err := m.Call(context.Background(), "calculate", ferrogo.Int32(10), ferrogo.Int32(20))
			if c.backend != ferrogo.BackendWASM && c.backend != ferrogo.BackendWazy {
				// Native symbols carry no types: raw calls are refused, not guessed.
				if !errors.Is(err, ferrogo.ErrFunctionNotFound) {
					t.Errorf("raw native call: err = %v", err)
				}
				return
			}
			if n, _ := v.AsInt32(); err != nil || n != 30 {
				t.Errorf("calculate = %v, %v", v, err)
			}
			if f, _ := mustCall(t, m, "scale", ferrogo.Float64(4)).AsFloat64(); f != 10 {
				t.Errorf("scale = %v", f)
			}
			if _, err := m.Call(context.Background(), "calculate", ferrogo.Float64(1), ferrogo.Int32(2)); !errors.Is(err, ferrogo.ErrABIMismatch) {
				t.Errorf("raw type mismatch: err = %v", err)
			}
		})
	}
}

func TestModuleConcurrentCalls(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			var wg sync.WaitGroup
			errs := make(chan error, 64)
			for i := range 64 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, err := m.Call(context.Background(), "add", ferrogo.Int32(int32(i)), ferrogo.Int32(1))
					if n, _ := v.AsInt32(); err == nil && n != int32(i)+1 {
						err = errors.New("wrong sum")
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
		})
	}
}

func TestModuleStateSemantics(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m := compileLib(t, c)
			first, _ := mustCall(t, m, "count").AsUint64()
			second, _ := mustCall(t, m, "count").AsUint64()
			// Sequential calls reuse state (one native image; a pooled WASM instance).
			if second != first+1 {
				t.Errorf("count: %d then %d", first, second)
			}
		})
	}
}

func TestWASMModuleTimeoutAndRecovery(t *testing.T) {
	rt := wasmRuntime(t)
	m, err := rt.Compile(context.Background(), ferrogo.Code(libSource), ferrogo.WithTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	start := time.Now()
	if _, err := m.Call(context.Background(), "spin"); !errors.Is(err, ferrogo.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("infinite loop was not interrupted")
	}
	if n, _ := mustCall(t, m, "add", ferrogo.Int32(1), ferrogo.Int32(1)).AsInt32(); n != 2 {
		t.Error("module unusable after timeout")
	}
}

func TestNativeLibraryRefusesUnenforceableOptions(t *testing.T) {
	if !ferrogo.CapabilitiesOf(ferrogo.BackendNativeLibrary).Call {
		t.Skip("native libraries unsupported on this platform")
	}
	rt := nativeRuntime(t)
	opts := map[string]ferrogo.CompileOption{
		"timeout": ferrogo.WithTimeout(time.Second),
		"memory":  ferrogo.WithMemoryLimit(1 << 20),
		"env":     ferrogo.WithEnv(map[string]string{"A": "1"}),
		"mount":   ferrogo.WithMount(ferrogo.Mount{HostPath: t.TempDir(), GuestPath: "/x"}),
	}
	for name, o := range opts {
		_, err := rt.Compile(context.Background(), ferrogo.Code(libSource), ferrogo.WithBackend(ferrogo.BackendNativeLibrary), o)
		if !errors.Is(err, ferrogo.ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
	untrusted := wasmRuntime(t)
	_, err := untrusted.Compile(context.Background(), ferrogo.Code(libSource), ferrogo.WithBackend(ferrogo.BackendNativeLibrary))
	if !errors.Is(err, ferrogo.ErrUntrusted) {
		t.Errorf("untrusted native library: err = %v", err)
	}
}

func TestModuleLifecycleAndLoading(t *testing.T) {
	rt := wasmRuntime(t)
	ctx := context.Background()
	m, err := rt.Compile(ctx, ferrogo.Code(libSource))
	if err != nil {
		t.Fatal(err)
	}
	path := m.Info().Path

	// Prebuilt artifacts load without compiling: from a file and from memory.
	fromFile, err := rt.LoadModule(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	code, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fromMem, err := rt.LoadWASM(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range []*ferrogo.Module{fromFile, fromMem} {
		if n, _ := mustCall(t, mod, "add", ferrogo.Int32(2), ferrogo.Int32(3)).AsInt32(); n != 5 {
			t.Error("loaded module returned wrong result")
		}
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Call(ctx, "add", ferrogo.Int32(1), ferrogo.Int32(1)); !errors.Is(err, ferrogo.ErrClosed) {
		t.Errorf("call after Close: err = %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Runtime.Close closes modules that are still open.
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fromFile.Call(ctx, "add", ferrogo.Int32(1), ferrogo.Int32(1)); !errors.Is(err, ferrogo.ErrClosed) {
		t.Errorf("module outlived its runtime: err = %v", err)
	}
	if _, err := rt.LoadWASM(ctx, []byte("not wasm")); !errors.Is(err, ferrogo.ErrClosed) {
		t.Errorf("load after runtime Close: err = %v", err)
	}
	if !strings.HasSuffix(path, ".wasm") {
		t.Errorf("artifact path %q", path)
	}
}

const workerLib = `
ferrogo::export! {
    fn add(a: i32, b: i32) -> i32 { println!("library output must not corrupt the protocol"); a + b }
    fn spin() { loop { std::hint::black_box(()); } }
    fn crash() -> u32 { std::process::abort() }
    fn env(name: String) -> String { std::env::var(&name).unwrap_or_default() }
    fn connect(addr: String) -> bool { std::net::TcpStream::connect(addr.as_str()).is_ok() }
}
`

func TestNativeProcessModules(t *testing.T) {
	rt := nativeRuntime(t)
	ctx := context.Background()
	t.Setenv("FERROGO_HOST_SECRET", "secret")
	m, err := rt.Compile(ctx, ferrogo.Code(workerLib), ferrogo.WithBackend(ferrogo.BackendNativeProcess),
		ferrogo.WithTimeout(500*time.Millisecond), ferrogo.WithEnv(map[string]string{"GRANTED": "yes"}))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if n, _ := mustCall(t, m, "add", ferrogo.Int32(2), ferrogo.Int32(3)).AsInt32(); n != 5 {
		t.Fatalf("add = %d", n)
	}
	if s, _ := mustCall(t, m, "env", ferrogo.String("GRANTED")).AsString(); s != "yes" {
		t.Errorf("granted env = %q", s)
	}
	if s, _ := mustCall(t, m, "env", ferrogo.String("FERROGO_HOST_SECRET")).AsString(); s != "" {
		t.Error("host environment leaked into the worker")
	}

	// A runaway call is stopped by killing its worker; the module recovers.
	start := time.Now()
	if _, err := m.Call(ctx, "spin"); !errors.Is(err, ferrogo.ErrTimeout) {
		t.Fatalf("spin: err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("runaway native call was not stopped")
	}
	// A hard crash (abort) is contained in the worker; the module recovers.
	if _, err := m.Call(ctx, "crash"); err == nil || !strings.Contains(err.Error(), "crashed") {
		t.Errorf("crash: err = %v", err)
	}
	if n, _ := mustCall(t, m, "add", ferrogo.Int32(1), ferrogo.Int32(1)).AsInt32(); n != 2 {
		t.Error("module unusable after a crash")
	}

	// The OS sandbox applies to workers.
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
	if ok, _ := mustCall(t, m, "connect", ferrogo.String(ln.Addr().String())).AsBool(); !ok {
		t.Fatal("control: unsandboxed worker could not connect")
	}
	sb, err := rt.Compile(ctx, ferrogo.Code(workerLib), ferrogo.WithBackend(ferrogo.BackendNativeProcess),
		ferrogo.WithSandbox(ferrogo.Sandbox{}))
	if errors.Is(err, ferrogo.ErrUnsupported) {
		t.Skipf("no OS sandbox: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if ok, _ := mustCall(t, sb, "connect", ferrogo.String(ln.Addr().String())).AsBool(); ok {
		t.Error("sandboxed worker reached the network")
	}
}

const rawLib = `
#[no_mangle] pub extern "C" fn mix(a: i32, b: f64, c: u64) -> f64 { a as f64 * b + c as f64 }
#[no_mangle] pub extern "C" fn neg(a: i64) -> i64 { -a }
#[no_mangle] pub extern "C" fn flip(b: bool) -> bool { !b }
#[no_mangle] pub extern "C" fn half(x: f32) -> f32 { x / 2.0 }
#[no_mangle] pub extern "C" fn big(x: u32) -> u32 { x.wrapping_mul(2) }
`

func TestCallRawDeclared(t *testing.T) {
	for _, c := range moduleCases(t) {
		t.Run(string(c.backend), func(t *testing.T) {
			m, err := c.rt.Compile(context.Background(), ferrogo.Code(rawLib), ferrogo.WithBackend(c.backend))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			ctx := context.Background()
			call := func(name string, sig ferrogo.Signature, args ...ferrogo.Value) (ferrogo.Value, error) {
				return m.CallRaw(ctx, name, sig, args...)
			}
			mixSig := ferrogo.Signature{Params: []ferrogo.Kind{ferrogo.KindInt32, ferrogo.KindFloat64, ferrogo.KindUint64}, Result: ferrogo.KindFloat64}
			v, err := call("mix", mixSig, ferrogo.Int32(3), ferrogo.Float64(1.5), ferrogo.Uint64(10))
			if c.backend == ferrogo.BackendNativeProcess || (errors.Is(err, ferrogo.ErrUnsupported) && runtime.GOOS == "windows") {
				if !errors.Is(err, ferrogo.ErrUnsupported) {
					t.Errorf("raw call on %s: err = %v, want ErrUnsupported", c.backend, err)
				}
				return
			}
			if f, _ := v.AsFloat64(); err != nil || f != 14.5 {
				t.Fatalf("mix = %v, %v", v, err)
			}
			checks := []struct {
				name string
				sig  ferrogo.Signature
				arg  ferrogo.Value
				want string
			}{
				{"neg", ferrogo.Signature{Params: []ferrogo.Kind{ferrogo.KindInt64}, Result: ferrogo.KindInt64}, ferrogo.Int64(-9), "i64(9)"},
				{"flip", ferrogo.Signature{Params: []ferrogo.Kind{ferrogo.KindBool}, Result: ferrogo.KindBool}, ferrogo.Bool(false), "true"},
				{"half", ferrogo.Signature{Params: []ferrogo.Kind{ferrogo.KindFloat32}, Result: ferrogo.KindFloat32}, ferrogo.Float32(5), "f32(2.5)"},
				{"big", ferrogo.Signature{Params: []ferrogo.Kind{ferrogo.KindUint32}, Result: ferrogo.KindUint32}, ferrogo.Uint32(3_000_000_000), "u32(1705032704)"},
			}
			for _, ch := range checks {
				v, err := call(ch.name, ch.sig, ch.arg)
				if err != nil || v.String() != ch.want {
					t.Errorf("%s = %v, %v; want %s", ch.name, v, err, ch.want)
				}
			}
			if _, err := call("neg", mixSig, ferrogo.Int32(1), ferrogo.Float64(1), ferrogo.Uint64(1)); (c.backend == ferrogo.BackendWASM || c.backend == ferrogo.BackendWazy) && !errors.Is(err, ferrogo.ErrABIMismatch) {
				t.Errorf("wrong declared signature on WASM: err = %v, want ErrABIMismatch", err)
			}
			if _, err := call("neg", ferrogo.Signature{Params: []ferrogo.Kind{ferrogo.KindInt64}, Result: ferrogo.KindInt64}, ferrogo.Int32(1)); !errors.Is(err, ferrogo.ErrABIMismatch) {
				t.Errorf("argument kind differing from the declaration: err = %v", err)
			}
		})
	}
}
