# Controller deployment

Every host needs an always-running Linux or macOS process, HTTPS ingress for Job
Hooks, outbound access to GitLab and the adapter's services, and a persistent local
volume. Run exactly one active process against its state file. Stop the old
instance before starting a replacement, or ensure both use the same locked
volume. Independent replicas with independent volumes cannot coordinate ownership.

The external hosting adapter is part of the operator's deployment. Install its
executable and dependencies, configure the command JSON array, and map only the
credentials and settings it requires. No adapter is bundled with the stock image.

## Release image and source builds

Set `CONTROLLER_VERSION` to the desired tag from the
[release tags](https://github.com/javulticat/gitlab-runner-controller/tags).
Images support `linux/amd64` and `linux/arm64`; Docker selects the host
architecture automatically:

```sh
docker pull "ghcr.io/javulticat/gitlab-runner-controller:${CONTROLLER_VERSION:?set CONTROLLER_VERSION to a release tag}"
docker run --rm --network none "ghcr.io/javulticat/gitlab-runner-controller:$CONTROLLER_VERSION" version
```

The command prints the selected tag. Pin the image by digest when deployments
require an exact artifact. Install or mount a compatible adapter before running
the service.

From a clean release-tagged source checkout, `make build` and `make container`
produce binaries that report that tag. Untagged or modified checkouts report `dev`.
`make container` supplies the native binary's version as a Docker build argument.
For a direct release build from the same checkout, pass it explicitly:

```sh
docker build --build-arg VERSION="$CONTROLLER_VERSION" -t "gitlab-runner-controller:$CONTROLLER_VERSION" .
```

Direct Docker builds default to `dev` because Git metadata is excluded from the
build context. The release workflow passes the verified tag as `VERSION` and
publishes both architectures only after CI succeeds. Release OCI labels identify
the source repository, commit, version, creation time, and AGPL-3.0-only license.

## Docker Compose

Prepare `controller.json` and `.env` as described in the README. Install an adapter
binary built for the image's Linux architecture and set `RUNNER_ADAPTER_PATH` to
its absolute host path in `.env` or the invoking shell. Then run from repository
root:

```sh
docker compose --env-file .env -f deploy/compose/compose.yaml config --quiet
docker compose --env-file .env -f deploy/compose/compose.yaml up -d
docker compose --env-file .env -f deploy/compose/compose.yaml logs -f
```

The recipe pins a release image, mounts that executable read-only at
`/opt/runner-host-adapter`, and
overrides `HOST_A_COMMAND_JSON` to `["/opt/runner-host-adapter"]`. The binary must
be executable by UID/GID `65532` and compatible with the stock scratch image.
A macOS binary cannot run in the Linux container. For an adapter requiring a
shell, interpreter, CLI, shared libraries, or certificates beyond the image's
contents, build a derived image that installs those dependencies and adjust the
recipe and command accordingly.

The controller runs as UID/GID `65532` with a read-only root filesystem and a
named volume at `/data`. Docker initializes a new named volume with the image
directory's ownership. For an existing volume or bind mount, arrange UID/GID
`65532` write access before starting. Do not mount a Docker socket or grant root
access unless the selected adapter independently requires it and you accept that
privilege boundary.

The supplied recipe binds port 3000 to loopback. Put a TLS reverse proxy in front
of it or deliberately change the bind address for your network. Route
`/webhook/gitlab`, and restrict administrative routes at ingress in addition to
bearer authentication. The image's liveness probe requires no shell or curl.
Use `/healthz` for restart policy and `/readyz` for dependency monitoring;
readiness can be 503 during backoff while liveness remains 200.

The explicit `--env-file .env` supplies Compose interpolation for
`RUNNER_ADAPTER_PATH`; service `env_file` alone does not supply it. Keep these
commands rooted in the repository.

Stop with `docker compose --env-file .env -f deploy/compose/compose.yaml down`.
The named volume is retained. Do not remove it while it contains managed IDs or
unresolved intents. The recipe provides a 90-second shutdown grace for the
default 50-second `request_timeout`. Increase the host grace whenever increasing
that timeout.

## Other container hosts

Build the standard image and install or mount an adapter, or supply a derived
image. Configure these platform settings:

1. Run one instance using the image entry point, without a startup shell wrapper.
2. Route the HTTP port supplied through `PORT` and terminate TLS at ingress.
3. Mount a private local persistent volume writable by UID/GID `65532` and set
   `STORAGE_DIR` to that directory. Storage must support locks and atomic rename.
4. Mount `controller.json` or set the complete non-secret document through
   `CONTROLLER_CONFIG_JSON`. Supply GitLab and adapter secrets separately through
   the host's protected environment mechanism.
5. Deliver SIGTERM and allow at least `request_timeout + 30s` for shutdown
   (80 seconds with defaults; the Compose recipe allows 90 seconds). Prevent
   overlapping instances on independent volumes during rollout.
6. Verify health, protected status, the expected account binding, and a durable
   state file after a controlled restart. Back up state outside the app volume.

Worker-local storage may not follow an application moved to another host. Move
state deliberately with the service stopped; do not bootstrap a fresh empty
volume over resources already owned by an older deployment.

## Native service

Build the controller for Linux or macOS and run it under an unprivileged account.
Give only its state directory write access, install an executable adapter at an
absolute path, and load secrets through the service manager's protected
configuration. Use a restart-on-failure policy and deliver termination signals
to the controller. No shell is evaluated by the adapter driver.

Shutdown leaves runner hosts intact. It stops new dispatch and lets the active
bounded request finish and save under a separate work context. HTTP drains for
up to 20 seconds in parallel. If work exceeds `request_timeout` plus five seconds
for cleanup, the controller cancels it, joins the worker, finishes durable writes,
and releases the state lock. A lost or failed create response remains
uncertain and requires an audit; a normal deployment shutdown allows that
response to finish. Runner lifecycle follows verified demand after the next
controller starts.
