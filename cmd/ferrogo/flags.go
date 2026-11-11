package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

// errUsage marks a command-line usage error (exit status 2). The message has
// already been printed by the flag package or the command.
var errUsage = errors.New("usage error")

// common holds the flags shared by commands that create a Runtime.
type common struct {
	fs             *flag.FlagSet
	backend        string
	config         string
	profile        string
	edition        string
	trust          bool
	noCache        bool
	cacheDir       string
	json           bool
	verbose        bool
	timeout        time.Duration
	compileTimeout time.Duration
	maxOutput      int64
	env            envFlag
}

type envFlag map[string]string

func (e envFlag) String() string { return "" }
func (e envFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", s)
	}
	e[k] = v
	return nil
}

func newCommon(name, usageLine string, stderr io.Writer) *common {
	c := &common{fs: flag.NewFlagSet(name, flag.ContinueOnError), env: envFlag{}}
	fs := c.fs
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: ferrogo %s\n\nFlags:\n", usageLine)
		fs.PrintDefaults()
	}
	fs.StringVar(&c.backend, "backend", "", "execution backend: auto, wasm, native (native-process), native-library")
	fs.StringVar(&c.config, "config", "", "configuration file (default: ./ferrogo.toml if present)")
	fs.StringVar(&c.profile, "profile", "", "build profile: release or debug")
	fs.StringVar(&c.edition, "edition", "", "Rust edition (2015, 2018, 2021, 2024)")
	fs.BoolVar(&c.trust, "trust", false, "allow native backends (implied by an explicit --backend native*)")
	fs.BoolVar(&c.noCache, "no-cache", false, "disable the artifact cache")
	fs.StringVar(&c.cacheDir, "cache-dir", "", "artifact cache directory")
	fs.BoolVar(&c.json, "json", false, "print machine-readable JSON")
	fs.BoolVar(&c.verbose, "v", false, "verbose: stream cargo progress")
	fs.DurationVar(&c.timeout, "timeout", 0, "execution time limit (0 = none; default from config)")
	fs.DurationVar(&c.compileTimeout, "compile-timeout", 0, "compilation time limit")
	fs.Int64Var(&c.maxOutput, "max-output", 0, "maximum combined stdout+stderr bytes (0 = unlimited)")
	fs.Var(c.env, "env", "environment variable KEY=VALUE for the program (repeatable)")
	return c
}

// parse parses flags interspersed with positional arguments. Everything
// after a literal "--" is returned separately (program arguments).
func (c *common) parse(args []string) (positional, rest []string, err error) {
	return c.parseN(args, 0)
}

// parseN is parse, but stops interpreting flags once maxPositional
// positional arguments were seen (0 = no limit); the remaining arguments are
// returned verbatim in rest, so values like -5 are not mistaken for flags.
func (c *common) parseN(args []string, maxPositional int) (positional, rest []string, err error) {
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], args[i+1:]
			break
		}
	}
	for {
		if err := c.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, nil, errUsage
			}
			return nil, nil, errUsage
		}
		args = c.fs.Args()
		if len(args) == 0 {
			return positional, rest, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
		if maxPositional > 0 && len(positional) == maxPositional {
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
			return positional, append(args, rest...), nil
		}
	}
}

func (c *common) isSet(name string) bool {
	set := false
	c.fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// options turns the flags (and the config file) into runtime options.
// Precedence: flags > config file > library defaults.
func (c *common) options() ([]ferrogo.Option, error) {
	var opts []ferrogo.Option
	cfgPath := c.config
	if cfgPath == "" {
		cfgPath = findConfig()
	}
	if cfgPath != "" {
		opts = append(opts, ferrogo.WithConfigFile(cfgPath))
	}
	if c.backend != "" {
		b, err := ferrogo.ParseBackend(c.backend)
		if err != nil {
			return nil, err
		}
		opts = append(opts, ferrogo.WithBackend(b))
		// Explicitly asking for a native backend on the command line is the
		// user's statement that they trust the code they are running.
		if b == ferrogo.BackendNativeProcess || b == ferrogo.BackendNativeLibrary {
			c.trust = true
		}
	}
	if c.trust {
		opts = append(opts, ferrogo.WithTrust(ferrogo.Trusted))
	}
	if c.profile != "" {
		opts = append(opts, ferrogo.WithProfile(ferrogo.Profile(c.profile)))
	}
	if c.edition != "" {
		opts = append(opts, ferrogo.WithEdition(c.edition))
	}
	if c.noCache {
		opts = append(opts, ferrogo.WithCache(false))
	}
	if c.cacheDir != "" {
		opts = append(opts, ferrogo.WithCacheDir(c.cacheDir))
	}
	if c.isSet("timeout") {
		opts = append(opts, ferrogo.WithTimeout(c.timeout))
	}
	if c.isSet("compile-timeout") {
		opts = append(opts, ferrogo.WithCompileTimeout(c.compileTimeout))
	}
	if c.isSet("max-output") {
		opts = append(opts, ferrogo.WithMaxOutput(c.maxOutput))
	}
	if len(c.env) > 0 {
		opts = append(opts, ferrogo.WithEnv(c.env))
	}
	return opts, nil
}

func (c *common) runtime(out *output) (*ferrogo.Runtime, error) {
	opts, err := c.options()
	if err != nil {
		return nil, err
	}
	if c.verbose && !c.json {
		opts = append(opts, ferrogo.WithBuildOutput(out.stderr))
	}
	return ferrogo.New(opts...)
}

// findConfig looks for ferrogo.toml in the current directory and its
// parents, like Cargo does for Cargo.toml.
func findConfig() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(dir, ferrogo.ConfigFileName)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
