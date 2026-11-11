package ferrogo

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ConfigFileName is the conventional project configuration file name.
const ConfigFileName = "ferrogo.toml"

// configSchemaVersion is the only accepted value of the top-level "version".
const configSchemaVersion = 1

// fileConfig is the ferrogo.toml schema (docs/configuration.md). Pointer
// fields distinguish "absent" from zero values.
type fileConfig struct {
	Version       int     `toml:"version"`
	Backend       *string `toml:"backend"`
	Trust         *string `toml:"trust"`
	Cache         *bool   `toml:"cache"`
	CacheDir      *string `toml:"cache_dir"`
	CacheMaxBytes *int64  `toml:"cache_max_bytes"`

	Toolchain struct {
		Rustc      *string `toml:"rustc"`
		Cargo      *string `toml:"cargo"`
		Toolchain  *string `toml:"toolchain"`
		RustupHome *string `toml:"rustup_home"`
		CargoHome  *string `toml:"cargo_home"`
	} `toml:"toolchain"`

	Compiler struct {
		Edition             *string           `toml:"edition"`
		Profile             *string           `toml:"profile"`
		OptLevel            *string           `toml:"opt_level"`
		RustFlags           []string          `toml:"rustflags"`
		Timeout             *duration         `toml:"timeout"`
		MaxConcurrentBuilds *int              `toml:"max_concurrent_builds"`
		Env                 map[string]string `toml:"env"`
	} `toml:"compiler"`

	Execution struct {
		Timeout       *duration         `toml:"timeout"`
		MaxOutputSize *int64            `toml:"max_output_size"`
		MemoryLimit   *int64            `toml:"memory_limit"`
		Env           map[string]string `toml:"env"`
		InheritEnv    []string          `toml:"inherit_env"`
		Dir           *string           `toml:"dir"`
		GracePeriod   *duration         `toml:"grace_period"`
	} `toml:"execution"`

	DependencyPolicy *struct {
		AllowRegistry bool     `toml:"allow_registry"`
		AllowGit      bool     `toml:"allow_git"`
		AllowPath     bool     `toml:"allow_path"`
		Network       bool     `toml:"network"`
		Crates        []string `toml:"crates"`
	} `toml:"dependency_policy"`

	Dependencies []struct {
		Name            string   `toml:"name"`
		Version         string   `toml:"version"`
		Features        []string `toml:"features"`
		DefaultFeatures *bool    `toml:"default_features"`
		Path            string   `toml:"path"`
		Git             string   `toml:"git"`
		Branch          string   `toml:"branch"`
		Tag             string   `toml:"tag"`
		Rev             string   `toml:"rev"`
		Registry        string   `toml:"registry"`
	} `toml:"dependencies"`

	WASM struct {
		MemoryLimit *int64 `toml:"memory_limit"`
		Mounts      []struct {
			Host     string `toml:"host"`
			Guest    string `toml:"guest"`
			ReadOnly *bool  `toml:"read_only"`
		} `toml:"mounts"`
	} `toml:"wasm"`
}

type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q (examples: \"10s\", \"5m\")", b)
	}
	d.Duration = v
	return nil
}

// WithConfigFile loads settings from a ferrogo.toml file. Unknown keys are
// errors. Relative paths in the file are resolved against the file's
// directory. Options after WithConfigFile override values from the file.
func WithConfigFile(path string) Option {
	return runtimeOption(func(c *config) error {
		opts, err := parseConfigFile(path)
		if err != nil {
			return err
		}
		for _, o := range opts {
			if err := o.applyRuntime(c); err != nil {
				return fmt.Errorf("ferrogo: %s: %w", path, err)
			}
		}
		return nil
	})
}

func parseConfigFile(path string) ([]Option, error) {
	var f fileConfig
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return nil, fmt.Errorf("ferrogo: config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("ferrogo: config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if f.Version != configSchemaVersion {
		return nil, fmt.Errorf("ferrogo: config %s: version = %d is not supported (want version = %d)", path, f.Version, configSchemaVersion)
	}
	base := filepath.Dir(path)
	rel := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}

	var opts []Option
	add := func(o Option) { opts = append(opts, o) }
	if f.Backend != nil {
		b, err := ParseBackend(*f.Backend)
		if err != nil {
			return nil, err
		}
		add(WithBackend(b))
	}
	if f.Trust != nil {
		t, err := ParseTrustLevel(*f.Trust)
		if err != nil {
			return nil, err
		}
		add(WithTrust(t))
	}
	if f.Cache != nil {
		add(WithCache(*f.Cache))
	}
	if f.CacheDir != nil {
		add(WithCacheDir(rel(*f.CacheDir)))
	}
	if f.CacheMaxBytes != nil {
		add(WithCacheMaxBytes(*f.CacheMaxBytes))
	}

	t := f.Toolchain
	if t.Rustc != nil || t.Cargo != nil || t.Toolchain != nil || t.RustupHome != nil || t.CargoHome != nil {
		var tc ToolchainConfig
		set := func(dst *string, src *string, isPath bool) {
			if src == nil {
				return
			}
			*dst = *src
			// Bare names ("rustc") are looked up on PATH; only paths are re-based.
			if isPath && strings.ContainsAny(*src, `/\`) {
				*dst = rel(*src)
			}
		}
		set(&tc.Rustc, t.Rustc, true)
		set(&tc.Cargo, t.Cargo, true)
		set(&tc.Toolchain, t.Toolchain, false)
		if t.RustupHome != nil {
			tc.RustupHome = rel(*t.RustupHome)
		}
		if t.CargoHome != nil {
			tc.CargoHome = rel(*t.CargoHome)
		}
		add(WithToolchain(tc))
	}

	cc := f.Compiler
	if cc.Edition != nil {
		add(WithEdition(*cc.Edition))
	}
	if cc.Profile != nil {
		add(WithProfile(Profile(*cc.Profile)))
	}
	if cc.OptLevel != nil {
		add(WithOptLevel(*cc.OptLevel))
	}
	if len(cc.RustFlags) > 0 {
		add(WithRustFlags(cc.RustFlags...))
	}
	if cc.Timeout != nil {
		add(WithCompileTimeout(cc.Timeout.Duration))
	}
	if cc.MaxConcurrentBuilds != nil {
		add(WithMaxConcurrentBuilds(*cc.MaxConcurrentBuilds))
	}
	if len(cc.Env) > 0 {
		add(WithBuildEnv(cc.Env))
	}

	ex := f.Execution
	if ex.Timeout != nil {
		add(WithTimeout(ex.Timeout.Duration))
	}
	if ex.MaxOutputSize != nil {
		add(WithMaxOutput(*ex.MaxOutputSize))
	}
	if ex.MemoryLimit != nil {
		add(WithMemoryLimit(*ex.MemoryLimit))
	}
	if len(ex.Env) > 0 {
		add(WithEnv(ex.Env))
	}
	if len(ex.InheritEnv) > 0 {
		add(WithInheritEnv(ex.InheritEnv...))
	}
	if ex.Dir != nil {
		if *ex.Dir == "" {
			return nil, errors.New("ferrogo: config: execution.dir must not be empty")
		}
		add(WithDir(rel(*ex.Dir)))
	}
	if ex.GracePeriod != nil {
		add(WithGracePeriod(ex.GracePeriod.Duration))
	}

	if p := f.DependencyPolicy; p != nil {
		add(WithDependencyPolicy(DependencyPolicy{
			AllowRegistry: p.AllowRegistry, AllowGit: p.AllowGit, AllowPath: p.AllowPath,
			Network: p.Network, Crates: p.Crates,
		}))
	}
	for _, d := range f.Dependencies {
		path := d.Path
		if path != "" {
			path = rel(path)
		}
		add(WithDependencies(Dependency{
			Name: d.Name, Version: d.Version, Features: d.Features,
			NoDefaultFeatures: d.DefaultFeatures != nil && !*d.DefaultFeatures,
			Path:              path, Git: d.Git, Branch: d.Branch, Tag: d.Tag, Rev: d.Rev, Registry: d.Registry,
		}))
	}

	if f.WASM.MemoryLimit != nil {
		add(WithWASMMemoryLimit(*f.WASM.MemoryLimit))
	}
	for _, m := range f.WASM.Mounts {
		// Mounts are read-only unless the file says otherwise.
		readOnly := m.ReadOnly == nil || *m.ReadOnly
		add(WithMount(Mount{HostPath: rel(m.Host), GuestPath: m.Guest, ReadOnly: readOnly}))
	}
	return opts, nil
}

// ConfigTemplate is the annotated ferrogo.toml written by `ferrogo init`.
const ConfigTemplate = `# FerroGo project configuration. See docs/configuration.md.
version = 1

# auto | wasm | native-process | native-library
# "auto" always means the sandboxed WASM backend.
backend = "auto"

# untrusted | trusted. Native backends require "trusted": they run code with
# your privileges (native-process) or inside your process (native-library).
trust = "untrusted"

cache = true
# cache_dir = ".ferrogo/cache"
# cache_max_bytes = 2147483648

[toolchain]
# toolchain = "stable"        # rustup toolchain name (RUSTUP_TOOLCHAIN)
# rustc = "/path/to/rustc"
# cargo = "/path/to/cargo"

[compiler]
edition = "2021"
profile = "release"           # release | debug
# opt_level = "3"             # 0-3 | s | z
# rustflags = ["-C", "target-cpu=native"]
timeout = "5m"

[execution]
timeout = "30s"
max_output_size = 4194304      # bytes, stdout + stderr
# env = { RUST_LOG = "info" }
# inherit_env = ["LANG"]

# Dependencies are denied unless the policy allows them. They run build
# scripts and proc-macros on this machine at compile time.
# [dependency_policy]
# allow_registry = true
# network = false              # true lets cargo download crates
# crates = ["serde_json"]      # optional allowlist
#
# [[dependencies]]
# name = "serde_json"
# version = "1"
# features = ["preserve_order"]

[wasm]
memory_limit = 268435456       # bytes of guest memory (256 MiB)
# Directories visible to WASM code (none by default). Read-only unless
# read_only = false.
# [[wasm.mounts]]
# host = "./data"
# guest = "/data"
`
