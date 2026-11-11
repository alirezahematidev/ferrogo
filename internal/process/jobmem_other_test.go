//go:build !windows

package process

import "errors"

// jobMemoryLimit is only meaningful with Windows Job Objects.
func jobMemoryLimit(*tree) (uint64, error) { return 0, errors.New("no job objects") }
