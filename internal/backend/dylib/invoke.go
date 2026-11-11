package dylib

import (
	"bytes"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
)

// fgBuf mirrors the Rust #[repr(C)] struct Buf { ptr: *mut u8, len: usize }.
//
// Rust writes a pointer to memory owned by the library's allocator into ptr.
// Holding a non-Go pointer in an unsafe.Pointer field is permitted: the
// garbage collector ignores pointers outside the Go heap.
type fgBuf struct {
	ptr unsafe.Pointer
	len uintptr
}

// invoke calls fn(in, len, out) and returns the status and a Go-owned copy
// of the result, which it then frees through the library's allocator.
//
// Memory rules (docs/abi.md §6): input is Go memory pinned for the duration
// of the call and must not be retained by Rust; the result buffer is owned
// by Rust until ferrogo_free.
func invoke(fn, free uintptr, input []byte, maxPayload int) (uint32, []byte, error) {
	var pinner runtime.Pinner
	defer pinner.Unpin()

	out := new(fgBuf)
	pinner.Pin(out)
	var in uintptr
	if len(input) > 0 {
		pinner.Pin(&input[0])
		in = uintptr(unsafe.Pointer(&input[0]))
	}
	status := uint32(call(fn, in, uintptr(len(input)), uintptr(unsafe.Pointer(out))))
	data, err := takeResult(out, free, maxPayload)
	return status, data, err
}

// invokeOut calls fn(out), the signature of ferrogo_v1_sig_* functions.
func invokeOut(fn, free uintptr, maxPayload int) (uint32, []byte, error) {
	var pinner runtime.Pinner
	defer pinner.Unpin()
	out := new(fgBuf)
	pinner.Pin(out)
	status := uint32(call(fn, uintptr(unsafe.Pointer(out))))
	data, err := takeResult(out, free, maxPayload)
	return status, data, err
}

// takeResult copies and frees the buffer described by out.
func takeResult(out *fgBuf, free uintptr, maxPayload int) ([]byte, error) {
	if out.ptr == nil {
		return nil, nil
	}
	defer call(free, uintptr(out.ptr), out.len)
	if out.len > uintptr(maxPayload) {
		return nil, fmt.Errorf("%w: result is %d bytes; the limit is %d", errdefs.ErrABIMismatch, out.len, maxPayload)
	}
	if out.len == 0 {
		return []byte{}, nil
	}
	return bytes.Clone(unsafe.Slice((*byte)(out.ptr), out.len)), nil
}
