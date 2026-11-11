//go:build windows

package process

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// treeMemoryLimits reports whether Spec.MemoryLimit is enforced here.
const treeMemoryLimits = true

// configureCmd starts the child in a new process group so console control
// events sent to the parent do not reach it.
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// tree is a Job Object holding the child and every process it creates.
// Closing it kills them all (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE).
//
// The child is assigned right after it starts, so a program could create a
// process in the first moments of its life before the assignment; such a
// process escapes the job. Programs compiled by FerroGo do not do this.
type tree struct{ job windows.Handle }

func attach(p *os.Process, memoryLimit int64) (*tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("creating job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if memoryLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(memoryLimit)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configuring job object: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("opening process: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("assigning process to job object: %w", err)
	}
	return &tree{job: job}, nil
}

// terminate kills the whole tree. Windows has no portable graceful-exit
// signal for console programs without a shared console.
func (t *tree) terminate() error { return t.kill() }

func (t *tree) kill() error {
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return fmt.Errorf("terminating job object: %w", err)
	}
	return nil
}

func (t *tree) close() error { return windows.CloseHandle(t.job) }

func signalName(*os.ProcessState) string { return "" }

// peakRSS is not available from os.ProcessState on Windows.
func peakRSS(*os.ProcessState) uint64 { return 0 }
