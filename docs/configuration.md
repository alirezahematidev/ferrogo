# Configuration

There is one source of truth: options passed to `ferrogo.New` (and per call
to `Run`/`Build`). A `ferrogo.toml` file is just a list of options.
`ferrogo.WithConfigFile(path)` applies it in place, so options passed after
it win. The CLI loads `./ferrogo.toml` automatically (or `--config path`) and
applies command-line flags on top.

FerroGo does not read any `FERROGO_*` environment variables. Nothing changes
behaviour implicitly.

## `ferrogo.toml` schema (version 1)

Unknown keys are errors. Relative paths are resolved against the file's
directory. Durations use Go syntax (`"500ms"`, `"10s"`, `"5m"`).
`ferrogo init` writes an annotated template.

| Key | Type | Option | Default |
| --- | --- | --- | --- |
| `version` | int, **required**, must be `1` | | |
| `backend` | `auto` \| `wasm` \| `native-process` \| `native-library` | `WithBackend` | `auto` (WASM) |
| `trust` | `untrusted` \| `trusted` | `WithTrust` | `untrusted` |
| `cache` | bool | `WithCache` | `true` |
| `cache_dir` | path | `WithCacheDir` | `os.UserCacheDir()/ferrogo` |
| `cache_max_bytes` | int | `WithCacheMaxBytes` | 2 GiB (`0` = unlimited) |
| `[toolchain] rustc`, `cargo` | path or name | `WithToolchain` | auto-detect |
| `[toolchain] toolchain` | rustup toolchain name | `WithToolchain` | rustup default |
| `[toolchain] rustup_home`, `cargo_home` | path | `WithToolchain` | rustup/cargo defaults |
| `[compiler] edition` | `2015`–`2024` | `WithEdition` | `2021` |
| `[compiler] profile` | `release` \| `debug` | `WithProfile` | `release` |
| `[compiler] opt_level` | `0`–`3`, `s`, `z` | `WithOptLevel` | profile default |
| `[compiler] rustflags` | string array | `WithRustFlags` | none |
| `[compiler] timeout` | duration | `WithCompileTimeout` | `5m` (`0s` = none) |
| `[compiler] max_concurrent_builds` | int ≥ 1 | `WithMaxConcurrentBuilds` | GOMAXPROCS/2 |
| `[compiler] env` | table | `WithBuildEnv` | none |
| `[execution] timeout` | duration | `WithTimeout` | `30s` (`0s` = none) |
| `[execution] max_output_size` | int (bytes) | `WithMaxOutput` | 4 MiB (`0` = unlimited) |
| `[execution] memory_limit` | int (bytes) | `WithMemoryLimit` | backend default |
| `[execution] env` | table | `WithEnv` | none |
| `[execution] inherit_env` | string array | `WithInheritEnv` | none |
| `[execution] dir` | path | `WithDir` | private temp dir per run |
| `[execution] grace_period` | duration | `WithGracePeriod` | `2s` |
| `[dependency_policy] allow_registry`, `allow_git`, `allow_path`, `network` | bool | `WithDependencyPolicy` | all `false` |
| `[dependency_policy] crates` | string array | `WithDependencyPolicy` | any allowed crate |
| `[[dependencies]] name`, `version`, `features`, `default_features`, `path`, `git`, `branch`, `tag`, `rev`, `registry` | | `WithDependencies` | none |
| `[wasm] memory_limit` | int (bytes) | `WithWASMMemoryLimit` | 256 MiB |
| `[[wasm.mounts]] host`, `guest`, `read_only` | path, absolute guest path, bool (default `true`) | `WithMount` | no mounts |

## Example

```toml
version = 1
backend = "native-process"
trust = "trusted"

[compiler]
edition = "2021"
profile = "release"
timeout = "2m"

[execution]
timeout = "10s"
max_output_size = 1048576
inherit_env = ["LANG"]
```
