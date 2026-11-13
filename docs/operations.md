# Operations

## Health and observability

| Route | Access | Meaning |
| --- | --- | --- |
| `GET /healthz` | Public | HTTP process is alive; no state or credentials |
| `GET /readyz` | Public | Fresh authoritative GitLab observation, usable storage, successful recent reconciliation, verified runner metadata, no unknown/error or running-not-ready slots |
| `POST /webhook/gitlab` | `X-Gitlab-Token` | Persist Job Hook hints; return 202 only after storage succeeds |
| `GET /status` | Bearer `ADMIN_TOKEN` | Pool/slot state, verified jobs, inbox revisions, operations, and backoff |
| `GET /metrics` | Bearer `ADMIN_TOKEN` | Prometheus controller and Go/process metrics |

Admin routes return 404 when `ADMIN_TOKEN` is absent. Use an independent admin
secret. Status excludes tokens but contains project, job, and resource IDs;
treat it as private operational data.

Logs are JSON on stderr. Adapter failures expose operation names and error kinds,
plus optional bounded call names, HTTP statuses, and public error codes from the
[provider diagnostic contract](providers.md#failure-diagnostics). Account and slot
errors in status include the same metadata. Raw child stdout, stderr, response
bodies, remote error messages, and command arguments are not diagnostic output.
Use the adapter's private diagnostic procedure and inspect
the resource directly when investigating bootstrap or runtime failures. Do not
paste credentials, runner configuration, or raw provider output into public issues.

Useful Prometheus metrics:

- `grc_reconciliations_total{result}` and `grc_reconciliation_duration_seconds`.
- `grc_instances{pool,state}` and `grc_jobs{status}`.
- `grc_runner_ready{pool}` and `grc_runner_not_ready{pool}` distinguish runner readiness from power state.
- `grc_bootstrap_timeouts{pool}` counts slots currently past their readiness deadline; it is a gauge, not a cumulative counter.
- `grc_bootstrap_pending_seconds{pool}` gives the longest observed readiness wait.
- `grc_unresolved_operations{pool}`.
- `grc_gitlab_last_sync_timestamp_seconds` and `grc_gitlab_backoff_until_timestamp_seconds`.

Alert on stale synchronization, unresolved creations, unknown instances, and
bootstrap errors. Queued demand at configured capacity is expected. Readiness
is false during GitLab backoff; use liveness for restart policy because restarting
cannot fix a rate limit.

New hints and the minimum refresh interval defer lifecycle work without creating
reconciliation failures, logs, or readiness changes while successful observations
remain fresh. Deferred work preserves the last completed cycle's time and error;
it does not increment reconciliation counters or duration observations. The
controller retries at the next eligible refresh deadline without waiting for a
poll tick. Initial startup still requires a successful live reconciliation, and
real failures, stale observations, and active GitLab backoff degrade readiness.
An undispatched activation does not start a bootstrap readiness deadline or slot
retry backoff; deadlines and errors from earlier real attempts remain intact.

Status includes `state.provider_errors`, each pool's `runner_error`, and slot
`missing_since`, `bootstrap_started_at`, and `bootstrap_deadline` timestamps.
These distinguish provider access failures, blocked runner registrations,
missing resources, and stalled bootstrap. A manager that stays `ready: false`
past `readiness_timeout` records an error and retries with backoff; repeated
observations do not renew the deadline. Creating, resuming, adopting a running
host, or losing health starts an interval. Assigned jobs remain protected.
Retained create and resume acknowledgments do not renew that interval or prove
readiness. If a host remains stopped past the deadline, the error stays visible
and retries wait at least the greater of `observation_grace` and
`readiness_timeout` (five minutes with the defaults). A running observation allows
configuration immediately; only confirmed runner readiness clears the failure.
Verified idle demand and fresh stopped inventory cancel an unused retained
resume only after `observation_grace` has elapsed since its most recent attempt,
including uncertain replies. Each attempt is recorded before dispatch and again
at completion, so a restart or failed retry cannot bypass that window. An expired
readiness deadline does not waive the grace for a newer retry. Cancellation clears
its bootstrap failure and capacity reservation, while keeping
the resource ID for future work. If that host starts late, normal running-host
observation, configuration, and idle draining stop it again. Missing or unknown
inventory and unverified job hints cannot cancel the reservation.
State metrics publish one complete snapshot per observation, so scrapes do not
combine old and new pool values.

## Backoff and account binding

GitLab 429 responses honor `Retry-After` seconds or HTTP dates, then
`RateLimit-Reset`. Explicit delay is bounded to one day. Without valid headers,
backoff doubles from 30 seconds to 16 minutes with up to 20% jitter. Network and
API failures also back off because demand is uncertain. Backoff and minimum
refresh timestamps persist across restarts; hooks cannot bypass them. Complete
successful synchronization clears GitLab backoff.

Adapter `retry_after_ms` persists an account-level deadline in `provider_backoff`.
All work for that account waits, including adoption, inventory, scheduling, and
other slots. Independent accounts continue; provider errors still degrade
readiness and appear in the combined reconciliation error. Restarting cannot
bypass backoff. Without an explicit delay, slot failures retry after 30 seconds.
No lifecycle action runs while job hints remain unverified or synchronization is
in backoff. Existing managers continue their work. An individual deleted-job 404
can clear that job only after a complete authorized 200 job listing for the same
project. A listing permission failure never clears known jobs.

Identity, endpoint/region scope, and lifecycle must match the saved binding before
mutations. If rotation or configuration edits select another account, restore the
expected settings. Do not edit state bindings to suppress the mismatch. Duplicate
account aliases block both aliases before mutation; compute/location and runner
ID changes with saved handles are rejected.
Migration or resource replacement must establish ownership explicitly.

## Protected pending jobs

The default `protected_pending_policy` is `probe_once`. Current protection rules
and job hooks do not prove a pending job's eligibility for a protected runner.
A uniquely matched job can activate capacity for one bounded attempt per runner
and job; status keeps eligibility unknown until GitLab assigns it. The stricter
`strict` policy never activates capacity for that unknown eligibility.

Only one protected attempt is active per pool. The controller saves it before
activation, then counts time only when a healthy ready manager has spare job
capacity. Bootstrap time does not spend the budget, and lost capacity pauses it.
After confirmed destruction, an unspent attempt keeps its remaining ready time
and can use a replacement host. `protected_probe_max_hosts` separately caps new
hosts started for a job's uncertain demand: the default is two, allowing one
bootstrap retry. Proven rejected creates do not count; uncertain creates still
require an audit before any replacement. Already available ready capacity can
finish the window without starting another host.
The default `pending_probe_timeout` is the greater of `observation_grace` and
twice `jobs_poll_interval`. Exhaustion requires a complete GitLab observation
past the budget and no unverified hints. Once exhausted, that job no longer
keeps idle capacity alive or causes automatic recreation. Jobs assigned to this
runner continue to protect capacity regardless of their tags or probe outcome.

Inspect `state.pending_probes` in protected status for the job and runner IDs,
remaining opportunity, active deadline, host count, and exhaustion reason.
`opportunity_budget` means the useful-ready window ended; `host_limit` means the
startup cap was reached without usable capacity. The latter preserves remaining
time but requires an audited `retry-pending` after the startup problem is fixed;
later spare capacity does not revive a terminal attempt. Spent attempts survive
restart, repeated hooks, and pool renaming. Investigate GitLab registration,
project availability, protected ref policy, and spending limits before granting
another attempt. Then stop the controller and use the same configuration and
state path:

```sh
./bin/gitlab-runner-controller retry-pending --pool protected --project-id 123 --job-id 456 --confirm-retry
```

This offline command requires the exclusive state lock and an exhausted attempt
for a still-pending job. It writes a private synchronized backup and an audit
record before authorizing one additional attempt. It does not change GitLab
eligibility or mutate provider resources. Preserve the reported backup path,
restart, and inspect the new observation and attempt outcome.

## Recovering an ambiguous creation

A lost create response or a controller/adapter crash can leave a real host whose
ID never reached durable state. The slot remains `unresolved`, holds a capacity
reservation, and will not automatically create another host.

1. Stop the controller so the recovery command can acquire the state lock.
2. Back up `state.json`. Audit complete inventory in the bound account/scope and
   identify the resource using the operation ID, time, pool, and the adapter's
   ownership evidence. Do not bind an unrelated host to clear the reservation.
3. If a resource exists, bind its verified ID:

   ```sh
   ./bin/gitlab-runner-controller resolve-create --pool linux --slot 0 --resource-id VERIFIED_ID
   ```

4. If the account audit proves no resource was created, release the reservation:

   ```sh
   ./bin/gitlab-runner-controller resolve-create --pool linux --slot 0 --confirm-absent
   ```

5. Restart and verify the resource against fresh inventory and job state.

The command never mutates provider resources. It refuses duplicate IDs, unknown
slots, and slots without an interrupted creation. Never confirm absence while a
resource might exist. The adapter's proven pre-side-effect rejection can release
a create reservation; a timeout or missing response cannot.

## Retained and disposable release

Retained resources drain, stop, and keep their saved ID for later resume. A stop
reply alone does not free capacity; a later inventory must explicitly show
`stopped`.

Disposable resources drain and then destroy. They are never resumed, persisted,
or stopped. Drain completion is durable, so a lost destroy reply does not require
a removed manager to drain again. Capacity and the saved ID remain reserved until
a later complete current inventory confirms absence under the adapter's declared
`absence_confirms_destroy` guarantee. A stopped but present resource still exists
and remains reserved until destruction.

Outside a requested destroy, a missing known resource stays unknown and reserved
for either lifecycle. Restore visibility or investigate the saved resource.
Status records when it first became missing. If an account audit establishes
that the exact saved ID no longer exists, stop the controller and run:

```sh
./bin/gitlab-runner-controller release-missing --pool linux --slot 0 --resource-id EXACT_SAVED_ID --confirm-absent
```

The command runs before normal startup normalization and requires the exclusive
state lock. It verifies a fresh adapter description against the saved identity,
scope, lifecycle, and absence guarantee, then requires a complete current
inventory that omits the exact ID. Any present resource, including `stopped` or
`unknown`, rejects release. A failed or incomplete inventory cannot release it.

For either retained or disposable hosting, successful release writes an exact
private synchronized backup and a durable audit record, clears the saved slot,
and preserves spent protected-job attempts. Preserve the reported backup path.
The command makes no provider mutation. After restart, fresh demand may use the
released slot. Never delete state records or skip the audit to bypass uncertainty.
An unknown creation without a saved resource ID still uses `resolve-create`.
The adapter's `absence_confirms_destroy` declaration authorizes automatic release
only after requested destruction; it does not authorize automatic missing-ID
release during normal observation.

## Backups, configuration changes, and shutdown

Snapshot replacement is atomic, so copying the current `state.json` gives a
complete version. Keep backups private and outside the application volume.
Preserve controller ID, GitLab project binding, account/scope bindings, and the
matching configuration. Stop the controller before restoring, audit remote
changes since the snapshot, and resolve uncertain creations before restarting.
Never restore a backup into a second active deployment.

A storage failure stops the service instead of advancing only in memory. Repair
disk capacity or permissions and inspect the saved snapshot before restart.
Corrupt, foreign, incomplete, and unknown-schema state is rejected without being
overwritten. The controller uses state schema version 1. No automatic conversion
is provided for unsupported snapshots. Preserve rejected files and use the documented
[resource adoption](migration.md) procedure for a controlled cutover. Snapshots
are bounded to 32 MiB for loading and writing; an oversized update fails before
replacing the last usable snapshot.

Runner settings, tokens, concurrency, and provider deployment revision apply to
the next configured manager. Healthy running managers keep their deployed
settings until idle release. Compute/location options and runner IDs are pinned
while IDs or unresolved intents exist. Plan explicit replacement or migration for those
changes. For urgent credential rotation, drain affected managers, confirm jobs
have finished, release resources according to their lifecycle, update secrets,
and reconcile. Revocation alone does not remove private configuration from an
existing host.

SIGINT/SIGTERM stops new dispatch and allows the active bounded adapter request
to finish and save. HTTP requests drain for up to 20 seconds in parallel. Forced
cancellation starts only after `request_timeout` plus five seconds for cleanup,
then shutdown joins the worker before releasing the state lock. Allow a host
grace of at least `request_timeout + 30s` (80 seconds with defaults); Compose
provides 90 seconds. Increase host grace when increasing `request_timeout`.
Commands terminate as process groups if forced cancellation is needed; remote
effects can still be uncertain after a lost reply. Durable intents prevent
untracked creation retries after restart.
Controller shutdown leaves runner hosts intact. Provider expiry and infrastructure
failures can still interrupt workloads; align retention policy and job timeouts
with the selected adapter and host.
