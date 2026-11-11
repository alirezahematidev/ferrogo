package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/cache"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/process"
)

// ProjectRequest describes a build of an existing Cargo project.
type ProjectRequest struct {
	// Dir is the absolute project directory (containing Cargo.toml).
	Dir string
	// DisplayDir is the directory as the user wrote it (for diagnostics).
	DisplayDir string
	Kind       Kind
	Target     string
	Backend    string
	// Profile is "release", "debug" or a custom profile defined by the project.
	Profile   string
	RustFlags []string
	Env       map[string]string
	Timeout   time.Duration
	Progress  io.Writer
	Network   bool
}

// ProjectDependency is a dependency declared by a project.
type ProjectDependency struct {
	Name string
	// Kind is "normal", "build" or "dev".
	Kind string
	// Source is "registry", "git" or "path".
	Source string
}

// ProjectInfo describes a Cargo project, read without resolving dependencies.
type ProjectInfo struct {
	Name           string
	ManifestPath   string
	Dependencies   []ProjectDependency
	HasBuildScript bool
	Bins           []string
	DefaultRun     string
	HasCdylib      bool
}

var profileName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// InspectProject reads a project's manifest with `cargo metadata --no-deps`,
// which needs no network and resolves nothing.
func (c *Compiler) InspectProject(ctx context.Context, dir string) (*ProjectInfo, error) {
	manifest := filepath.Join(dir, "Cargo.toml")
	if _, err := os.Stat(manifest); err != nil {
		return nil, fmt.Errorf("compiler: %s is not a Cargo project: %w", dir, err)
	}
	res, err := c.run(ctx, process.Spec{
		Path: c.tc.Cargo, Env: c.tc.BaseEnv(map[string]string{"RUSTC": c.tc.Rustc}), Dir: dir,
		Args:    []string{"metadata", "--no-deps", "--format-version", "1", "--offline", "--manifest-path", manifest, "--color", "never"},
		Timeout: time.Minute, MaxOutput: 32 << 20,
	}, false, dir)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &Error{Reason: ReasonManifest, Output: string(res.Stderr)}
	}
	var meta struct {
		Packages []struct {
			Name         string `json:"name"`
			ManifestPath string `json:"manifest_path"`
			DefaultRun   string `json:"default_run"`
			Dependencies []struct {
				Name   string  `json:"name"`
				Source *string `json:"source"`
				Kind   *string `json:"kind"`
			} `json:"dependencies"`
			Targets []struct {
				Name       string   `json:"name"`
				Kind       []string `json:"kind"`
				CrateTypes []string `json:"crate_types"`
			} `json:"targets"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(res.Stdout, &meta); err != nil {
		return nil, fmt.Errorf("compiler: parsing cargo metadata: %w", err)
	}
	want, err := filepath.EvalSymlinks(manifest)
	if err != nil {
		return nil, err
	}
	for _, p := range meta.Packages {
		got, err := filepath.EvalSymlinks(p.ManifestPath)
		if err != nil || got != want {
			continue
		}
		info := &ProjectInfo{Name: p.Name, ManifestPath: p.ManifestPath, DefaultRun: p.DefaultRun}
		for _, d := range p.Dependencies {
			pd := ProjectDependency{Name: d.Name, Kind: "normal", Source: "path"}
			if d.Kind != nil {
				pd.Kind = *d.Kind
			}
			if d.Source != nil {
				pd.Source, _, _ = strings.Cut(*d.Source, "+") // "registry+https://..." / "git+https://..."
				if pd.Source == "sparse" {
					pd.Source = "registry"
				}
			}
			info.Dependencies = append(info.Dependencies, pd)
		}
		for _, t := range p.Targets {
			for _, k := range t.Kind {
				switch k {
				case "bin":
					info.Bins = append(info.Bins, t.Name)
				case "custom-build":
					info.HasBuildScript = true
				case "cdylib":
					info.HasCdylib = true
				}
			}
		}
		return info, nil
	}
	return nil, fmt.Errorf("compiler: %s is a virtual workspace; point to a member package's directory", dir)
}

// BuildProject builds a Cargo project in place, with its target directory in
// the cache. Cargo's own fingerprinting decides what to rebuild (it tracks
// every input, including path dependencies outside the project), so there
// is no FerroGo cache lookup; the produced artifact is stored in the cache
// for a stable, read-only path.
func (c *Compiler) BuildProject(ctx context.Context, req ProjectRequest, info *ProjectInfo) (*Artifact, error) {
	if !c.tc.HasTarget(req.Target) {
		return nil, &Error{Reason: ReasonTarget, Target: req.Target,
			Output: fmt.Sprintf("error: the %s target is not installed; fix: %s", req.Target, c.tc.TargetFix(req.Target))}
	}
	if req.Profile != "" && !profileName.MatchString(req.Profile) {
		return nil, fmt.Errorf("compiler: invalid profile name %q", req.Profile)
	}
	args := []string{
		"build", "--manifest-path", info.ManifestPath, "--message-format=json", "--color", "never",
		"--target", req.Target,
	}
	switch req.Profile {
	case "", ProfileRelease:
		args = append(args, "--release")
	case ProfileDebug:
	default:
		args = append(args, "--profile", req.Profile)
	}
	var bin string
	if req.Kind == KindLibrary {
		if !info.HasCdylib {
			return nil, fmt.Errorf("%w: project %s has no cdylib target; add crate-type = [\"cdylib\"] under [lib]", errdefs.ErrUnsupported, info.Name)
		}
		args = append(args, "--lib")
	} else {
		switch {
		case len(info.Bins) == 1:
			bin = info.Bins[0]
		case info.DefaultRun != "":
			bin = info.DefaultRun
		case len(info.Bins) == 0:
			return nil, fmt.Errorf("compiler: project %s has no binary target", info.Name)
		default:
			return nil, fmt.Errorf("compiler: project %s has several binaries (%s); set default-run in Cargo.toml", info.Name, strings.Join(info.Bins, ", "))
		}
		args = append(args, "--bin", bin)
	}
	if !req.Network {
		args = append(args, "--offline")
	}
	if _, err := os.Stat(filepath.Join(req.Dir, "Cargo.lock")); err == nil {
		args = append(args, "--locked")
	}
	projects, err := c.cache.Subdir("projects")
	if err != nil {
		return nil, err
	}
	dirKey := cache.NewKeyBuilder("ferrogo-project-dir-v1")
	dirKey.AddString("dir", req.Dir)
	targetDir := filepath.Join(projects, dirKey.Sum()[:16])
	args = append(args, "--target-dir", targetDir)

	env := c.cargoEnv(Request{Env: req.Env, RustFlags: req.RustFlags}, "")
	start := time.Now()
	// Path dependencies outside the project directory are not known here;
	// with the sandbox enabled they must live inside it (or outside $HOME).
	res, err := c.run(ctx, process.Spec{
		Path: c.tc.Cargo, Args: args, Env: c.tc.BaseEnv(env), Dir: req.Dir,
		Stderr: req.Progress, MaxOutput: maxCargoOutput, Timeout: req.Timeout,
	}, req.Network, req.Dir)
	if err != nil {
		if errors.Is(err, errdefs.ErrTimeout) {
			return nil, &Error{Reason: ReasonTimeout, Target: req.Target, Output: "error: compilation exceeded the timeout"}
		}
		return nil, err
	}
	mapper := sourceMapper{projectDir: req.DisplayDir}
	parsed := parseCargoJSON(res.Stdout, mapper)
	stderr := string(res.Stderr)
	if res.ExitCode != 0 || !parsed.success {
		return nil, &Error{Reason: classify(parsed.diagnostics, stderr), Target: req.Target,
			Diagnostics: parsed.diagnostics, Output: networkHint(stderr, req.Network)}
	}

	var output string
	fresh := false
	for _, m := range parsed.artifacts {
		if m.Target == nil || m.ManifestPath != info.ManifestPath {
			continue
		}
		if req.Kind == KindProgram && m.Target.Name == bin && m.Executable != nil {
			output, fresh = *m.Executable, m.Fresh
		}
		if req.Kind == KindLibrary {
			for _, f := range m.Filenames {
				switch strings.ToLower(filepath.Ext(f)) {
				case ".so", ".dylib", ".dll", ".wasm":
					output, fresh = f, m.Fresh
				}
			}
		}
	}
	if output == "" {
		return nil, &Error{Reason: ReasonInternal, Target: req.Target, Output: "cargo reported no artifact for " + info.Name}
	}
	sum, size, err := hashFileForKey(output)
	if err != nil {
		return nil, err
	}
	key := cache.NewKeyBuilder("ferrogo-project-artifact-v1")
	key.AddString("content", sum)
	key.AddString("kind", string(req.Kind))
	key.AddString("target", req.Target)
	entry, err := c.cache.Put(key.Sum(), output, cache.Meta{
		Kind: string(req.Kind), Target: req.Target, Backend: req.Backend,
		Inputs: map[string]string{"project": req.Dir},
	})
	if err != nil {
		return nil, err
	}
	var warnings []Diagnostic
	for _, d := range parsed.diagnostics {
		if d.Level == "warning" {
			warnings = append(warnings, d)
		}
	}
	return &Artifact{
		Path: entry.Path, Key: entry.Key, Kind: req.Kind, Target: req.Target, Size: size,
		Cached: fresh, Duration: time.Since(start), Warnings: warnings,
	}, nil
}

func hashFileForKey(path string) (string, int64, error) {
	b := cache.NewKeyBuilder("file")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	b.Add("content", data)
	return b.Sum(), int64(len(data)), nil
}
