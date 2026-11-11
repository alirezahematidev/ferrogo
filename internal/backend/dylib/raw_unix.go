//go:build (darwin || linux || netbsd) && !android

package dylib

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"

	"github.com/ebitengine/purego"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// rawCache holds purego functions built for declared signatures.
type rawCache struct {
	declared sync.Map // name+signature -> reflect.Value
}

var kindTypes = map[wire.Kind]reflect.Type{
	wire.Bool: reflect.TypeFor[bool](), wire.Int32: reflect.TypeFor[int32](), wire.Int64: reflect.TypeFor[int64](),
	wire.Uint32: reflect.TypeFor[uint32](), wire.Uint64: reflect.TypeFor[uint64](),
	wire.Float32: reflect.TypeFor[float32](), wire.Float64: reflect.TypeFor[float64](),
}

// CallDeclared calls a plain C-ABI function through a caller-declared
// scalar signature. purego implements the platform calling convention
// (including floating-point registers). A wrong declaration is undefined
// behavior, which is why this is only reachable for trusted code.
func (l *Library) CallDeclared(ctx context.Context, name string, params []wire.Kind, result wire.Kind, args []wire.Value) (wire.Value, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return wire.Value{}, errdefs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return wire.Value{}, err
	}
	key := name + "\x00" + fmt.Sprint(params, result)
	fnVal, ok := l.raw.declared.Load(key)
	if !ok {
		sym := l.symbol(name)
		if sym == 0 || strings.HasPrefix(name, "ferrogo_") {
			return wire.Value{}, fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
		}
		in := make([]reflect.Type, len(params))
		for i, k := range params {
			if in[i], ok = kindTypes[k]; !ok {
				return wire.Value{}, fmt.Errorf("%w: parameter type %s is not a C scalar", errdefs.ErrABIMismatch, k)
			}
		}
		var out []reflect.Type
		if result != wire.Void {
			t, ok := kindTypes[result]
			if !ok {
				return wire.Value{}, fmt.Errorf("%w: result type %s is not a C scalar", errdefs.ErrABIMismatch, result)
			}
			out = []reflect.Type{t}
		}
		ptr := reflect.New(reflect.FuncOf(in, out, false))
		purego.RegisterFunc(ptr.Interface(), sym)
		fnVal, _ = l.raw.declared.LoadOrStore(key, ptr.Elem())
	}
	in := make([]reflect.Value, len(args))
	for i, a := range args {
		v := reflect.New(kindTypes[params[i]]).Elem()
		switch params[i] {
		case wire.Bool:
			v.SetBool(a.Num == 1)
		case wire.Int32, wire.Int64:
			v.SetInt(int64(a.Num))
		case wire.Uint32, wire.Uint64:
			v.SetUint(a.Num)
		case wire.Float32:
			v.SetFloat(float64(math.Float32frombits(uint32(a.Num))))
		case wire.Float64:
			v.SetFloat(math.Float64frombits(a.Num))
		}
		in[i] = v
	}
	out := fnVal.(reflect.Value).Call(in)
	if result == wire.Void {
		return wire.Value{Kind: wire.Void}, nil
	}
	r := out[0]
	res := wire.Value{Kind: result}
	switch result {
	case wire.Bool:
		if r.Bool() {
			res.Num = 1
		}
	case wire.Int32, wire.Int64:
		res.Num = uint64(r.Int())
	case wire.Uint32, wire.Uint64:
		res.Num = r.Uint()
	case wire.Float32:
		res.Num = uint64(math.Float32bits(float32(r.Float())))
	case wire.Float64:
		res.Num = math.Float64bits(r.Float())
	}
	return res, nil
}
