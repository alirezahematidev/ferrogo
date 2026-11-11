//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// Available reports whether sandboxing works on this machine.
func Available() error {
	if _, err := os.Stat(sandboxExec); err != nil {
		return fmt.Errorf("sandbox-exec not available: %w", err)
	}
	return nil
}

// Command returns a command line running path with args under p, using a
// generated Seatbelt profile.
func Command(p Policy, path string, args []string) (string, []string, error) {
	if err := Available(); err != nil {
		return "", nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	// Seatbelt matches resolved paths (/var is a symlink to /private/var).
	quote := func(s string) (string, error) {
		if resolved, err := filepath.EvalSymlinks(s); err == nil {
			s = resolved
		}
		if strings.ContainsAny(s, "\"\\\x00\n") {
			return "", fmt.Errorf("path %q cannot be used in a sandbox profile", s)
		}
		return `"` + s + `"`, nil
	}
	subpaths := func(paths []string) (string, error) {
		var b strings.Builder
		for _, s := range paths {
			q, err := quote(s)
			if err != nil {
				return "", err
			}
			b.WriteString(" (subpath " + q + ")")
		}
		return b.String(), nil
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	if !p.AllowNetwork {
		b.WriteString("(deny network*)\n")
	}
	w, err := subpaths(p.Writable)
	if err != nil {
		return "", nil, err
	}
	b.WriteString("(deny file-write*)\n(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/tty\") (regex #\"^/dev/fd/\")" + w + ")\n")
	qh, err := quote(home)
	if err != nil {
		return "", nil, err
	}
	r, err := subpaths(append(append([]string{}, p.Readable...), p.Writable...))
	if err != nil {
		return "", nil, err
	}
	b.WriteString("(deny file-read-data (subpath " + qh + "))\n")
	if r != "" {
		b.WriteString("(allow file-read-data" + r + ")\n")
	}
	return sandboxExec, append([]string{"-p", b.String(), path}, args...), nil
}
