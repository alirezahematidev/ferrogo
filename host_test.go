package ferrogo_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/alirezahematidev/ferrogo"
)

const hostLib = `
ferrogo::import! {
    fn lookup(id: u64) -> String;
    fn log(msg: String);
    fn fail_please() -> u32;
    fn explode() -> u32;
    fn not_registered() -> u32;
}

ferrogo::export! {
    fn greet(id: u64) -> Result<String, String> {
        log(format!("greeting {id}"))?;
        Ok(format!("hello {}", lookup(id)?))
    }
    fn try_fail() -> Result<u32, String> { fail_please() }
    fn try_panic() -> Result<u32, String> { explode() }
    fn try_missing() -> Result<u32, String> { not_registered() }
}
`

type ctxKey struct{}

func TestHostFunctions(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	var sawCtx bool
	rt := wasmRuntime(t,
		ferrogo.WithHostFunction("lookup", func(ctx context.Context, args []ferrogo.Value) (ferrogo.Value, error) {
			id, err := args[0].AsUint64()
			if err != nil {
				return ferrogo.Value{}, err
			}
			mu.Lock()
			sawCtx = ctx.Value(ctxKey{}) == "request-42"
			mu.Unlock()
			return ferrogo.String("user-" + strings.Repeat("x", int(id))), nil
		}),
		ferrogo.WithHostFunction("log", func(_ context.Context, args []ferrogo.Value) (ferrogo.Value, error) {
			s, _ := args[0].AsString()
			mu.Lock()
			logs = append(logs, s)
			mu.Unlock()
			return ferrogo.Void(), nil
		}),
		ferrogo.WithHostFunction("fail_please", func(context.Context, []ferrogo.Value) (ferrogo.Value, error) {
			return ferrogo.Value{}, errors.New("denied by Go")
		}),
		ferrogo.WithHostFunction("explode", func(context.Context, []ferrogo.Value) (ferrogo.Value, error) {
			panic("boom")
		}),
	)
	m, err := rt.Compile(context.Background(), ferrogo.Code(hostLib))
	if err != nil {
		var ce *ferrogo.CompileError
		if errors.As(err, &ce) {
			t.Fatalf("%v\n%s", err, ce.Rendered())
		}
		t.Fatal(err)
	}
	defer m.Close()

	ctx := context.WithValue(context.Background(), ctxKey{}, "request-42")
	v, err := m.Call(ctx, "greet", ferrogo.Uint64(3))
	if s, _ := v.AsString(); err != nil || s != "hello user-xxx" {
		t.Fatalf("greet = %v, %v", v, err)
	}
	if len(logs) != 1 || logs[0] != "greeting 3" || !sawCtx {
		t.Errorf("host side: logs=%v sawCtx=%v", logs, sawCtx)
	}

	for fn, want := range map[string]string{
		"try_fail":    "denied by Go",
		"try_panic":   "host function panicked: boom",
		"try_missing": `unknown host function "not_registered"`,
	} {
		_, err := m.Call(context.Background(), fn)
		var ce *ferrogo.CallError
		if !errors.As(err, &ce) || ce.Message != want {
			t.Errorf("%s: err = %v, want CallError %q", fn, err, want)
		}
	}

	// The module stays healthy after host-side failures.
	if v, err := m.Call(ctx, "greet", ferrogo.Uint64(1)); err != nil || v.String() != `String("hello user-x")` {
		t.Errorf("after failures: %v %v", v, err)
	}

	if ferrogo.CapabilitiesOf(ferrogo.BackendNativeLibrary).Call {
		native := nativeRuntime(t, ferrogo.WithHostFunction("log", func(context.Context, []ferrogo.Value) (ferrogo.Value, error) {
			return ferrogo.Void(), nil
		}))
		if _, err := native.Compile(context.Background(), ferrogo.Code(hostLib), ferrogo.WithBackend(ferrogo.BackendNativeLibrary)); !errors.Is(err, ferrogo.ErrUnsupported) {
			t.Errorf("host functions on native library: err = %v", err)
		}
	}
	if _, err := ferrogo.New(ferrogo.WithHostFunction("not an ident", func(context.Context, []ferrogo.Value) (ferrogo.Value, error) { return ferrogo.Void(), nil })); err == nil {
		t.Error("invalid host function name accepted")
	}
}

func TestHostFunctionsInPrograms(t *testing.T) {
	prog := ferrogo.Code(`
ferrogo::import! { fn greeting(name: String) -> String; }
fn main() {
    match greeting("run".into()) {
        Ok(s) => println!("{s}"),
        Err(e) => println!("error: {e}"),
    }
}`)
	rt := wasmRuntime(t, ferrogo.WithHostFunction("greeting", func(_ context.Context, args []ferrogo.Value) (ferrogo.Value, error) {
		s, _ := args[0].AsString()
		return ferrogo.String("hello " + s), nil
	}))
	res, err := rt.Run(context.Background(), prog)
	if err != nil || res.Stdout != "hello run\n" {
		t.Fatalf("program host call: %v %+v", err, res)
	}
	bare := wasmRuntime(t)
	res, err = bare.Run(context.Background(), prog)
	if err != nil || !strings.Contains(res.Stdout, "no host functions are registered") {
		t.Errorf("unregistered host call: %v %+v", err, res)
	}
}

func TestWASMDeterminism(t *testing.T) {
	prog := ferrogo.Code(`use std::hash::{BuildHasher, Hasher};
fn main() {
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_secs();
    let mut h = std::collections::hash_map::RandomState::new().build_hasher();
    h.write_u64(42);
    println!("{now} {}", h.finish());
}`)
	run := func(opts ...ferrogo.RunOption) string {
		t.Helper()
		rt := wasmRuntime(t)
		res, err := rt.Run(context.Background(), prog, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return res.Stdout
	}
	a, b := run(ferrogo.WithWASMDeterminism(7)), run(ferrogo.WithWASMDeterminism(7))
	if a != b {
		t.Errorf("same seed, different output: %q vs %q", a, b)
	}
	if c := run(ferrogo.WithWASMDeterminism(8)); c == a {
		t.Error("different seeds gave identical randomness")
	}
	if d, e := run(), run(); d == e {
		t.Error("non-deterministic mode is unexpectedly repeatable")
	}
	native := nativeRuntime(t)
	if _, err := native.Run(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.WithWASMDeterminism(1)); !errors.Is(err, ferrogo.ErrUnsupported) {
		t.Errorf("determinism on native: err = %v", err)
	}
}
