# Releasing and publishing on pkg.go.dev

pkg.go.dev has no upload step. It documents any module version that the
public Go module proxy (`proxy.golang.org`) has fetched. Publishing
therefore means pushing a semver tag to the public repository, then asking
the proxy to fetch it. The release workflow does that last step
automatically.

## Before the first release

1. Create the **public** repository `github.com/alirezahematidev/ferrogo`.
   The module path in `go.mod` must match it exactly.
2. Push the code:

   ```sh
   git add -A
   git commit -m "Initial release"
   git branch -M main
   git remote add origin git@github.com:alirezahematidev/ferrogo.git
   git push -u origin main
   ```

3. In the repository settings:
   * enable **private vulnerability reporting** (Security → Advisories);
   * under Actions → General, allow workflows to create releases
     (read and write permissions).
4. Wait for CI to pass on all platforms. This is the first time Linux and
   Windows run the integration suite.

## Each release

1. Make sure `main` is green, then move the *Unreleased* entries in
   `CHANGELOG.md` under a new version heading with today's date.
2. Run the local checks:

   ```sh
   make lint race examples
   make release-check   # optional: needs goreleaser v2 installed
   ```

3. Tag and push:

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

4. The **release** workflow:
   1. runs the unit tests;
   2. builds CLI archives with SHA-256 checksums for linux, darwin and windows
      (amd64 and arm64) with GoReleaser;
   3. creates a GitHub release (marked pre-release for v0.x);
   4. runs `go list -m github.com/alirezahematidev/ferrogo@v0.1.0` against
      `proxy.golang.org`, which makes the version appear on pkg.go.dev
      within minutes.
5. Copy the CHANGELOG section into the GitHub release notes, then check
   <https://pkg.go.dev/github.com/alirezahematidev/ferrogo>. If the page
   is missing, open it and click **Request**.

## Rules

* **Versions are permanent.** The proxy caches every fetched version
  forever. Never move or delete a pushed tag. If a release is broken,
  publish a fixed patch release and retract the bad one in `go.mod`:

  ```go
  retract v0.1.1 // corrupted cache keys; use v0.1.2
  ```

* **v0.x** may break the API in minor releases, with each break listed
  under *Changed* in the CHANGELOG.
* **v1.0.0** freezes the API. A later breaking change requires a `/v2`
  module path.
* Users install the CLI with
  `go install github.com/alirezahematidev/ferrogo/cmd/ferrogo@latest`, or
  download the binaries from the GitHub release.
