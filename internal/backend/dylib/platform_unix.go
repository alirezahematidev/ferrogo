//go:build (darwin || linux || netbsd) && !android

package dylib

import "github.com/ebitengine/purego"

// Supported reports whether native libraries can be loaded on this platform.
const Supported = true

func openLibrary(path string) (uintptr, error) {
	// RTLD_LOCAL keeps each library's symbols private, so two FerroGo
	// libraries never resolve each other's ferrogo_* exports.
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func lookup(handle uintptr, name string) (uintptr, error) {
	return purego.Dlsym(handle, name)
}

func closeLibrary(handle uintptr) error {
	return purego.Dlclose(handle)
}

// call invokes a C function taking only integer/pointer arguments.
func call(fn uintptr, args ...uintptr) uintptr {
	r1, _, _ := purego.SyscallN(fn, args...)
	return r1
}
