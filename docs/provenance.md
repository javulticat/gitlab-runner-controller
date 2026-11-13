# Release source and dependency provenance

Each controller release corresponds to its matching
[Git tag](https://github.com/javulticat/gitlab-runner-controller/tags).
The Go module and provider SDK share that version. Published images use
`ghcr.io/javulticat/gitlab-runner-controller:<version>` for `linux/amd64` and
`linux/arm64`. Its OCI labels record source, revision, version, creation time,
and license. Use an image digest to identify an exact published artifact.

The project is licensed `AGPL-3.0-only`; see [LICENSE](../LICENSE).
Dependencies retain their own licenses. Exact module versions and checksum
verification are recorded in `go.mod` and `go.sum`; Go tool dependencies are
pinned there as well. The Go toolchain version is declared in `go.mod`, and base
image references are in `Dockerfile`. Dependency changes must verify releases
against official registries or upstream sources.

Container build contexts include only the Go source, module records, example
configuration, and license. Git metadata, credentials, local state, and external
adapter sources are excluded. Hosting adapters have their own source, dependency,
and deployment records; no production adapter is bundled with this release.

## Reproducible verification

Run `make check` for formatting, vet, Staticcheck, workflow syntax, race tests,
coverage, and the binary build. Run `make vuln` for vulnerability scanning.
Image changes additionally require `make container` and
`make smoke` for non-root, read-only execution, persistent storage,
authentication, process locking, shutdown, and recovery.

The automated suite uses deterministic provider fakes and local HTTP services.
The smoke test packages a generic fake command adapter; it does not verify a
production hosting integration or provision real resources. Adapter-specific
verification belongs to that adapter's own test suite. Passing local checks
proves only the behavior those checks exercise; operators must verify their
chosen adapter and hosting environment separately.
