# Releasing

Maintainer checklist. Customers verify releases via "Verifying a release" in
the README; this file is the producing side.

1. Move the `[Unreleased]` items in CHANGELOG.md under the new version with
   the date. Anything touching the log schema gets called out explicitly.
2. Confirm a clean tree: `gofmt -l .` empty, `go vet ./...`,
   `go test -race ./...`, `git status` clean, `go.mod` still has no requires.
3. Tag with a signature and push:

   ```sh
   git tag -s v1.0.0 -m "v1.0.0"
   git push origin v1.0.0
   ```

4. The release workflow (`.github/workflows/release.yml`) takes it from
   there: re-runs vet and the race suite, builds every target with the
   reproducible flags (`CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`,
   `-ldflags "-s -w -buildid="`), writes `SHA256SUMS`, and publishes a
   release against the verified tag. `--verify-tag` means a tag that does not
   match the pushed commit publishes nothing.
5. Spot-check: download one binary, `sha256sum -c SHA256SUMS
   --ignore-missing`, and rebuild it locally with the same Go version to
   confirm the hash matches the published one.

Rules that make the pinning story true:

- Tags are immutable. A bad release gets a new tag and a CHANGELOG entry,
  never a moved tag.
- The Go version in `release.yml` and `ci.yml` move together, and the release
  notes state the version used, because reproduction requires it.
