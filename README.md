# GitLab Runner Controller

A portable Go service that starts runner hosts when GitLab jobs are pending and
drains them when their pool becomes idle. Retained hosts stop and resume with
their saved identity; disposable hosts are destroyed after draining.

Runner hosting uses an operator-installed command adapter through a
[versioned JSON protocol](docs/providers.md). The controller contains no cloud
SDK, executor, image, or hosting-platform assumptions. Its concurrent capacity
policy counts running and unknown account resources, including unmanaged ones.

Features include prioritized pools, complete GitLab tag matching, missed-webhook
recovery, durable backoff and lifecycle intents, explicit creation recovery,
bounded protected-job attempts, protected status and Prometheus metrics, and
fail-closed local storage.

## Installation

Install the native binary with the Go version declared in [go.mod](go.mod) or newer:

```sh
go install github.com/javulticat/gitlab-runner-controller/cmd/gitlab-runner-controller@latest
gitlab-runner-controller version
```

The version command prints the installed release tag. Add Go's binary directory
to `PATH` if needed. To pin an installation, replace `@latest` with a tag from
the [release tags](https://github.com/javulticat/gitlab-runner-controller/tags).

Container images use the same release tags at
`ghcr.io/javulticat/gitlab-runner-controller:<version>` and support `linux/amd64`
and `linux/arm64`. See [deployment](docs/deployment.md) for adapter installation
and persistent storage.

## Quick start

Requirements: Linux or macOS, an adapter that implements the provider protocol,
credentials for its account, and a GitLab personal,
project, or group access token with the `read_api` scope. The token's user needs
access to every configured project and at least the Maintainer or Auditor role
for project runner availability.
Create a **project runner** for each pool. Save its numeric runner ID from the
GitLab UI and its matching `glrt-` authentication token. Set its tags to the
pool's complete tag set, disable untagged jobs, and set protected-runner
restrictions in GitLab. See [credentials](docs/configuration.md#credentials) for
the read-only checks and protected-job limits.

Clone the release tag matching the installed binary for the configuration examples:

```sh
CONTROLLER_VERSION=$(gitlab-runner-controller version)
git clone --branch "$CONTROLLER_VERSION" --depth 1 https://github.com/javulticat/gitlab-runner-controller.git
cd gitlab-runner-controller
cp controller.example.json controller.json
cp .env.example .env
```

Edit `controller.json` and `.env`: replace project `123`, the expected account
identity, adapter command and environment mappings, runner IDs, pools, and
capacity policy.
The examples are placeholders; an external adapter is required. Read its own
documentation for compute options and runner configuration. Use an absolute
executable path and keep credentials out of command arguments.

The application does not read `.env`; load it through your shell or host:

```sh
set -a
. ./.env
set +a
gitlab-runner-controller check-config
gitlab-runner-controller serve
```

`check-config` validates configuration, environment mappings, and the executable
file without executing the adapter or contacting services. Normal startup checks
the adapter's account identity, scope, and lifecycle before any mutation.

Expose the service through HTTPS, then add a GitLab project webhook pointing to
`https://YOUR_HOST/webhook/gitlab`. Enable **Job events** and use
`GITLAB_WEBHOOK_SECRET` as its secret token. Periodic API polling recovers missed
delivery.

```sh
curl -fsS http://localhost:3000/healthz
curl -fsS http://localhost:3000/readyz
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:3000/status
```

Run one active process per durable local state directory. See
[deployment](docs/deployment.md) for Compose and container requirements, including
installation of the external adapter in the shell-free stock image. Taking over
existing resources requires [explicit adoption](docs/migration.md).

## Development and verification

```sh
make check
make vuln
make container
make smoke
```

`make check` checks formatting, vet, Staticcheck, workflow syntax, race tests,
coverage, and the binary build. Dependencies and tools are pinned in
`go.mod`/`go.sum`. GitHub Actions runs `make check` and `make vuln`, builds the
container, and runs its smoke test. Tests use local fakes and HTTP servers; they
do not provision resources or need real credentials.
The container smoke test requires Bash, curl, jq, and Docker. It uses a generic
fake adapter and fake services and removes its own test containers and volume.

Build from a source checkout with `make build`. A clean release checkout reports
its tag; untagged or modified source builds report `dev`. `make container` passes
the same version to Docker. Direct Docker builds need the matching release tag
in `--build-arg VERSION=<version>` because their context excludes Git metadata.
Without that argument they report `dev`.

Pushing a `v*` tag runs all GitHub verification before publishing the versioned
multi-platform image to GHCR. Its OCI labels include source, revision, version,
creation time, and license. See [contributing](CONTRIBUTING.md#releases) for the
release process.

## Documentation

- [Architecture and lifecycles](docs/architecture.md): ownership, demand, durable operations, and capacity.
- [Configuration](docs/configuration.md): settings, credentials, adapter commands, and pools.
- [Provider protocol](docs/providers.md): language-neutral wire contract and adapter responsibilities.
- [Deployment](docs/deployment.md): Compose, container packaging, and host requirements.
- [Operations](docs/operations.md): diagnostics, recovery, backups, and shutdown.
- [State compatibility and adoption](docs/migration.md): accepted snapshots and safe resource cutover.
- [Design decisions](docs/implementation-plan.md): boundaries and safety requirements.
- [Source provenance](docs/provenance.md), [changelog](CHANGELOG.md), [contributing](CONTRIBUTING.md), and [security policy](SECURITY.md).

The Go module is `github.com/javulticat/gitlab-runner-controller`.
Go adapters import `github.com/javulticat/gitlab-runner-controller/pkg/provider`;
see the [SDK compatibility policy](docs/providers.md#sdk-compatibility).

Licensed under the [GNU Affero General Public License v3.0](LICENSE)
(`AGPL-3.0-only`).
