# Contributing to FerroGo

Thanks for your interest! Bug reports, documentation fixes and code are all
welcome.

## Development setup

* Go ≥ 1.25.
* Rust ≥ 1.82 via [rustup](https://rustup.rs), plus
  `rustup target add wasm32-wasip1`. Without Rust, integration tests are
  skipped and unit tests still run.
* `go run ./cmd/ferrogo doctor` checks the toolchain.

## Before sending a change

```sh
make lint    # gofmt, go vet (linux/darwin/windows), staticcheck
make race    # all tests with the race detector
make examples
```

* Add tests for every behavior change. Integration tests that need Rust must
  skip cleanly when `rustc` is missing (see `wasmRuntime` and
  `nativeRuntime` in the tests).
* Keep every user-visible error actionable. Say what failed and how to fix
  it.
* Security-relevant behavior (trust policy, sandbox capabilities, limits,
  environment scrubbing) needs a test that proves the boundary, not just the
  happy path.
* Never use a shell to run tools. Pass arguments as an argv slice.
* Never silently ignore a requested limit or option. Return
  `ErrUnsupported` instead.
* Update `CHANGELOG.md` (under *Unreleased*) and the relevant `docs/` page.
  If the change completes a roadmap item, update `docs/roadmap.md`.

## Design changes

Larger changes, such as new backends, ABI changes or public API additions,
should start as an issue that refers to the relevant section of
[docs/architecture.md](docs/architecture.md) or [docs/abi.md](docs/abi.md).

## Commit messages

Use the imperative mood ("Add WASM mounts", not "Added …"). Reference
issues where relevant.

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
