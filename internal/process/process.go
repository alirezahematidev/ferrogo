// Package process runs external programs safely: arguments are passed as an
// argv vector (never through a shell), the environment is always explicit,
// output is captured with a hard limit, and cancellation terminates the whole
// process group gracefully before killing it.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/outcap"
)

// DefaultGracePeriod is how long a terminated process may take to exit
// before it is forcibly killed.
const DefaultGracePeriod = 2 * time.Second

// Spec describes a process to run.
type Spec struct {
	// Path is the executable. It is executed directly, never via a shell.
	Path string
	// Args are the arguments after argv[0].
	Args []string
	// Env is the complete environment. A nil Env means an empty environment;
	// the parent environment is never inherited implicitly.
	Env []string
	// Dir is the working directory. Empty means the caller's directory.
	Dir string
	// Stdin is connected to the process's standard input. Nil means the null device.
	Stdin io.Reader
	// Stdout and Stderr, when non-nil, receive a live copy of captured output.
	// Writes beyond MaxOutput are not forwarded.
	Stdout, Stderr io.Writer
	// MaxOutput bounds the combined captured stdout+stderr in bytes. When it
	// is exceeded the process is terminated. Zero means unlimited.
	MaxOutput int64
	// Timeout bounds the wall-clock run time. Zero means no timeout beyond ctx.
	Timeout time.Duration
	// GracePeriod is the time between graceful termination and a forced kill.
	// Zero means DefaultGracePeriod.
	GracePeriod time.Duration
	// MemoryLimit bounds the memory of the process tree in bytes (0 = none).
	// It is enforced with a Job Object on Windows; elsewhere Run rejects it
	// (callers wrap the program with prlimit on Linux).
	MemoryLimit int64
}

// Result describes a finished process.
type Result struct {
	Stdout, Stderr []byte
	// ExitCode is the exit status, or -1 if the process was killed by a signal.
	ExitCode int
	// Signal names the terminating signal on Unix, or is empty.
	Signal     string
	Duration   time.Duration
	UserTime   time.Duration
	SystemTime time.Duration
	// PeakRSS is the peak resident set size in bytes, or 0 if unknown.
	PeakRSS uint64
}

// Run executes spec and waits for it to finish.
//
// A non-zero exit status is not an error: inspect Result.ExitCode. Run
// returns a nil Result only if the process could not be started. When the
// process was terminated because of a timeout, an output-limit violation, or
// cancellation of ctx, Run returns the partial Result together with an error
// matching errdefs.ErrTimeout, errdefs.ErrOutputLimit, or ctx.Err().
func Run(ctx context.Context, spec Spec) (*Result, error) {
	if spec.Path == "" {
		return nil, errors.New("process: empty executable path")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	runCtx, cancelCause := context.WithCancelCause(runCtx)
	defer cancelCause(nil)

	capture := outcap.New(spec.MaxOutput, func() { cancelCause(errdefs.ErrOutputLimit) })
	stdout := capture.Stream(spec.Stdout)
	stderr := capture.Stream(spec.Stderr)

	cmd := exec.CommandContext(runCtx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // never inherit the parent environment
	}
	cmd.Dir = spec.Dir
	cmd.Stdin = spec.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	grace := spec.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	var terminated atomic.Bool
	var group atomic.Pointer[tree] // set right after Start
	configureCmd(cmd)
	cmd.Cancel = func() error {
		terminated.Store(true)
		if t := group.Load(); t != nil {
			return t.terminate()
		}
		return cmd.Process.Kill()
	}
	// After the grace period exec kills the process leader and closes pipes
	// that leaked to grandchildren; killGroup below handles the rest.
	cmd.WaitDelay = grace

	if spec.MemoryLimit > 0 && !treeMemoryLimits {
		return nil, errors.New("process: memory limits are not supported on this platform")
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process: start %s: %w", spec.Path, err)
	}
	t, err := attach(cmd.Process, spec.MemoryLimit)
	if err != nil {
		_ = cmd.Process.Kill() // the process must not run outside its limits
		_ = cmd.Wait()
		return nil, fmt.Errorf("process: %w", err)
	}
	group.Store(t)
	waitErr := cmd.Wait()
	duration := time.Since(start)
	// Kill anything the program left behind, then release the group.
	if err := t.kill(); err != nil {
		waitErr = errors.Join(waitErr, fmt.Errorf("process: kill process tree: %w", err))
	}
	if err := t.close(); err != nil {
		waitErr = errors.Join(waitErr, fmt.Errorf("process: %w", err))
	}

	state := cmd.ProcessState
	if state == nil {
		return nil, fmt.Errorf("process: wait %s: %w", spec.Path, waitErr)
	}
	res := &Result{
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		ExitCode:   state.ExitCode(),
		Signal:     signalName(state),
		Duration:   duration,
		UserTime:   state.UserTime(),
		SystemTime: state.SystemTime(),
		PeakRSS:    peakRSS(state),
	}

	if terminated.Load() {
		switch {
		case errors.Is(context.Cause(runCtx), errdefs.ErrOutputLimit):
			return res, fmt.Errorf("%w: more than %d bytes", errdefs.ErrOutputLimit, spec.MaxOutput)
		case ctx.Err() != nil:
			return res, ctx.Err()
		default:
			return res, fmt.Errorf("%w: process exceeded %s", errdefs.ErrTimeout, spec.Timeout)
		}
	}

	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return res, fmt.Errorf("process: wait %s: %w", spec.Path, waitErr)
	}
	return res, nil
}
