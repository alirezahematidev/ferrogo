package ferrogo

import (
	"context"
	"fmt"
	"maps"
	"regexp"

	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// HostFunc is a Go function that WASM modules can call. Rust declares it
// with ferrogo::import! and calls it like a function returning
// Result<T, String>:
//
//	ferrogo::import! {
//	    fn lookup_user(id: u64) -> String;
//	}
//
// args holds the arguments with the kinds Rust passed; the returned Value
// must have the kind Rust expects (Void for no return value). A returned
// error reaches Rust as Err(err.Error()). The context is the context of
// the Module.Call that led to the host call.
type HostFunc func(ctx context.Context, args []Value) (Value, error)

var rustIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// WithHostFunction exposes fn to WASM modules under name. Host functions
// are capabilities you grant explicitly: WASM code can call nothing else.
// The native-library backend rejects them with ErrUnsupported.
func WithHostFunction(name string, fn HostFunc) SharedOption {
	return sharedOption(func(c *config) error {
		if !rustIdent.MatchString(name) {
			return fmt.Errorf("ferrogo: host function name %q is not a Rust identifier", name)
		}
		if fn == nil {
			return fmt.Errorf("ferrogo: host function %s is nil", name)
		}
		c.hostFuncs = maps.Clone(c.hostFuncs)
		if c.hostFuncs == nil {
			c.hostFuncs = map[string]HostFunc{}
		}
		c.hostFuncs[name] = fn
		return nil
	})
}

// hostDispatcher adapts registered host functions to the wire format.
func hostDispatcher(funcs map[string]HostFunc) func(ctx context.Context, name string, args []byte) ([]byte, error) {
	if len(funcs) == 0 {
		return nil
	}
	return func(ctx context.Context, name string, input []byte) ([]byte, error) {
		fn, ok := funcs[name]
		if !ok {
			return nil, fmt.Errorf("unknown host function %q", name)
		}
		raw, err := wire.DecodeArgs(input, wire.DefaultMaxPayload)
		if err != nil {
			return nil, fmt.Errorf("host function %s: %w", name, err)
		}
		args := make([]Value, len(raw))
		for i, v := range raw {
			args[i] = Value{v}
		}
		out, err := fn(ctx, args)
		if err != nil {
			return nil, err
		}
		if err := out.validate(); err != nil {
			return nil, err
		}
		return wire.EncodeValue(out.v)
	}
}
