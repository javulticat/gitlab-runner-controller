# Contributing

Use the Go version declared in `go.mod` or newer. Read `docs/architecture.md` and
the documents relevant to your change. Keep the controller provider-neutral:
external hosting adapters own SDKs, endpoints, bootstrap, executors, and runtime
settings. Public examples and deployment recipes must remain generic.

Preserve fail-closed behavior for incomplete demand, missing resources, account
drift, and ambiguous mutations. Cover both retained and disposable lifecycles
when changing shared scheduling. Exercise behavior with deterministic fakes and
local HTTP services; ordinary tests must never require real credentials or
mutate hosting resources.

Run `make check` and `make vuln` before submitting a change. For container changes,
run `make container` and `make smoke` to exercise the non-root, read-only runtime,
external adapter packaging, durable volume, and recovery. Update documentation
and `CHANGELOG.md` when behavior changes. Use focused commits that explain the
problem and resulting behavior.

Protocol changes must preserve the versioned wire contract or explicitly
introduce a new version. Keep the language-neutral protocol documentation and Go
server SDK consistent. Test malformed messages, output bounds, cancellation,
complete inventory, identity checks, uncertain responses, and safe error handling.
Do not add a constructor or provider SDK for a particular hosting service.

Never include credentials, state snapshots, logs containing private operational
data, machine-specific harness settings, or external adapter sources in a change.
Keep `AGPL-3.0-only` licensing. For security-sensitive
reports, follow `SECURITY.md`.

## Releases

Each release is a Git tag and Go module version, with a matching image tag at
`ghcr.io/javulticat/gitlab-runner-controller`. For each release, update the
changelog and release-pinned deployment manifests. Keep guides independent of
patch versions and update supported release lines in `SECURITY.md` when needed.
Run `make check`, `make vuln`, `make container`, and `make smoke` before tagging
the reviewed commit as `vMAJOR.MINOR.PATCH`.

Pushing a `v*` tag runs the full GitHub verification job, including a check that
the native binary reports the tag and that the container reports the same
version. Only successful verification enables the publishing job. It uses the
job-scoped `GITHUB_TOKEN` with `packages: write` to publish a version-tagged image
for `linux/amd64` and `linux/arm64`, with OCI source, revision, version, creation
time, and license labels. Branch and pull-request builds have read-only
permissions and do not publish images. Tags must name a complete semantic
version; a tag that cannot be reported by the binary fails verification.

Repository administrators must enable GitHub private vulnerability reporting
under the repository's security settings. Allow GitHub Actions to write packages
and set the GHCR package visibility to public so anonymous image pulls work.
These are hosting settings, separate from the files in this repository.
