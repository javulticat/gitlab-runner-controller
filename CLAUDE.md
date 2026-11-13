# GitLab Runner Controller

Project mode: **Production**. This Go service has one active owner of a durable
local state file. The released version is **v0.1.0**.

Read these documents before changing behavior:

- `README.md`: setup and development commands.
- `docs/architecture.md`: scheduling, verified demand, ownership, both lifecycles, and recovery.
- `docs/configuration.md`: JSON schema, environment mappings, pools, and opaque runner settings.
- `docs/providers.md`: language-neutral command protocol and adapter responsibilities.
- `docs/deployment.md`: portable hosting, Compose, adapter packaging, and storage.
- `docs/operations.md`: diagnostics, backoff, ambiguity, release confirmation, and backups.
- `docs/migration.md`: state compatibility and safe resource adoption.
- `docs/provenance.md`: release source, dependency records, and verification scope.
- `docs/implementation-plan.md`: implemented design decisions and boundaries.
- `CHANGELOG.md`, `CONTRIBUTING.md`, `SECURITY.md`: public project policies.

Use `make check` and `make vuln`. The former includes formatting, vet,
Staticcheck, workflow syntax, race tests, coverage, and the binary build. Tools
are pinned as Go tool dependencies. Run `make container` and `make smoke` for
image changes. Tests use local fakes and HTTP fixtures; do not contact real
projects or mutate hosting resources.

The composition root is `cmd/gitlab-runner-controller`. `pkg/provider` defines
the contract and server SDK; `internal/providers/registry.go` constructs the
generic command driver. Hosting adapters are external operator-installed
executables. Keep provider-specific names, endpoints, SDKs, deployment manifests,
and runtime assumptions out of this public repository. Controller hosting belongs
under `deploy/`; core scheduling must stay vendor-independent.

Preserve retained and disposable semantics, stable account/scope bindings, pinned
resource placement and runner IDs, durable pending-attempt budgets, and complete
inventory checks. Do not silently replace missing
resources, rebind ownership, or replay uncertain creations. Never put secrets in
argv, controller state, status, metrics, logs, or error messages. Keep the
AGPL-3.0-only license. `AGENTS.md` remains a symlink to this file.
