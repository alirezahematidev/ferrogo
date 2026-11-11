# Security policy

## Supported versions

FerroGo is pre-1.0. Security fixes are released for the latest minor
version only.

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub's
[private vulnerability reporting](https://github.com/alirezahematidev/ferrogo/security/advisories/new).
Do not open a public issue.

Include the FerroGo version (`ferrogo version`), your OS and architecture,
the backend involved, and a minimal reproduction. You will receive an
acknowledgement within 7 days. A fix or mitigation plan follows once the
report is confirmed.

## Scope

In scope are bugs that break a documented guarantee in
[docs/security.md](docs/security.md), for example:

* WASM guest code reaching the host filesystem, network or environment
  beyond what was granted;
* a limit (time, memory, output) that can be bypassed;
* `BackendAuto` or an `Untrusted` runtime executing native code;
* host environment variables leaking into compilation;
* shell or manifest injection through options or file names.

Out of scope are the limitations documented in docs/security.md. In
particular, native backends are not sandboxed, and `include_str!` can read
files at compile time.
