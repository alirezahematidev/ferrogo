//go:build unix

package process

import (
	"errors"
	"syscall"
)

func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
