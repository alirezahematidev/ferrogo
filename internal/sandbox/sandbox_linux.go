//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

var (
	probeOnce sync.Once
	bwrapPath string
	probeErr  error
)

// Available reports whether bubblewrap exists and can create namespaces
// (some distributions restrict unprivileged user namespaces).
func Available() error {
	probeOnce.Do(func() {
		path, err := exec.LookPath("bwrap")
		if err != nil {
			probeErr = fmt.Errorf("bubblewrap (bwrap) is not installed")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, path, "--ro-bind", "/", "/", "--unshare-all", "true").CombinedOutput(); err != nil {
			probeErr = fmt.Errorf("bubblewrap cannot create a sandbox here: %v: %s", err, out)
			return
		}
		bwrapPath = path
	})
	return probeErr
}

// Command returns a command line running path with args under p: a
// read-only view of the file system with a private /tmp, the home directory
// replaced by an empty tmpfs (Readable paths bound back read-only),
// Writable paths bound read-write, and no network unless allowed.
func Command(p Policy, path string, args []string) (string, []string, error) {
	if err := Available(); err != nil {
		return "", nil, err
	}
	a := []string{"--die-with-parent", "--new-session", "--unshare-all"}
	if p.AllowNetwork {
		a = append(a, "--share-net")
	}
	a = append(a, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp")
	if home, err := os.UserHomeDir(); err == nil {
		a = append(a, "--tmpfs", home)
	}
	exists := func(s string) bool { _, err := os.Stat(s); return err == nil }
	for _, r := range p.Readable {
		if exists(r) {
			a = append(a, "--ro-bind", r, r)
		}
	}
	for _, w := range p.Writable {
		if exists(w) {
			a = append(a, "--bind", w, w)
		}
	}
	if p.Dir != "" {
		a = append(a, "--chdir", p.Dir)
	}
	a = append(a, "--", path)
	return bwrapPath, append(a, args...), nil
}
