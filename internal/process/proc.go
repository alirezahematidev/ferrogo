package process

import (
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/alirezahematidev/ferrogo/internal/outcap"
)

// Proc is a long-running child process with piped stdin and stdout, used
// for worker processes. Its stderr is captured up to Spec.MaxOutput bytes.
type Proc struct {
	cmd    *exec.Cmd
	tree   *tree
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	stderr *outcap.Stream
	done   chan struct{}
}

// Start launches spec without waiting for it. Spec.Stdin, Stdout, Timeout
// and GracePeriod do not apply; the caller controls the lifetime with
// Kill. The whole process tree is killed when the process exits or Kill is
// called.
func Start(spec Spec) (*Proc, error) {
	if spec.Path == "" {
		return nil, errors.New("process: empty executable path")
	}
	if spec.MemoryLimit > 0 && !treeMemoryLimits {
		return nil, errors.New("process: memory limits are not supported on this platform")
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // never inherit the parent environment
	}
	cmd.Dir = spec.Dir
	configureCmd(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("process: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("process: %w", err)
	}
	stderr := outcap.New(spec.MaxOutput, nil).Stream(spec.Stderr)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process: start %s: %w", spec.Path, err)
	}
	t, err := attach(cmd.Process, spec.MemoryLimit)
	if err != nil {
		_ = cmd.Process.Kill() // the process must not run outside its limits
		_ = cmd.Wait()
		return nil, fmt.Errorf("process: %w", err)
	}
	p := &Proc{cmd: cmd, tree: t, Stdin: stdin, Stdout: stdout, stderr: stderr, done: make(chan struct{})}
	go func() {
		// Wait's error only repeats the exit status, read via ExitCode.
		_ = cmd.Wait()
		_ = t.kill()  // stragglers in the tree
		_ = t.close() // release the group/job handle
		close(p.done)
	}()
	return p, nil
}

// Kill terminates the process tree. Done is closed once it has exited.
func (p *Proc) Kill() error { return p.tree.kill() }

// Done is closed when the process has exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// ExitCode returns the exit status; valid after Done is closed.
func (p *Proc) ExitCode() int { return p.cmd.ProcessState.ExitCode() }

// Stderr returns the captured stderr; valid after Done is closed.
func (p *Proc) Stderr() []byte { return p.stderr.Bytes() }
