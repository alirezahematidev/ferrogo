// Package dylib loads native shared libraries (.so, .dylib, .dll) built with
// the FerroGo ABI helper into the current process and calls their exports.
//
// Loaded code runs with the full privileges of the Go process and cannot be
// interrupted; it is not a sandbox. Every exported entry point has the same
// machine signature, (const uint8_t*, size_t, fg_buf*) -> uint32_t, so calls
// pass only integers and pointers and need no C compiler (docs/abi.md).
package dylib

import (
	"context"
	"fmt"
	"sync"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

const (
	callPrefix = "ferrogo_v1_call_"
	sigPrefix  = "ferrogo_v1_sig_"
)

// Library is a loaded FerroGo ABI library. It is safe for concurrent use;
// Rust's Send/Sync rules govern the thread safety of the library's own state.
type Library struct {
	path       string
	handle     uintptr
	free       uintptr
	maxPayload int
	functions  []string

	mu      sync.RWMutex // read: calls in flight; write: Close
	closed  bool
	symbols sync.Map // symbol name -> uintptr (0 = missing)
	raw     rawCache // declared-signature functions (platform-specific)
}

// Open loads the library at path (which must be absolute) and verifies that
// it implements FerroGo ABI v1.
func Open(path string, maxPayload int) (*Library, error) {
	if maxPayload <= 0 {
		maxPayload = wire.DefaultMaxPayload
	}
	functions, err := ExportedFunctions(path)
	if err != nil {
		return nil, fmt.Errorf("dylib: reading exports of %s: %w", path, err)
	}
	handle, err := openLibrary(path)
	if err != nil {
		return nil, fmt.Errorf("dylib: loading %s: %w", path, err)
	}
	l := &Library{path: path, handle: handle, maxPayload: maxPayload, functions: functions}
	fail := func(err error) (*Library, error) {
		_ = closeLibrary(handle) // already failing; report the original error
		return nil, err
	}
	version, err := lookup(handle, "ferrogo_abi_version")
	if err != nil {
		return fail(fmt.Errorf("%w: %s does not export ferrogo_abi_version; export functions with the ferrogo::export! macro", errdefs.ErrABIMismatch, path))
	}
	if v := uint32(call(version)); v != 1 {
		return fail(fmt.Errorf("%w: library uses FerroGo ABI v%d; this FerroGo supports v1", errdefs.ErrABIMismatch, v))
	}
	if l.free, err = lookup(handle, "ferrogo_free"); err != nil {
		return fail(fmt.Errorf("%w: library does not export ferrogo_free", errdefs.ErrABIMismatch))
	}
	return l, nil
}

func (l *Library) symbol(name string) uintptr {
	if v, ok := l.symbols.Load(name); ok {
		return v.(uintptr)
	}
	addr, err := lookup(l.handle, name)
	if err != nil {
		addr = 0
	}
	l.symbols.Store(name, addr)
	return addr
}

// Functions lists the library's FerroGo ABI functions.
func (l *Library) Functions() []string { return l.functions }

// Call invokes ferrogo_v1_call_<name>. The context is only checked before
// the call starts: native code cannot be interrupted.
func (l *Library) Call(ctx context.Context, name string, input []byte) (uint32, []byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return 0, nil, errdefs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	fn := l.symbol(callPrefix + name)
	if fn == 0 {
		return 0, nil, fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
	}
	return invoke(fn, l.free, input, l.maxPayload)
}

// Signature returns the Rust signature of an exported function.
func (l *Library) Signature(ctx context.Context, name string) (string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return "", errdefs.ErrClosed
	}
	fn := l.symbol(sigPrefix + name)
	if fn == 0 {
		return "", fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
	}
	_, out, err := invokeOut(fn, l.free, l.maxPayload)
	return string(out), err
}

// Close waits for in-flight calls, then unloads the library. The OS may keep
// the code mapped (macOS never unloads images with thread-local destructors,
// which Rust's std registers); further calls through this Library fail with
// ErrClosed either way.
func (l *Library) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if err := closeLibrary(l.handle); err != nil {
		return fmt.Errorf("dylib: unloading %s: %w", l.path, err)
	}
	return nil
}
