//go:build windows

package dylib

import "syscall"

// Supported reports whether native libraries can be loaded on this platform.
const Supported = true

func openLibrary(path string) (uintptr, error) {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return 0, err
	}
	return uintptr(dll.Handle), nil
}

func lookup(handle uintptr, name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(handle), name)
}

func closeLibrary(handle uintptr) error {
	return syscall.FreeLibrary(syscall.Handle(handle))
}

// call invokes a C function taking only integer/pointer arguments.
func call(fn uintptr, args ...uintptr) uintptr {
	r1, _, _ := syscall.SyscallN(fn, args...)
	return r1
}
