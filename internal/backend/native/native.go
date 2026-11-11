// Package native executes compiled Rust programs as operating-system
// processes, and serves function calls from worker processes. It is not a
// sandbox unless an OS sandbox is requested: programs run with the
// privileges of the host user. See docs/security.md.
package native

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/alirezahematidev/ferrogo/internal/backend"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/process"
	"github.com/alirezahematidev/ferrogo/internal/sandbox"
)

// Runner runs native executables.
type Runner struct{}

var _ backend.Runner = Runner{}

// Run executes spec.Artifact directly (no shell) with exactly spec.Env.
func (Runner) Run(ctx context.Context, spec backend.RunSpec) (*backend.RunResult, error) {
	dir := spec.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "ferrogo-run-")
		if err != nil {
			return nil, fmt.Errorf("native: creating working directory: %w", err)
		}
		defer os.RemoveAll(tmp) // best effort: the program may have left read-only files
		dir = tmp
	}
	path, args, jobMemory, err := command(spec.Artifact, spec.Args, dir, spec.MemoryLimit, spec.Sandbox)
	if err != nil {
		return nil, err
	}
	res, err := process.Run(ctx, process.Spec{
		Path: path, Args: args, Env: spec.Env, Dir: dir,
		Stdin: spec.Stdin, Stdout: spec.Stdout, Stderr: spec.Stderr,
		MaxOutput: spec.MaxOutput, Timeout: spec.Timeout, GracePeriod: spec.GracePeriod,
		MemoryLimit: jobMemory,
	})
	if res == nil {
		return nil, err
	}
	out := &backend.RunResult{
		Stdout: res.Stdout, Stderr: res.Stderr,
		ExitCode: res.ExitCode, Signal: res.Signal,
		Duration: res.Duration, UserTime: res.UserTime, SystemTime: res.SystemTime,
		PeakMemory: res.PeakRSS,
	}
	if err == nil && spec.MemoryLimit > 0 && res.ExitCode != 0 && memoryExhausted(res.Stderr) {
		err = fmt.Errorf("%w: the program is limited to %d bytes", errdefs.ErrMemoryLimit, spec.MemoryLimit)
	}
	return out, err
}

// Close is a no-op; native processes hold no shared resources.
func (Runner) Close() error { return nil }

// memoryExhausted recognises Rust's allocation-failure abort message.
func memoryExhausted(stderr []byte) bool {
	return bytes.Contains(stderr, []byte("memory allocation of "))
}

// command builds the command line for running program with args in dir,
// applying the memory limit (prlimit on Linux; a Job Object limit, returned
// as jobMemory, on Windows) inside the OS sandbox (if requested).
// readable lists extra files the sandboxed program must read.
func command(program string, args []string, dir string, memoryLimit int64, sb *backend.Sandbox, readable ...string) (string, []string, int64, error) {
	path := program
	var jobMemory int64
	if memoryLimit > 0 {
		switch runtime.GOOS {
		case "windows":
			jobMemory = memoryLimit
		case "linux":
			// prlimit sets RLIMIT_AS on itself and then execs the program,
			// so the limit is in place before the program's first instruction.
			prlimit, err := exec.LookPath("prlimit")
			if err != nil {
				return "", nil, 0, fmt.Errorf("%w: memory limits need prlimit (util-linux)", errdefs.ErrUnsupported)
			}
			args = append([]string{fmt.Sprintf("--as=%d", memoryLimit), "--", path}, args...)
			path = prlimit
		default:
			return "", nil, 0, fmt.Errorf("%w: memory limits for native processes on %s", errdefs.ErrUnsupported, runtime.GOOS)
		}
	}
	if sb != nil {
		policy := sandbox.Policy{
			AllowNetwork: sb.AllowNetwork, Dir: dir,
			Writable: append([]string{dir}, sb.Writable...),
			Readable: append(append([]string{program}, readable...), sb.Readable...),
		}
		var err error
		if path, args, err = sandbox.Command(policy, path, args); err != nil {
			return "", nil, 0, fmt.Errorf("%w: OS sandbox: %v", errdefs.ErrUnsupported, err)
		}
	}
	return path, args, jobMemory, nil
}
