//go:build unix

package process

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// treeMemoryLimits reports whether Spec.MemoryLimit is enforced here.
const treeMemoryLimits = false

// configureCmd places the child in its own process group so that it and any
// descendants can be signalled together.
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// tree is the child's process group.
type tree struct{ pgid int }

func attach(p *os.Process, _ int64) (*tree, error) { return &tree{pgid: p.Pid}, nil }

// terminate asks the process group to exit with SIGTERM.
func (t *tree) terminate() error {
	err := syscall.Kill(-t.pgid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// kill SIGKILLs any process left in the group. A process group ID cannot be
// reused while the group still has members, so this cannot hit an
// unrelated group. ESRCH (group already empty) is the common case.
func (t *tree) kill() error {
	err := syscall.Kill(-t.pgid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	// EPERM can occur on macOS when the group consists only of zombies.
	if errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

func (t *tree) close() error { return nil }

func signalName(state *os.ProcessState) string {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	return ws.Signal().String()
}

func peakRSS(state *os.ProcessState) uint64 {
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || ru.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return uint64(ru.Maxrss) // bytes
	}
	return uint64(ru.Maxrss) * 1024 // kilobytes on Linux and the BSDs
}
