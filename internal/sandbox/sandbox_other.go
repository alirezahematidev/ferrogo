//go:build !darwin && !linux

package sandbox

import "errors"

var errUnavailable = errors.New("no OS sandbox integration on this platform")

// Available reports whether sandboxing works on this machine.
func Available() error { return errUnavailable }

// Command is unavailable on this platform.
func Command(Policy, string, []string) (string, []string, error) {
	return "", nil, errUnavailable
}
