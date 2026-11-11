// Package errdefs defines the sentinel errors shared by FerroGo's internal
// packages. The public ferrogo package re-exports these exact values, so
// errors.Is works across package boundaries without translation.
package errdefs

import "errors"

var (
	// ErrToolchainNotFound reports that no usable Rust toolchain was found.
	ErrToolchainNotFound = errors.New("ferrogo: Rust toolchain not found")
	// ErrTargetNotInstalled reports that a required compilation target is missing.
	ErrTargetNotInstalled = errors.New("ferrogo: compilation target not installed")
	// ErrCompilation reports that Rust compilation failed.
	ErrCompilation = errors.New("ferrogo: compilation failed")
	// ErrTimeout reports that compilation or execution exceeded its time limit.
	ErrTimeout = errors.New("ferrogo: time limit exceeded")
	// ErrOutputLimit reports that a program wrote more output than allowed.
	ErrOutputLimit = errors.New("ferrogo: output limit exceeded")
	// ErrMemoryLimit reports that guest code exceeded its memory limit.
	ErrMemoryLimit = errors.New("ferrogo: memory limit exceeded")
	// ErrUnsupported reports that a backend does not support an operation.
	ErrUnsupported = errors.New("ferrogo: unsupported by backend")
	// ErrUntrusted reports that the trust policy forbids an operation.
	ErrUntrusted = errors.New("ferrogo: forbidden by trust policy")
	// ErrABIMismatch reports a type or protocol mismatch at the Go/Rust boundary.
	ErrABIMismatch = errors.New("ferrogo: ABI mismatch")
	// ErrFunctionNotFound reports that a module does not export a function.
	ErrFunctionNotFound = errors.New("ferrogo: function not found")
	// ErrClosed reports use of a closed Runtime or Module.
	ErrClosed = errors.New("ferrogo: closed")
	// ErrDependencyPolicy reports that the dependency policy forbids a dependency.
	ErrDependencyPolicy = errors.New("ferrogo: forbidden by dependency policy")
)
