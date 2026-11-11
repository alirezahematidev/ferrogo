//go:build !((darwin || linux || netbsd) && !android) && !windows

package dylib

import "errors"

// errUnsupportedPlatform is returned on platforms without a loader. FreeBSD
// is excluded because purego's cgo-free mode does not build there.
var errUnsupportedPlatform = errors.New("dylib: native libraries are not supported on this platform")

// Supported reports whether native libraries can be loaded on this platform.
const Supported = false

func openLibrary(string) (uintptr, error)      { return 0, errUnsupportedPlatform }
func lookup(uintptr, string) (uintptr, error)  { return 0, errUnsupportedPlatform }
func closeLibrary(uintptr) error               { return errUnsupportedPlatform }
func call(fn uintptr, args ...uintptr) uintptr { panic(errUnsupportedPlatform) }
