// Command ferrogo compiles and runs Rust code through the FerroGo library.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `ferrogo — compile and run Rust from Go

Usage:
  ferrogo <command> [flags] [arguments]

Commands:
  run <file.rs|dir> [-- args]  compile and run a Rust program or Cargo project
  build <file.rs|dir> [-o p]   compile a program (or --lib library) and write the artifact
  compile <file.rs|dir>        compile into the cache and report diagnostics, without running
  call <src> <fn> [args...]    call an exported Rust function (src: .rs, project dir or artifact)
  generate [dir]               compile Rust embedded in a Go package and generate Go wrappers
  abi                          print the Rust ABI helper (vendor it into Cargo projects)
  doctor [--fix]               check the Rust toolchain, targets and backends
  inspect [artifact]           show toolchain, cache and backends, or a module's exports
  cache stats|clean            show or clear the artifact cache
  init                         write a ferrogo.toml template
  version                      print version information

Run 'ferrogo <command> -h' for the flags of a command.
`

// exitError carries a process exit code through the command functions.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the CLI and returns the process exit code: the program's own
// exit code for `ferrogo run`, 1 for failures, 2 for usage errors.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	out := newOutput(stdout, stderr)
	cmds := map[string]func(context.Context, *output, io.Reader, []string) error{
		"run":      cmdRun,
		"build":    cmdBuild,
		"compile":  cmdCompile,
		"generate": cmdGenerate,
		"abi":      cmdABI,
		"call":     cmdCall,
		"doctor":   cmdDoctor,
		"inspect":  cmdInspect,
		"cache":    cmdCache,
		"init":     cmdInit,
		"version":  cmdVersion,
	}
	cmd, ok := cmds[args[0]]
	if !ok {
		out.errorf("unknown command %q", args[0])
		fmt.Fprint(stderr, usage)
		return 2
	}
	err := cmd(ctx, out, stdin, args[1:])
	var ee exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, errUsage):
		return 2
	default:
		out.errorf("%v", err)
		return 1
	}
}
