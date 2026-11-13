# Changelog

## v0.1.0 — 2026-11-13

Initial release.

- Provider-neutral prioritized pools with validated JSON configuration and opaque
  adapter-defined compute and runner settings.
- Version 1 command-provider JSON protocol and Go server SDK, with direct
  executable invocation, allowlisted environment variables, bounded messages,
  deadlines, and process-group cancellation.
- Retained drain/stop/resume and disposable drain/destroy lifecycles, complete
  inventory confirmation, account capacity limits, and stable ownership bindings.
- Complete GitLab job discovery, verified webhook hints, full tag matching,
  runner assignment tracking, and read-only runner eligibility checks.
- Durable bounded attempts for protected pending jobs, useful-ready time budgets,
  host-start limits, and audited retry authorization.
- Version 1 locked atomic state with strict validation, pinned resource
  placement, explicit adoption, and durable lifecycle and drain progress.
- Audited recovery for uncertain creations and missing resources, with private
  backups and inventory checks.
- Per-account backoff, persistent readiness deadlines, safe resume reservations,
  and graceful shutdown that lets active bounded requests finish and save.
- Bearer-protected status and Prometheus metrics, readiness and liveness probes,
  and local configuration validation.
- Version-aware native builds and installation, non-root container images for
  Linux amd64 and arm64, OCI metadata, and verification-gated GHCR publication.
- Generic Compose deployment, fixture-based container smoke tests, GitHub Actions
  verification, and operational documentation.
- AGPL-3.0-only licensing and private vulnerability reporting.
