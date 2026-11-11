package compiler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/cache"
	"github.com/alirezahematidev/ferrogo/internal/process"
)

// Dependency is a Cargo dependency of a generated crate. Exactly one source
// is set: Version (registry), Path, or Git.
type Dependency struct {
	Name              string
	Version           string
	Features          []string
	NoDefaultFeatures bool
	Path              string
	Git               string
	Branch, Tag, Rev  string
	Registry          string
}

var (
	crateName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	versionReq = regexp.MustCompile(`^[0-9A-Za-z.*^~=<>, +-]{1,64}$`)
	feature    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_+./-]{0,63}$`)
	gitRef     = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,128}$`)
	gitURL     = regexp.MustCompile(`^(https://|ssh://|git@|file://)[^\s"'\\]+$`)
)

// lockRoot replaces the generated package's name in stored lockfiles, so a
// lock resolved for one snippet is reusable for any snippet with the same
// dependencies.
const lockRoot = "@@FERROGO_ROOT@@"

// Validate checks the dependency for well-formedness.
func (d Dependency) Validate() error {
	if !crateName.MatchString(d.Name) {
		return fmt.Errorf("invalid crate name %q", d.Name)
	}
	if d.Name == "ferrogo" {
		return errors.New(`dependency name "ferrogo" is reserved for the ABI helper module`)
	}
	sources := 0
	for _, set := range []bool{d.Version != "", d.Path != "", d.Git != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return fmt.Errorf("dependency %s: set exactly one of Version, Path or Git", d.Name)
	}
	if d.Version != "" && !versionReq.MatchString(d.Version) {
		return fmt.Errorf("dependency %s: invalid version requirement %q", d.Name, d.Version)
	}
	if d.Path != "" && !filepath.IsAbs(d.Path) {
		return fmt.Errorf("dependency %s: path %q must be absolute", d.Name, d.Path)
	}
	if d.Git != "" && !gitURL.MatchString(d.Git) {
		return fmt.Errorf("dependency %s: invalid git URL %q", d.Name, d.Git)
	}
	refs := 0
	for _, r := range []string{d.Branch, d.Tag, d.Rev} {
		if r != "" {
			refs++
			if !gitRef.MatchString(r) {
				return fmt.Errorf("dependency %s: invalid git reference %q", d.Name, r)
			}
		}
	}
	if refs > 0 && d.Git == "" {
		return fmt.Errorf("dependency %s: Branch, Tag and Rev require Git", d.Name)
	}
	if refs > 1 {
		return fmt.Errorf("dependency %s: set at most one of Branch, Tag and Rev", d.Name)
	}
	if d.Registry != "" && (!crateName.MatchString(d.Registry) || d.Version == "") {
		return fmt.Errorf("dependency %s: invalid registry %q (registries need a Version)", d.Name, d.Registry)
	}
	for _, f := range d.Features {
		if !feature.MatchString(f) {
			return fmt.Errorf("dependency %s: invalid feature %q", d.Name, f)
		}
	}
	return nil
}

// toml renders the dependency as an inline-table entry.
func (d Dependency) toml() string {
	var fields []string
	add := func(k, v string) { fields = append(fields, k+" = "+tomlString(v)) }
	switch {
	case d.Version != "":
		add("version", d.Version)
		if d.Registry != "" {
			add("registry", d.Registry)
		}
	case d.Path != "":
		add("path", filepath.ToSlash(d.Path))
	case d.Git != "":
		add("git", d.Git)
		for k, v := range map[string]string{"branch": d.Branch, "tag": d.Tag, "rev": d.Rev} {
			if v != "" {
				add(k, v)
			}
		}
	}
	if len(d.Features) > 0 {
		quoted := make([]string, len(d.Features))
		for i, f := range d.Features {
			quoted[i] = tomlString(f)
		}
		fields = append(fields, "features = ["+strings.Join(quoted, ", ")+"]")
	}
	if d.NoDefaultFeatures {
		fields = append(fields, "default-features = false")
	}
	return d.Name + " = { " + strings.Join(fields, ", ") + " }"
}

// tomlString renders s as a TOML basic string. Values are validated before
// they get here; escaping is a second line of defence against injection.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// dependencyPaths lists the local path dependencies' directories.
func (r *Request) dependencyPaths() []string {
	var out []string
	for _, d := range r.Dependencies {
		if d.Path != "" {
			out = append(out, d.Path)
		}
	}
	return out
}

// dependencySection renders [dependencies] in a stable order.
func (r *Request) dependencySection() string {
	deps := slices.Clone(r.Dependencies)
	slices.SortFunc(deps, func(a, b Dependency) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	for _, d := range deps {
		b.WriteString(d.toml())
		b.WriteByte('\n')
	}
	return b.String()
}

// depsFingerprint identifies a dependency set for lock and target-dir reuse.
func (c *Compiler) depsFingerprint(req Request, configs []string) string {
	b := cache.NewKeyBuilder("ferrogo-deps-v1")
	b.AddString("cargo", c.tc.CargoVersion)
	b.AddString("rustc", c.tc.RustcVerbose)
	b.AddString("deps", req.dependencySection())
	b.AddString("edition", req.edition())
	for _, p := range configs {
		b.AddString("cargo-config", p)
	}
	return b.Sum()
}

// lockfile returns the resolved Cargo.lock for req's dependencies (with the
// root package name replaced by lockRoot), resolving and storing it on
// first use. Reusing stored locks makes builds reproducible: versions stay
// pinned until the cache is cleared.
func (c *Compiler) lockfile(ctx context.Context, req Request, manifest, fingerprint string) ([]byte, error) {
	locks, err := c.cache.Subdir("locks")
	if err != nil {
		return nil, err
	}
	stored := filepath.Join(locks, fingerprint+".lock")
	if data, err := os.ReadFile(stored); err == nil {
		return data, nil
	}
	work, cleanup, err := c.cache.NewWorkDir()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cleanup(); err != nil {
			c.log.Warn("removing lock resolution directory failed", "dir", work, "error", err)
		}
	}()
	if err := writeProject(work, manifest, req.Sources); err != nil {
		return nil, err
	}
	args := []string{"generate-lockfile", "--manifest-path", filepath.Join(work, "Cargo.toml"), "--color", "never"}
	if !req.Network {
		args = append(args, "--offline")
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	res, err := c.run(ctx, process.Spec{
		Path: c.tc.Cargo, Args: args, Env: c.tc.BaseEnv(c.cargoEnv(req, "")), Dir: work,
		Stderr: req.Progress, MaxOutput: maxCargoOutput, Timeout: timeout,
	}, req.Network, req.dependencyPaths()...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		out := string(res.Stderr)
		if !req.Network && strings.Contains(out, "offline") {
			out += "\nhint: the crates are not in the local Cargo cache; allow downloads with DependencyPolicy{Network: true}"
		}
		return nil, &Error{Reason: ReasonDependency, Target: req.Target, Output: out}
	}
	data, err := os.ReadFile(filepath.Join(work, "Cargo.lock"))
	if err != nil {
		return nil, fmt.Errorf("compiler: reading resolved Cargo.lock: %w", err)
	}
	data = []byte(strings.Replace(string(data), `name = "`+req.packageName()+`"`, `name = "`+lockRoot+`"`, 1))
	tmp, err := os.CreateTemp(locks, ".lock-*")
	if err != nil {
		return nil, err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		werr = os.Rename(tmp.Name(), stored)
	}
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("compiler: storing Cargo.lock: %w", errors.Join(werr, cerr))
	}
	return data, nil
}

// cargoEnv is the environment for cargo invocations (on top of BaseEnv).
// It sets only stable flags: varying flags would change cargo's
// fingerprints and force dependencies to rebuild on every build.
func (c *Compiler) cargoEnv(req Request, extraFlags string) map[string]string {
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	flags := slices.Clone(req.RustFlags)
	// Remap machine-specific paths for reproducible artifacts. The prefixes
	// are constant per cache, so cargo's fingerprints stay stable.
	flags = append(flags, "--remap-path-prefix="+filepath.Join(c.cache.Dir(), "work")+"=/ferrogo")
	flags = append(flags, "--remap-path-prefix="+filepath.Join(c.cache.Dir(), "targets")+"=/ferrogo-target")
	if home := c.tc.CargoHome(); home != "" {
		flags = append(flags, "--remap-path-prefix="+home+"=/cargo")
	}
	if extraFlags != "" {
		flags = append(flags, extraFlags)
	}
	env["RUSTC"] = c.tc.Rustc
	env["CARGO_ENCODED_RUSTFLAGS"] = strings.Join(flags, "\x1f")
	env["CARGO_TERM_COLOR"] = "never"
	env["CARGO_TERM_PROGRESS_WHEN"] = "never"
	return env
}

// errNetworkHint explains an offline failure during a build.
func networkHint(stderr string, network bool) string {
	if !network && (strings.Contains(stderr, "--offline") || strings.Contains(stderr, "offline mode")) {
		return stderr + "\nhint: a crate is missing from the local Cargo cache; allow downloads with DependencyPolicy{Network: true}"
	}
	return stderr
}
