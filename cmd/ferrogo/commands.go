package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo"
	"github.com/alirezahematidev/ferrogo/internal/abi"
)

// singleSource accepts a .rs file or a Cargo project directory.
func singleSource(c *common, positional []string) (ferrogo.Source, error) {
	if len(positional) != 1 {
		c.fs.Usage()
		return nil, errUsage
	}
	if fi, err := os.Stat(positional[0]); err == nil && fi.IsDir() {
		// Building a project the user points at is like `cargo run`: they
		// trust the code (it may have a build script).
		c.trust = true
		return ferrogo.Project(positional[0]), nil
	}
	return ferrogo.File(positional[0]), nil
}

func cmdRun(ctx context.Context, out *output, stdin io.Reader, args []string) error {
	c := newCommon("run", "run [flags] <file.rs | cargo-project-dir> [-- program args...]", out.stderr)
	positional, progArgs, err := c.parse(args)
	if err != nil {
		return err
	}
	src, err := singleSource(c, positional)
	if err != nil {
		return err
	}
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()

	runOpts := []ferrogo.RunOption{ferrogo.WithStdin(stdin), ferrogo.WithArgs(progArgs...)}
	if !c.json {
		runOpts = append(runOpts, ferrogo.WithStdout(out.stdout), ferrogo.WithStderr(out.stderr))
	}
	res, err := rt.Run(ctx, src, runOpts...)

	if c.json {
		type jsonRun struct {
			Stdout     string            `json:"stdout"`
			Stderr     string            `json:"stderr"`
			ExitCode   int               `json:"exit_code"`
			Backend    ferrogo.Backend   `json:"backend,omitempty"`
			DurationMS float64           `json:"duration_ms"`
			Artifact   *ferrogo.Artifact `json:"artifact,omitempty"`
			Error      *jsonError        `json:"error,omitempty"`
		}
		jr := jsonRun{Error: toJSONError(err)}
		if res != nil {
			jr.Stdout, jr.Stderr, jr.ExitCode, jr.Backend = res.Stdout, res.Stderr, res.ExitCode, res.Backend
			jr.DurationMS, jr.Artifact = float64(res.Duration.Microseconds())/1000, res.Artifact
		}
		if jerr := out.json(jr); jerr != nil {
			return jerr
		}
		return exitCode(res, err)
	}

	if res != nil && len(res.Artifact.Warnings) > 0 && !res.Artifact.Cached {
		out.diagnostics(res.Artifact.Warnings)
	}
	var ee *ferrogo.ExitError
	if errors.As(err, &ee) && ee.Panic == nil && ee.Signal == "" {
		// The program reported its own failure; just propagate the status.
		return exitError{code: ee.Code}
	}
	if err != nil {
		out.failure(err)
		return exitCode(res, err)
	}
	return nil
}

// exitCode maps a run outcome to the CLI's exit status.
func exitCode(res *ferrogo.Result, err error) error {
	switch {
	case err == nil:
		return nil
	case res != nil && res.ExitCode > 0:
		return exitError{code: res.ExitCode}
	default:
		return exitError{code: 1}
	}
}

func buildFlags(name, usageLine string, out *output) (*common, *bool) {
	c := newCommon(name, usageLine, out.stderr)
	lib := c.fs.Bool("lib", false, "compile a library (cdylib) instead of a program")
	return c, lib
}

func cmdBuild(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c, lib := buildFlags("build", "build [flags] <file.rs> [-o output]", out)
	outPath := c.fs.String("o", "", "output path (default: derived from the source name)")
	positional, _, err := c.parse(args)
	if err != nil {
		return err
	}
	src, err := singleSource(c, positional)
	if err != nil {
		return err
	}
	art, err := buildArtifact(ctx, c, out, src, *lib)
	if err != nil {
		return err
	}
	dst := *outPath
	if dst == "" {
		dst = defaultOutput(fmt.Sprint(src), art)
	}
	if err := copyArtifact(art.Path, dst); err != nil {
		return err
	}
	if c.json {
		return out.json(map[string]any{"output": dst, "artifact": art})
	}
	out.status("Finished", "%s (%s, %s, %s)", dst, art.Target, humanBytes(art.Size), cachedLabel(art))
	return nil
}

func cmdCompile(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c, lib := buildFlags("compile", "compile [flags] <file.rs>", out)
	positional, _, err := c.parse(args)
	if err != nil {
		return err
	}
	src, err := singleSource(c, positional)
	if err != nil {
		return err
	}
	art, err := buildArtifact(ctx, c, out, src, *lib)
	if err != nil {
		return err
	}
	if c.json {
		return out.json(art)
	}
	out.status("Compiled", "%s → %s (%s, %s)", src, art.Path, art.Target, cachedLabel(art))
	return nil
}

func buildArtifact(ctx context.Context, c *common, out *output, src ferrogo.Source, lib bool) (*ferrogo.Artifact, error) {
	rt, err := c.runtime(out)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	kind := ferrogo.ArtifactProgram
	if lib {
		kind = ferrogo.ArtifactLibrary
	}
	if !c.json {
		out.status("Compiling", "%s", src)
	}
	art, err := rt.Build(ctx, src, kind)
	if err != nil {
		if c.json {
			if jerr := out.json(map[string]any{"error": toJSONError(err)}); jerr != nil {
				return nil, jerr
			}
			return nil, exitError{code: 1}
		}
		out.failure(err)
		return nil, exitError{code: 1}
	}
	if !c.json && !art.Cached {
		out.diagnostics(art.Warnings)
	}
	return art, nil
}

func cachedLabel(a *ferrogo.Artifact) string {
	if a.Cached {
		return "cached"
	}
	return "built in " + a.BuildDuration.Round(time.Millisecond).String()
}

func defaultOutput(src string, a *ferrogo.Artifact) string {
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	ext := filepath.Ext(a.Path)
	if a.Kind == ferrogo.ArtifactLibrary && ext != ".wasm" && ext != ".dll" {
		base = "lib" + base
	}
	return base + ext
}

func copyArtifact(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

func cmdInspect(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("inspect", "inspect [flags] [artifact]", out.stderr)
	positional, _, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(positional) == 1 {
		return inspectArtifact(ctx, c, out, positional[0])
	}
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()
	rep, err := rt.Inspect(ctx)
	if err != nil {
		return err
	}
	if c.json {
		return out.json(rep)
	}
	w := out.stdout
	fmt.Fprintf(w, "ferrogo %s (%s, %s)\n\n", rep.Version, rep.GoVersion, rep.Platform)
	if rep.Toolchain != nil {
		t := rep.Toolchain
		fmt.Fprintf(w, "toolchain:  rustc %s (%s, LLVM %s)\n            %s\n            %s\n", t.Version, t.Host, t.LLVMVersion, t.Rustc, t.CargoVersion)
		fmt.Fprintf(w, "targets:    %s\n", strings.Join(t.Targets, ", "))
	} else {
		fmt.Fprintf(w, "toolchain:  %s\n", out.paint(red, rep.ToolchainError))
	}
	fmt.Fprintf(w, "default:    backend=%s trust=%s\n", rep.DefaultBackend, rep.Trust)
	fmt.Fprintf(w, "cache:      %s (%d entries, %s", rep.Cache.Dir, rep.Cache.Entries, humanBytes(rep.Cache.Bytes))
	if rep.Cache.MaxBytes > 0 {
		fmt.Fprintf(w, " of %s", humanBytes(rep.Cache.MaxBytes))
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintln(w, "\nbackends:")
	for _, b := range rep.Backends {
		mark := out.paint(green, "ready")
		if !b.Usable {
			mark = out.paint(yellow, "unavailable")
		}
		fmt.Fprintf(w, "  %-15s %-12s %s\n", b.Backend, mark, b.Target)
		for _, p := range b.Problems {
			fmt.Fprintf(w, "  %15s - %s\n", "", p)
		}
	}
	return nil
}

func cmdDoctor(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("doctor", "doctor [flags]", out.stderr)
	fix := c.fs.Bool("fix", false, "install missing compilation targets with rustup (explicit consent)")
	if _, _, err := c.parse(args); err != nil {
		return err
	}
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()
	rep, err := rt.Inspect(ctx)
	if err != nil {
		return err
	}

	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
		Fix    string `json:"fix,omitempty"`
	}
	var checks []check
	if t := rep.Toolchain; t != nil {
		checks = append(checks,
			check{Name: "rustc", OK: true, Detail: fmt.Sprintf("%s (%s) at %s", t.Version, t.Host, t.Rustc)},
			check{Name: "cargo", OK: true, Detail: t.CargoVersion})
	} else {
		checks = append(checks, check{Name: "rust toolchain", Detail: rep.ToolchainError,
			Fix: "install Rust from https://rustup.rs (rustup installs rustc and cargo per user)"})
	}
	if runtime.GOOS != "windows" {
		if path, err := exec.LookPath("cc"); err == nil {
			checks = append(checks, check{Name: "linker", OK: true, Detail: "cc at " + path})
		} else {
			fix := "install a C toolchain (e.g. build-essential or clang)"
			if runtime.GOOS == "darwin" {
				fix = "xcode-select --install"
			}
			checks = append(checks, check{Name: "linker", Detail: "cc not found (needed for native targets)", Fix: fix})
		}
	}
	checks = append(checks, check{Name: "cache", OK: true, Detail: rep.Cache.Dir})
	for _, b := range rep.Backends {
		// The trust policy is a deliberate setting, not an environment
		// problem: report it as a note on an otherwise healthy backend.
		var problems []string
		note := ""
		for _, p := range b.Problems {
			if strings.HasPrefix(p, "requires WithTrust") {
				note = " (requires --trust / WithTrust(Trusted))"
				continue
			}
			problems = append(problems, p)
		}
		ch := check{Name: "backend " + string(b.Backend), OK: len(problems) == 0, Detail: b.Target + note}
		for _, p := range problems {
			if i := strings.Index(p, "fix: "); i >= 0 {
				ch.Fix = p[i+len("fix: "):]
			}
		}
		if !ch.OK {
			ch.Detail = strings.Join(problems, "; ")
		}
		checks = append(checks, ch)
	}

	if *fix && rep.Toolchain != nil {
		if err := fixTargets(ctx, out, rep); err != nil {
			return err
		}
	}
	healthy := rep.Toolchain != nil
	if c.json {
		if err := out.json(map[string]any{"healthy": healthy, "checks": checks}); err != nil {
			return err
		}
	} else {
		for _, ch := range checks {
			mark := out.paint(green, "✓")
			if !ch.OK {
				mark = out.paint(yellow, "✗")
			}
			fmt.Fprintf(out.stdout, "%s %-24s %s\n", mark, ch.Name, ch.Detail)
			if ch.Fix != "" {
				fmt.Fprintf(out.stdout, "  %-24s %s %s\n", "", out.paint(bold, "fix:"), ch.Fix)
			}
		}
	}
	if !healthy {
		return exitError{code: 1}
	}
	return nil
}

func cmdCache(_ context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("cache", "cache [flags] stats|clean", out.stderr)
	positional, _, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || (positional[0] != "stats" && positional[0] != "clean") {
		c.fs.Usage()
		return errUsage
	}
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()
	if positional[0] == "clean" {
		before, err := rt.CacheStats()
		if err != nil {
			return err
		}
		if err := rt.ClearCache(); err != nil {
			return err
		}
		if c.json {
			return out.json(map[string]any{"removed_entries": before.Entries, "freed_bytes": before.Bytes + before.OtherBytes})
		}
		out.status("Cleaned", "%d entries, %s freed", before.Entries, humanBytes(before.Bytes+before.OtherBytes))
		return nil
	}
	st, err := rt.CacheStats()
	if err != nil {
		return err
	}
	if c.json {
		return out.json(st)
	}
	fmt.Fprintf(out.stdout, "directory:  %s\nentries:    %d\nartifacts:  %s\n", st.Dir, st.Entries, humanBytes(st.Bytes))
	if st.MaxBytes > 0 {
		fmt.Fprintf(out.stdout, "limit:      %s\n", humanBytes(st.MaxBytes))
	}
	fmt.Fprintf(out.stdout, "other:      %s (build directories, runtime caches)\n", humanBytes(st.OtherBytes))
	return nil
}

func cmdInit(_ context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("init", "init [--force]", out.stderr)
	force := c.fs.Bool("force", false, "overwrite an existing ferrogo.toml")
	if _, _, err := c.parse(args); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(ferrogo.ConfigFileName, flags, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists (use --force to overwrite)", ferrogo.ConfigFileName)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(ferrogo.ConfigTemplate); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	out.status("Created", "%s", ferrogo.ConfigFileName)
	return nil
}

func cmdVersion(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("version", "version [--json]", out.stderr)
	if _, _, err := c.parse(args); err != nil {
		return err
	}
	info := map[string]string{"ferrogo": ferrogo.Version(), "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH}
	rt, err := c.runtime(out)
	if err == nil {
		defer rt.Close()
		if tc, err := rt.Toolchain(ctx); err == nil {
			info["rustc"], info["cargo"] = tc.Version, tc.CargoVersion
		} else {
			info["rustc"] = "not found"
		}
	}
	if c.json {
		return out.json(info)
	}
	fmt.Fprintf(out.stdout, "ferrogo %s\ngo     %s %s\nrustc  %s\n", info["ferrogo"], info["go"], info["platform"], info["rustc"])
	return nil
}

// cmdABI prints the Rust ABI helper, for vendoring into Cargo projects
// (save it as src/ferrogo.rs and add `mod ferrogo;` to lib.rs).
func cmdABI(_ context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("abi", "abi > src/ferrogo.rs", out.stderr)
	if _, _, err := c.parse(args); err != nil {
		return err
	}
	_, err := out.stdout.Write(abi.Source)
	return err
}

// fixTargets installs missing targets with rustup. It only runs because the
// user passed --fix; FerroGo never installs anything implicitly.
func fixTargets(ctx context.Context, out *output, rep *ferrogo.Report) error {
	tc := rep.Toolchain
	for _, b := range rep.Backends {
		if b.Target == "" || tc.HasTarget(b.Target) {
			continue
		}
		if tc.Rustup == "" {
			return fmt.Errorf("target %s is missing and rustup is not installed; install it manually", b.Target)
		}
		out.status("Installing", "target %s (rustup target add %s)", b.Target, b.Target)
		cmd := exec.CommandContext(ctx, tc.Rustup, "target", "add", b.Target)
		cmd.Stdout, cmd.Stderr = out.stderr, out.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("rustup target add %s: %w", b.Target, err)
		}
		tc.Targets = append(tc.Targets, b.Target)
	}
	return nil
}
