//go:build !((darwin || linux || netbsd) && !android)

package dylib

import (
	"context"
	"fmt"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// rawCache is empty where declared-signature calls are unavailable: the
// standard library's syscall interface cannot pass floating-point
// arguments, and FerroGo does not guess calling conventions.
type rawCache struct{}

func (rawCache) unavailable() error {
	return fmt.Errorf("%w: raw C calls with declared signatures on this platform", errdefs.ErrUnsupported)
}

// CallDeclared is not available on this platform.
func (l *Library) CallDeclared(context.Context, string, []wire.Kind, wire.Kind, []wire.Value) (wire.Value, error) {
	return wire.Value{}, l.raw.unavailable()
}
