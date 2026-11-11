package ferrogo

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/alirezahematidev/ferrogo/internal/compiler"
)

// Dependency is a Cargo dependency of the Rust code being compiled.
// Exactly one source must be set: Version (a registry crate), Path or Git.
//
// Dependencies execute code on the build machine at compile time (build
// scripts and procedural macros), so they are only used when a
// DependencyPolicy allows them.
type Dependency struct {
	// Name is the crate name, e.g. "serde_json".
	Name string
	// Version is a Cargo version requirement for a registry crate, e.g. "1"
	// or "=1.0.150".
	Version string
	// Features enables crate features.
	Features []string
	// NoDefaultFeatures disables the crate's default features.
	NoDefaultFeatures bool
	// Path is a local crate directory (relative paths are resolved against
	// the working directory).
	Path string
	// Git is a repository URL, optionally pinned by Branch, Tag or Rev.
	Git              string
	Branch, Tag, Rev string
	// Registry names an alternative registry configured in Cargo's config.
	Registry string
}

func (d Dependency) source() string {
	switch {
	case d.Path != "":
		return "path"
	case d.Git != "":
		return "git"
	}
	return "registry"
}

// DependencyPolicy decides which dependencies may be used. The zero value
// allows none. Allowing any dependency (or building a Project) permits
// code execution on the build machine through build scripts and
// procedural macros; see docs/security.md.
type DependencyPolicy struct {
	// AllowRegistry permits crates from crates.io or a configured registry.
	AllowRegistry bool
	// AllowGit permits git dependencies.
	AllowGit bool
	// AllowPath permits local path dependencies.
	AllowPath bool
	// Network lets Cargo download crates and resolve versions online.
	// Without it every build runs with --offline and can only use crates
	// already in the Cargo home.
	Network bool
	// Crates, if non-empty, restricts dependencies to these crate names.
	Crates []string
}

// check reports whether a dependency of the given source kind is allowed.
func (p DependencyPolicy) check(name, source string) error {
	allowed := map[string]bool{"registry": p.AllowRegistry, "git": p.AllowGit, "path": p.AllowPath}[source]
	if !allowed {
		field := map[string]string{"registry": "AllowRegistry", "git": "AllowGit", "path": "AllowPath"}[source]
		return fmt.Errorf("%w: %s dependency %q; allow it with WithDependencyPolicy(DependencyPolicy{%s: true}) "+
			"(dependencies run build scripts and proc-macros on this machine)", ErrDependencyPolicy, source, name, field)
	}
	if len(p.Crates) > 0 && !slices.Contains(p.Crates, name) {
		return fmt.Errorf("%w: crate %q is not in DependencyPolicy.Crates", ErrDependencyPolicy, name)
	}
	return nil
}

// WithDependencies adds Cargo dependencies to code compiled from Code, File
// or Fragments sources. They are only used if the DependencyPolicy allows
// them. Resolved versions are recorded in a lockfile stored in the cache,
// so builds stay reproducible until the cache is cleared.
func WithDependencies(deps ...Dependency) SharedOption {
	return sharedOption(func(c *config) error {
		for _, d := range deps {
			if d.Path != "" && !filepath.IsAbs(d.Path) {
				abs, err := filepath.Abs(d.Path)
				if err != nil {
					return fmt.Errorf("ferrogo: dependency %s: %w", d.Name, err)
				}
				d.Path = abs
			}
			d.Features = slices.Clone(d.Features)
			if err := toCompilerDep(d).Validate(); err != nil {
				return fmt.Errorf("ferrogo: %w", err)
			}
			c.deps = append(c.deps, d)
		}
		return nil
	})
}

// WithDependencyPolicy sets the dependency policy (default: no dependencies).
func WithDependencyPolicy(p DependencyPolicy) Option {
	return runtimeOption(func(c *config) error {
		if p.Network && !p.AllowRegistry && !p.AllowGit {
			return errors.New("ferrogo: DependencyPolicy.Network has no effect without AllowRegistry or AllowGit")
		}
		p.Crates = slices.Clone(p.Crates)
		c.depPolicy = p
		return nil
	})
}

func toCompilerDep(d Dependency) compiler.Dependency {
	return compiler.Dependency{
		Name: d.Name, Version: d.Version, Features: d.Features, NoDefaultFeatures: d.NoDefaultFeatures,
		Path: d.Path, Git: d.Git, Branch: d.Branch, Tag: d.Tag, Rev: d.Rev, Registry: d.Registry,
	}
}
