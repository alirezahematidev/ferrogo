//go:build !unix && !windows

package process

import (
	"os"
	"os/exec"
)

const treeMemoryLimits = false

func configureCmd(*exec.Cmd) {}

// tree is just the process on platforms without process groups.
type tree struct{ p *os.Process }

func attach(p *os.Process, _ int64) (*tree, error) { return &tree{p: p}, nil }
func (t *tree) terminate() error                   { return t.p.Kill() }
func (t *tree) kill() error                        { return nil }
func (t *tree) close() error                       { return nil }

func signalName(*os.ProcessState) string { return "" }

func peakRSS(*os.ProcessState) uint64 { return 0 }
