# Design decisions and implementation boundaries

This record describes the controller contract and the reasons for its
boundaries. Build and test output establish verification status; the architecture
and operational documents describe supported behavior.

## Hosting adapters are external commands

One generic subprocess driver implements the provider interface through a
versioned JSON stdin/stdout protocol. Operators install adapters separately. This
keeps vendor SDKs, endpoints, runner runtimes, bootstrap scripts, and credentials
out of the public controller. Adapters can use any language; Go has a small server
SDK in `pkg/provider`.

The process boundary uses direct argv, explicitly mapped environment variables,
bounded messages, safe error kinds and optional public call diagnostics,
cancellation deadlines, and process-group
termination. The stock image remains minimal; adapter runtime dependencies are an
operator packaging responsibility.

## Resource lifetime is explicit

Retained hosts preserve identity and storage, drain and stop when idle, and later
resume. Disposable hosts drain and destroy; they never use retained-host actions.
The shared scheduler owns demand, capacity, durable intent, and release
confirmation. It does not duplicate those decisions in adapters.

A later explicit stopped observation releases retained capacity. Disposable
capacity is released only after requested destruction and later authoritative
absence from complete current inventory. Missing resources at other times remain
unknown. An explicit offline missing-resource audit can release a known absent
ID after fresh inventory, a private synchronized backup, and a durable audit
record; uncertain creation without an ID uses a separate recovery command.
Count-based account capacity includes unmanaged resources and unresolved
reservations; additional quota models remain the adapter/operator's responsibility.

## Ownership precedes mutation

Expected stable account identity is independent of rotating credentials. The
adapter describes its account and endpoint/region scope, and state pins the
binding before lifecycle work. Repeated description checks reject account drift.
Duplicate account aliases cannot split capacity accounting. Saved resource
placement options and numeric GitLab runner IDs cannot silently change while
handles exist. A provider failure or backoff blocks that account's work;
independent accounts continue and overall readiness reports the failure.

The controller uses state schema version 1. Startup rejects unsupported,
unversioned, and incomplete snapshots without rewriting them. Taking over existing
resources requires a controlled cutover with explicit ordered adoption IDs;
the previous owner's snapshot remains separate and private.

## Durable intent handles uncertainty

Every external creation and release has a durable intent first. An ambiguous
create stays unresolved until an operator audit; a lost reply is not evidence of
failure. Drain progress persists so destruction recovery does not depend on a
manager that may already be gone. Storage failure stops the process rather than
advancing only in memory.

The complete JSON snapshot and local process lock fit a single active controller.
Active-active operation would need distributed ownership and a transactional
shared store, which is a separate deployment model.

## Verified GitLab demand drives scheduling

Hooks are durable hints. Complete API snapshots and individual job confirmation
establish demand; revision checks prevent a new hint from racing a lifecycle
decision. An individual job 404 removes a deleted job only after its project's
complete authorized listing succeeds. Read-only runner registration and complete
project runner availability checks precede pending scale-out. Full tag
compatibility selects a unique pool. Actual runner assignment protects busy
work even if tags change; unrelated runner jobs do not count. Unknown assignment
and ambiguous matches protect existing capacity without allocating hosts.

Refresh throttling and racing hints are normal deferrals. They preserve completed
reconciliation status and service readiness while observations remain fresh,
without weakening verified-demand checks for lifecycle changes. A dedicated
refresh timer honors the persisted minimum interval and GitLab backoff, so a
deferred hint does not have to wait for a poll tick. Startup still requires a
successful live reconciliation; real failures remain visible.

Protected pending eligibility remains unknown because current ref policy cannot
prove the job's persisted eligibility. The default policy saves one bounded
attempt per runner and job before activation. Time counts only with healthy
ready spare capacity, and exhaustion requires a complete observation after the
window. A spent attempt cannot hold idle capacity or recreate hosts; restart,
hooks, and pool renaming do not reset it. Operators can select strict policy or
authorize an audited additional attempt. Pools scale in only when wholly idle,
and adapters drain without interrupting assigned work.

Runner JSON is opaque to the core. Configuration revisions apply on the next
configured manager; healthy managers retain their deployed settings until idle
release. The controller never chooses an executor or job isolation policy for an
operator.

Runner readiness is distinct from resource power. A persisted bootstrap deadline
covers creation, resume, running adoption, and health recovery without repeated
extension. Expiry records a visible error and retry backoff. Readiness metrics
and status expose the difference without changing the existing instance labels.

Termination stops new dispatch and allows the bounded active operation to save
before forced cancellation. HTTP drains in parallel. The host shutdown grace
must cover `request_timeout + 30s`; normal rollout must not manufacture uncertain
creates by immediately canceling their replies.

## Scope and verification

The public service supplies the binary, protocol SDK, generic container/Compose
packaging, example configuration, resource adoption and recovery commands, documentation,
CI, and AGPL-3.0-only licensing. External adapters and infrastructure deployments
have their own implementation and verification responsibilities.

Local tests cover contract behavior with fakes, uncertainty, cancellation,
ownership, inventory completeness, both lifecycles, storage, HTTP, and backoff.
They must not contact configured production projects or mutate hosting resources.
Use the documented `make check`, `make vuln`, container, and smoke targets before
claiming a change verified.
