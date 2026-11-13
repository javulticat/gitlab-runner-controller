# Configuration

The default file is `controller.json`. Select another with `--config PATH` or
`CONTROLLER_CONFIG`. `CONTROLLER_CONFIG_JSON` contains the same complete JSON
document and takes precedence over the file. Unknown top-level fields, trailing
JSON, and invalid settings fail startup. Configuration is bounded to 1 MiB.

Secrets stay in environment variables. Configuration records their names, never
their values. `.env` loading belongs to your shell or hosting platform. Run
`check-config` before deployment: it validates settings, mapped environment
values, and the command executable without starting an adapter or using a network.
Adapter-specific JSON is validated by the adapter during normal operation.

## Global settings

| JSON key | Default | Meaning |
| --- | --- | --- |
| `controller_id` | `gitlab-runner-controller` | Stable ownership ID; preserve with existing state |
| `gitlab_url` | `https://gitlab.com` | HTTPS GitLab installation URL; subpaths supported |
| `project_ids` | Required | Unique positive numeric IDs for every project these runners serve |
| `listen_addr` | `:3000` | Bind address; `PORT` overrides the port and binds all interfaces |
| `state_path` | `data/state.json` | Snapshot path; `STORAGE_DIR` overrides its directory |
| `poll_interval` | `15s` | Reconciliation tick, supplemented by hooks and GitLab refresh deadlines |
| `jobs_poll_interval` | `60s` | Normal complete GitLab synchronization interval |
| `min_refresh_interval` | `5s` | Minimum interval between GitLab refresh attempts; deferred hints retry at expiry |
| `idle_grace` | `45s` | Entire-pool idle delay; at least one reconciliation tick |
| `observation_grace` | `90s` | Delay before retrying an unconfirmed lifecycle request |
| `request_timeout` | `50s` | Adapter operation timeout; maximum `5m` |
| `readiness_timeout` | `5m` | Maximum bootstrap or health-recovery wait before a visible readiness failure and retry backoff |
| `pending_probe_timeout` | Greater of `observation_grace` and twice `jobs_poll_interval` | Available healthy runner time for one protected pending-job attempt; explicit value must be at least `jobs_poll_interval` |
| `runner` | `{}` | Opaque runner configuration passed to the adapter |

Durations are strings between `1s` and `24h`, subject to the listed relationships.
GitLab requests have a 12-second timeout. A complete reconciliation has a
five-minute cancellation deadline. Pagination is bounded to 200 pages of 100
active jobs per project; incomplete snapshots never drive lifecycle decisions.
The inbox holds at most 10,000 distinct unverified job hints; overflow returns 503
and polling still provides recovery.

## Credentials

| Environment variable | Purpose |
| --- | --- |
| `GITLAB_API_TOKEN` | Personal, project, or group access token with `read_api` and access to all projects |
| `GITLAB_WEBHOOK_SECRET` | At least 32 random characters for the GitLab hook |
| `ADMIN_TOKEN` | Optional independent secret of at least 32 characters; enables status and metrics |
| Pool-specific `runner_token_env` | A distinct pre-created runner's `glrt-` authentication token |
| Adapter-specific mapped variables | Credentials and connection settings needed by the external adapter |

Generate random secrets with a tool such as `openssl rand -hex 32`. Set protected
runner policy, tags, project locking, and untagged-job policy in GitLab. The
controller does not change GitLab runner registrations. Do not share a pool's
token with another controller or unmanaged manager. Include every project that
can submit work to these runners in `project_ids`.

Each pool requires a unique positive numeric `runner_id`. Copy the ID of the
registered runner from the GitLab UI and pair it with that runner's `glrt-`
authentication token. The controller does not call `POST /runners/verify` to
discover an ID: that endpoint can update runner contact and manager records.
The configured ID is pinned while a pool has saved resources or unresolved
operations. A token and numeric ID must identify the same runner; preserve this
pair during credential rotation.

Read-only GraphQL runner metadata supplies `id`, `paused`, `accessLevel`,
`runUntagged`, and `tagList`. Paginated `GET /projects/:id/runners` verifies that
the runner is available to each project, including inherited runners. Supply
the [`read_api` scope](https://docs.gitlab.com/api/graphql/#token-scopes) for
read-only GraphQL queries and a token user authorized to read the registered
runner. [Project runner listings](https://docs.gitlab.com/api/runners/#list-all-of-a-projects-runners)
require at least the Maintainer or Auditor role in each target project, or
instance administrator access. These role checks are separate from token scopes;
[ordinary personal, project, and group token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/)
do not include `read_runner`.
Missing permissions, unavailable metadata, paused runners, runners that accept
untagged jobs, a tag set different from configured `tags`, or unavailable project
bindings block pending scale-out for the affected runner or project. Failures
appear in the pool's `runner_error` with the failed metadata query or project
availability check. Offline, stale, and never-contacted runner status does not
block it: stopped pools normally have no connected manager.

GitLab's current protected-branch or protected-tag settings and job hooks do not
prove that a pending job is eligible for a protected runner. By default,
`protected_pending_policy: "probe_once"` gives a uniquely matched job one bounded
attempt with healthy available capacity; eligibility remains unknown until
GitLab assigns it. Use `"strict"` to prevent pending jobs of unknown protected
eligibility from activating capacity. A pool name such as `protected` has no
effect on the registered runner's policy; API metadata is authoritative.
See [protected pending jobs](operations.md#protected-pending-jobs) for attempt
accounting and the explicit retry workflow.

## Command providers

`providers` maps a local stable name to these fields:

| Provider key | Meaning |
| --- | --- |
| `identity` | Required stable account identity expected from the adapter; independent of API token |
| `lifecycle` | Required `retained` or `disposable` policy |
| `command_env` | Name of an environment variable containing a JSON array of command arguments |
| `environment` | Map of child environment names to source environment variable names |
| `max_running` | Required concurrent resource-count policy, 1–1,000 |
| `revision` | Optional deployment revision included in runner configuration revisions |

For example:

```json
{
  "identity": "account-example",
  "lifecycle": "retained",
  "command_env": "HOST_A_COMMAND_JSON",
  "environment": {
    "API_ENDPOINT": "HOST_A_ENDPOINT",
    "API_TOKEN": "HOST_A_TOKEN",
    "ACCOUNT_ID": "HOST_A_ACCOUNT_ID",
    "LIFECYCLE": "HOST_A_LIFECYCLE"
  },
  "max_running": 5
}
```

The child environment names above are illustrative; the adapter defines its own.
Set `HOST_A_COMMAND_JSON` to a JSON array such as
`["/opt/runner-host-adapter"]`. The first element must be an absolute path to an
installed executable. Additional fixed arguments are allowed, but credentials
must use environment mappings or JSON stdin. Quotes inside the JSON are data;
there is no implicit shell, variable expansion, pipeline, or command substitution.

Environment names use uppercase letters, digits, and underscores and start with
a letter. Every mapped source must have a nonempty value. Only mapped variables
reach the child, with defaults `PATH=/usr/bin:/bin` and `LANG=C` unless explicitly
mapped. The child does not inherit unrelated controller secrets. A script requires
its interpreter to be installed and declared explicitly or through its executable
format. The stock container has no shell, CLI tools, or interpreter. Mount a
compatible static adapter binary or build a derived image with its dependencies.

Use one provider entry per account/scope. Aliases for the same binding are
rejected so capacity accounting cannot be split. The adapter describes its
account identity, endpoint/region scope, lifecycle, and revision before mutations;
the controller pins this binding in state. Changing endpoint, region, account, or
lifecycle for saved resources requires an explicit resource cutover, not a secret edit.
One account cannot run simultaneous mixed lifecycle policies.

Bindings can change after all saved handles and unresolved operations have been
released. The adapter's implementation revision can change without rebinding;
provider `revision` is a separate operator-controlled deployment setting.
Removing an unused provider prunes its binding only after its saved handles and
uncertain operations have been released.

`max_running` includes unmanaged running and unknown resources and durable
reservations. It does not discover subscription limits or model cost, CPU,
regional, or storage quotas. Choose a count policy that fits the account and let
the adapter enforce additional restrictions.

## Pools and runner configuration

| Pool key | Meaning |
| --- | --- |
| `name` | Unique stable identifier, independent of mutable tags |
| `provider` | Key in `providers` |
| `tags` | Complete runner tag set matching its GitLab registration |
| `max_instances` | 1–100 slots |
| `jobs_per_instance` | 1–64 concurrent jobs per manager |
| `priority` | -10000 to 10000, descending; default zero |
| `runner_id` | Required unique positive numeric ID of this pool's registered GitLab runner |
| `runner_token_env` | Environment variable holding this pool's unique token |
| `protected_pending_policy` | `probe_once` by default; `strict` blocks attempts when protected eligibility is unknown |
| `protected_probe_max_hosts` | New host starts for one uncertain job; default two, configurable from 1 to 10 |
| `adopt_ids` | Optional ordered existing resource IDs bound to slots 0, 1, … |
| `options` | Opaque JSON object of adapter-specific compute and location settings |

At most 100 pools and 1,000 configured slots are accepted. Shrinking a pool keeps
extra saved slots until idle release. Adoption IDs must be unique within a
provider and cannot replace saved IDs. Adoption is a one-time bootstrap: consumed
IDs remain recorded, so a destroyed disposable resource is not adopted again
after its slot is reused or the controller restarts. Use `resolve-create` for
interrupted creation rather than `adopt_ids`.

The global `runner` object defaults to `{}`. Its keys are adapter-defined; the
controller requires no particular executor, container engine, image, guest OS,
or bootstrap tool. The adapter receives it as `RunnerSpec.options`, alongside
name, GitLab URL, transient token, concurrency, and revision. Pool `options` is a
separate object describing compute placement or shape.

Runner configuration, pool options, and provider `revision` contribute to the
configuration revision. Healthy managers keep their deployed settings until idle
release. Compute/location options and `runner_id` cannot change while saved
handles exist; migrate or replace those resources deliberately. See [operations](operations.md)
for rotation and recovery.
