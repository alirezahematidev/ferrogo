package ferrogo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/alirezahematidev/ferrogo/internal/backend"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// Module is a loaded Rust library whose exported functions can be called
// repeatedly. Create one with Runtime.Compile, Runtime.LoadModule or
// Runtime.LoadWASM, and release it with Close. A Module is safe for
// concurrent use.
//
// Functions exported with ferrogo::export! accept and return any Value kind,
// report Result::Err as *CallError and panics as *PanicError, and behave the
// same on every backend. On the WASM backend, plain #[no_mangle] extern "C"
// functions with scalar parameters can be called too (see docs/abi.md).
//
// State differs by backend: a WASM module runs calls in a pool of instances
// with separate memories, so Rust statics are per instance and an instance
// is discarded after a panic; a native library has one shared state.
type Module struct {
	rt         *Runtime
	impl       backend.Module
	info       ModuleInfo
	loc        locator
	maxPayload int
	envelope   map[string]bool

	closeOnce sync.Once
	closeErr  error
}

// ModuleInfo describes a loaded module.
type ModuleInfo struct {
	Backend Backend `json:"backend"`
	// Target is the Rust target triple (empty for prebuilt artifacts).
	Target string `json:"target,omitempty"`
	// Path is the artifact file (empty for LoadWASM).
	Path string `json:"path,omitempty"`
	// Key is the artifact cache key (empty for prebuilt artifacts).
	Key string `json:"key,omitempty"`
}

// Function describes an exported function.
type Function struct {
	Name string `json:"name"`
	// Signature is the Rust signature, e.g. "fn add(a: i32, b: i32) -> i32".
	Signature string `json:"signature"`
}

// CallError reports that a Rust function returned Err(e).
type CallError struct {
	Function string
	// Message is e.to_string().
	Message string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("ferrogo: %s returned an error: %s", e.Function, e.Message)
}

func newModule(rt *Runtime, impl backend.Module, info ModuleInfo, loc locator, maxPayload int) *Module {
	m := &Module{rt: rt, impl: impl, info: info, loc: loc, maxPayload: maxPayload, envelope: map[string]bool{}}
	for _, name := range impl.Functions() {
		m.envelope[name] = true
	}
	return m
}

// Info describes the module.
func (m *Module) Info() ModuleInfo { return m.info }

// Functions lists the functions exported with ferrogo::export!, with their
// Rust signatures.
func (m *Module) Functions(ctx context.Context) ([]Function, error) {
	names := m.impl.Functions()
	out := make([]Function, 0, len(names))
	for _, name := range names {
		sig, err := m.impl.Signature(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("ferrogo: signature of %s: %w", name, err)
		}
		out = append(out, Function{Name: name, Signature: sig})
	}
	return out, nil
}

// Call invokes an exported function. Argument kinds must match the Rust
// parameter types exactly (no implicit conversions); mismatches fail with
// ErrABIMismatch. The context bounds the call on the WASM backend (together
// with WithTimeout); native-library calls can only be cancelled before they
// start.
func (m *Module) Call(ctx context.Context, name string, args ...Value) (Value, error) {
	for i, a := range args {
		if err := a.validate(); err != nil {
			return Value{}, fmt.Errorf("argument %d of %s: %w", i, name, err)
		}
	}
	if m.envelope[name] {
		return m.callEnvelope(ctx, name, args)
	}
	if raw, ok := m.impl.(backend.RawCaller); ok {
		if params, results, ok := raw.RawSignature(name); ok {
			return m.callRaw(ctx, raw, name, params, results, args)
		}
	}
	hint := "export it with the ferrogo::export! macro"
	if fns := m.impl.Functions(); len(fns) > 0 {
		hint = "available: " + strings.Join(fns, ", ")
	}
	return Value{}, fmt.Errorf("%w: %s (%s)", ErrFunctionNotFound, name, hint)
}

func (m *Module) callEnvelope(ctx context.Context, name string, args []Value) (Value, error) {
	wargs := make([]wire.Value, len(args))
	for i, a := range args {
		wargs[i] = a.v
	}
	input, err := wire.EncodeArgs(wargs, m.maxPayload)
	if err != nil {
		return Value{}, fmt.Errorf("%w: %s: %v", ErrABIMismatch, name, err)
	}
	status, out, err := m.impl.Call(ctx, name, input)
	if err != nil {
		return Value{}, err
	}
	switch status {
	case wire.StatusOK:
		v, err := wire.DecodeValue(out, m.maxPayload)
		if err != nil {
			return Value{}, fmt.Errorf("%w: %s returned %v", ErrABIMismatch, name, err)
		}
		return Value{v}, nil
	case wire.StatusError:
		return Value{}, &CallError{Function: name, Message: string(out)}
	case wire.StatusPanic:
		return Value{}, m.panicError(string(out))
	case wire.StatusABI:
		return Value{}, fmt.Errorf("%w: %s: %s", ErrABIMismatch, name, out)
	}
	return Value{}, fmt.Errorf("%w: %s returned unknown status %d", ErrABIMismatch, name, status)
}

// panicError parses "file:line:col\x1fmessage" from the ABI helper.
func (m *Module) panicError(s string) *PanicError {
	loc, msg, ok := strings.Cut(s, "\x1f")
	if !ok {
		return &PanicError{Message: s}
	}
	p := &PanicError{Message: msg}
	parts := strings.Split(loc, ":")
	if len(parts) >= 3 {
		line, err1 := strconv.Atoi(parts[len(parts)-2])
		col, err2 := strconv.Atoi(parts[len(parts)-1])
		if err1 == nil && err2 == nil {
			p.File, p.Line, p.Column = strings.Join(parts[:len(parts)-2], ":"), line, col
		}
	}
	p.File, p.Line, p.Column = m.loc.locate(p.File, p.Line, p.Column)
	return p
}

func (m *Module) callRaw(ctx context.Context, raw backend.RawCaller, name string, params, results []backend.RawType, args []Value) (Value, error) {
	if len(args) != len(params) {
		return Value{}, fmt.Errorf("%w: %s takes %d argument(s), got %d", ErrABIMismatch, name, len(params), len(args))
	}
	in := make([]uint64, len(args))
	for i, a := range args {
		n := a.v.Num
		ok := false
		switch params[i] {
		case backend.RawI32:
			ok = a.Kind() == KindInt32 || a.Kind() == KindUint32 || a.Kind() == KindBool
			n = uint64(uint32(n))
		case backend.RawI64:
			ok = a.Kind() == KindInt64 || a.Kind() == KindUint64
		case backend.RawF32:
			ok = a.Kind() == KindFloat32
		case backend.RawF64:
			ok = a.Kind() == KindFloat64
		}
		if !ok {
			return Value{}, fmt.Errorf("%w: argument %d of %s is %s; the export takes %s",
				ErrABIMismatch, i, name, a.Kind(), rawTypeName(params[i]))
		}
		in[i] = n
	}
	out, err := raw.CallRaw(ctx, name, in)
	if err != nil {
		return Value{}, err
	}
	switch len(results) {
	case 0:
		return Void(), nil
	case 1:
		r := out[0]
		switch results[0] {
		case backend.RawI32:
			return Int32(int32(uint32(r))), nil
		case backend.RawI64:
			return Int64(int64(r)), nil
		case backend.RawF32:
			return Float32(math.Float32frombits(uint32(r))), nil
		case backend.RawF64:
			return Float64(math.Float64frombits(r)), nil
		}
	}
	return Value{}, &UnsupportedError{Backend: m.info.Backend, Feature: "multi-value or reference-typed results"}
}

func rawTypeName(t backend.RawType) string {
	switch t {
	case backend.RawI32:
		return "i32 (Int32, Uint32 or Bool)"
	case backend.RawI64:
		return "i64 (Int64 or Uint64)"
	case backend.RawF32:
		return "f32 (Float32)"
	case backend.RawF64:
		return "f64 (Float64)"
	}
	return fmt.Sprintf("type %#x", byte(t))
}

// Close waits for in-flight calls and unloads the module. It is safe to
// call more than once; later calls fail with ErrClosed.
func (m *Module) Close() error {
	m.closeOnce.Do(func() {
		m.closeErr = m.impl.Close()
		if m.rt != nil {
			m.rt.forgetModule(m)
		}
	})
	return m.closeErr
}

// errNotLibrary is returned when LoadModule is given something else.
var errNotLibrary = errors.New("ferrogo: not a WebAssembly module or native shared library")

// Signature declares the C signature of a plain `#[no_mangle] extern "C"`
// function for Module.CallRaw. Params and Result use the scalar kinds
// (KindBool, KindInt32, KindInt64, KindUint32, KindUint64, KindFloat32,
// KindFloat64); Result may be KindVoid.
type Signature struct {
	Params []Kind
	Result Kind
}

func (s Signature) String() string {
	parts := make([]string, len(s.Params))
	for i, k := range s.Params {
		parts[i] = k.String()
	}
	return "extern \"C\" fn(" + strings.Join(parts, ", ") + ") -> " + s.Result.String()
}

// declaredCaller is implemented by native libraries.
type declaredCaller interface {
	CallDeclared(ctx context.Context, name string, params []wire.Kind, result wire.Kind, args []wire.Value) (wire.Value, error)
}

// CallRaw calls a plain `#[no_mangle] extern "C"` function with scalar
// arguments, using a signature you declare.
//
// On WASM the declaration is checked against the module's export, so a
// mismatch fails with ErrABIMismatch. A native shared library carries no
// type information: the declaration is trusted, and a wrong one is
// undefined behavior (native libraries already require WithTrust(Trusted)).
// Raw calls are not available on the native-process backend, or for native
// libraries on Windows.
func (m *Module) CallRaw(ctx context.Context, name string, sig Signature, args ...Value) (Value, error) {
	scalar := func(k Kind) bool { return k >= KindBool && k <= KindFloat64 }
	if len(args) != len(sig.Params) {
		return Value{}, fmt.Errorf("%w: %s declares %d parameter(s), got %d argument(s)", ErrABIMismatch, name, len(sig.Params), len(args))
	}
	for i, k := range sig.Params {
		if !scalar(k) {
			return Value{}, fmt.Errorf("%w: parameter %d of %s: %s is not a C scalar", ErrABIMismatch, i, name, k)
		}
		if args[i].Kind() != k {
			return Value{}, fmt.Errorf("%w: argument %d of %s is %s; the declaration says %s", ErrABIMismatch, i, name, args[i].Kind(), k)
		}
	}
	if sig.Result != KindVoid && !scalar(sig.Result) {
		return Value{}, fmt.Errorf("%w: result of %s: %s is not a C scalar", ErrABIMismatch, name, sig.Result)
	}
	switch impl := m.impl.(type) {
	case backend.RawCaller:
		params, results, ok := impl.RawSignature(name)
		if !ok {
			return Value{}, fmt.Errorf("%w: %s", ErrFunctionNotFound, name)
		}
		core := func(k Kind) backend.RawType {
			switch k {
			case KindInt64, KindUint64:
				return backend.RawI64
			case KindFloat32:
				return backend.RawF32
			case KindFloat64:
				return backend.RawF64
			}
			return backend.RawI32
		}
		match := len(params) == len(sig.Params) && (len(results) == 0) == (sig.Result == KindVoid) && len(results) <= 1
		for i := 0; match && i < len(params); i++ {
			match = params[i] == core(sig.Params[i])
		}
		if match && len(results) == 1 {
			match = results[0] == core(sig.Result)
		}
		if !match {
			return Value{}, fmt.Errorf("%w: %s does not match the module's export", ErrABIMismatch, sig)
		}
		in := make([]uint64, len(args))
		for i, a := range args {
			in[i] = a.v.Num
			if core(sig.Params[i]) != backend.RawI64 && core(sig.Params[i]) != backend.RawF64 {
				in[i] = uint64(uint32(a.v.Num))
			}
		}
		out, err := impl.CallRaw(ctx, name, in)
		if err != nil {
			return Value{}, err
		}
		if sig.Result == KindVoid {
			return Void(), nil
		}
		r := out[0]
		switch sig.Result {
		case KindBool:
			return Bool(uint32(r) != 0), nil
		case KindInt32:
			return Int32(int32(uint32(r))), nil
		case KindUint32:
			return Uint32(uint32(r)), nil
		case KindFloat32:
			return Float32(math.Float32frombits(uint32(r))), nil
		}
		return Value{wire.Value{Kind: wire.Kind(sig.Result), Num: r}}, nil
	case declaredCaller:
		params := make([]wire.Kind, len(sig.Params))
		for i, k := range sig.Params {
			params[i] = wire.Kind(k)
		}
		wargs := make([]wire.Value, len(args))
		for i, a := range args {
			wargs[i] = a.v
		}
		v, err := impl.CallDeclared(ctx, name, params, wire.Kind(sig.Result), wargs)
		return Value{v}, err
	}
	return Value{}, &UnsupportedError{Backend: m.info.Backend, Feature: "raw C calls", Hint: "export the function with ferrogo::export!"}
}
