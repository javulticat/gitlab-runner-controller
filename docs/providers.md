# Runner hosting protocol

The controller invokes an operator-installed executable for every provider
operation. The same command implements all operations below. Adapters may be
written in any language; only the JSON protocol is required. Go adapters can use
[the provider types](../pkg/provider/provider.go) and
[the single-request server SDK](../pkg/provider/protocol.go).

The controller does not contain provider SDKs or runner bootstrap scripts.
Operators supply the adapter, its credentials, runtime dependencies, and compute
and runner settings. See [configuration](configuration.md) for command and
child-environment mappings and [deployment](deployment.md) for container packaging.

## SDK compatibility

Controller **v0.1.0** includes the Go SDK at
`github.com/javulticat/gitlab-runner-controller/pkg/provider`. Add it to a Go
adapter with:

```sh
go get github.com/javulticat/gitlab-runner-controller@v0.1.0
```

The SDK follows the controller's module version. While the major version is
`v0`, minor releases may change Go APIs incompatibly; patch releases preserve
the public Go API. Pin an exact SDK release, review the changelog before
upgrading, and run the adapter's checks and installation with `GOWORK=off` to
verify its standalone dependencies.

The SDK version, JSON wire protocol version, and state schema version are
separate contracts. Release `v0.1.0` uses wire protocol **1**. Adapters in any
language can interoperate through that protocol regardless of their SDK version.
Existing protocol-1 operations and safety guarantees remain compatible within
protocol 1; incompatible wire changes require a new protocol version. The strict
decoder rejects unknown fields, so adapters must emit only fields supported by
the controller's protocol version.

## Execution and framing

The command is a JSON argv array stored in the configured `command_env`. The
first element is an absolute executable path. Arguments are passed directly;
there is no implicit shell interpretation. One invocation reads exactly one JSON
request from stdin until EOF and writes exactly one JSON response to stdout,
optionally followed by whitespace. Stdout must contain no logs or other output.

Protocol version is the integer `1`. Each request and response has matching
`version`, nonempty `id`, and `operation`. Unknown fields, malformed JSON, extra
messages, mismatches, oversized output, and an unsuccessful process exit are
failures. Each request or response is bounded to **4 MiB**. Stderr is bounded to
**32 KiB** and discarded; the controller never forwards raw child diagnostics.
Use the adapter's own private diagnostic workflow to investigate failures.

The driver passes only explicitly mapped environment variables, with defaults
`PATH=/usr/bin:/bin` and `LANG=C` unless explicitly mapped. Credentials belong in
those mappings or runner JSON stdin,
never argv, stdout diagnostics, resource IDs, error details, or inventory. The
controller host must be Linux or macOS. The controller enforces context deadlines
and kills the command's Unix process group on cancellation; the adapter must also
honor the request deadline and cancel remote work where possible. Killing a local
process does not prove that a remote create did not happen.

## Identity and description

Successful responses include a verified `description`:

```json
{
  "identity": "account-example",
  "scope": "region-example",
  "lifecycle": "retained",
  "revision": "adapter-v1",
  "absence_confirms_destroy": false
}
```

- `identity` is a stable, non-secret account ID, independent of rotating tokens.
- `scope` identifies the endpoint, region, or other inventory namespace in which
  resource IDs are meaningful. Include all placement boundaries needed to prevent
  an ID from resolving to a different host.
- `lifecycle` is `retained` or `disposable` and must match configuration.
- `revision` is a nonempty adapter implementation revision. Compatible adapter
  upgrades do not rebind account ownership. This is separate from the optional
  provider deployment `revision` in controller configuration.
- `absence_confirms_destroy` must be `true` for disposable hosting: after a
  requested destroy, absence from a later complete current inventory authoritatively
  proves release. Do not claim this guarantee for incomplete or eventually
  ambiguous inventories.

`identity`, `scope`, and `revision` are at most 256 bytes each and cannot contain
newlines or NUL characters. The account/scope, lifecycle, and absence guarantee
are pinned while saved handles or unresolved operations exist; the adapter
implementation revision can change independently.

The initial `describe` verifies the configured expected identity and lifecycle.
The controller persists the identity/scope binding before mutations and repeats
description checks on reconciliation. Subsequent operations carry `expected` so
an adapter can reject a changed account or scope before touching a resource.
The adapter must validate its authenticated account rather than echo an
unverified configured value. Duplicate aliases cannot split one binding's quota.

## Requests and responses

Common request fields:

| Field | Meaning |
| --- | --- |
| `version` | Integer `1` |
| `id` | Correlation ID; echo unchanged |
| `operation` | One operation from the table below |
| `expected` | Expected description; initial description can omit unknown binding values |
| `deadline` | UTC RFC 3339 timestamp; do not continue after it |
| `resource_id` | Opaque saved ID for resource operations |
| `operation_id` | Durable creation ID; present for `create` |
| `options` | Pool compute/location JSON object where relevant |
| `runner` | Transient runner specification for `create` and `configure` |
| `owner` | Expected manager ownership identity for `healthy` and `drain` |

Runner JSON has `name`, `gitlab_url`, `token`, `jobs`, `options`, and `revision`.
Its `options` object is the global runner configuration, distinct from compute
`options`. The token must remain transient and private. The adapter defines its
executor, image, installation, files, and manager identity persistence; the
controller assumes none of those details.

For example, a complete inventory response is:

```json
{
  "version": 1,
  "id": "request-example",
  "operation": "list",
  "description": {
    "identity": "account-example",
    "scope": "region-example",
    "lifecycle": "retained",
    "revision": "adapter-v1",
    "absence_confirms_destroy": false
  },
  "resources": [{"id": "host-example", "state": "running"}],
  "complete": true
}
```

| Operation | Input beyond common fields | Successful output |
| --- | --- | --- |
| `describe` | No resource required | Valid `description` |
| `list` | No resource required | Non-null `resources` array and `complete: true`; use `[]` for an empty account |
| `create` | `operation_id`, compute `options`, `runner` | `resource` and `ok: true` |
| `resume` | `resource_id` | `ok: true`; retained lifecycle only |
| `persist` | `resource_id` | `ok: true`; retained lifecycle only |
| `healthy` | `resource_id`, compute `options`, `owner` | `ready` boolean |
| `configure` | `resource_id`, compute `options`, `runner` | `ready` boolean |
| `drain` | `resource_id`, compute `options`, `owner` | `ready` boolean; true only after jobs finish and the manager cannot accept more |
| `stop` | `resource_id` | `ok: true`; retained lifecycle only |
| `destroy` | `resource_id` | `ok: true`; disposable lifecycle only |

Each resource has a nonempty opaque `id`, state `running`, `stopped`, or `unknown`,
and optional RFC 3339 `created_at`. Inventory covers every current resource in the
bound account/scope, including unmanaged hosts. Destroyed history is excluded.
An incomplete or inaccessible inventory is an error, never an empty list. Missing
known resources remain unknown except after a requested destroy under the
authoritative absence guarantee.

The absence guarantee applies only after a requested destruction. A missing ID
at any other point must not be reported as `stopped` or treated as a replacement
opportunity. The controller's separate offline `release-missing` workflow uses
an operator audit and read-only calls to release a confirmed absent saved ID.

For failure, include a structured `error` instead of success fields:

```json
{"kind":"transient","stage":"preflight","retry_after_ms":30000,"no_side_effect":true}
```

Kinds are `transient`, `uncertain`, `not_found`, `conflict`, and `invalid`.
`stage` is required and is either `preflight` or `operation`. A `preflight`
failure certifies that the operation was never dispatched and must include
`no_side_effect: true`. Account verification can fail before a description is
available; a correlated, zero-exit preflight error still preserves its category
and retry delay. Operation errors and all successful responses require the
verified account/scope description. A preflight claim with success payload fields
is invalid.
`retry_after_ms` is an optional account backoff duration, capped at one day.
No raw remote message or child output is part of the wire error. A create that
may have taken effect must be `uncertain`. Only an adapter that can prove the
request was rejected before any side effect may set `no_side_effect: true` on its
create error. A generic failure, timeout, process exit, or lost response cannot
prove absence and must retain the creation reservation.

Repeatability means safe repetition after a lost reply. Repeating `drain` must
not start a new manager or force-stop assigned work. Return `ready: true` only
when completion is verified; return a safe error if ownership or manager state
cannot be verified. Persisted drain completion lets the controller recover a
lost destroy reply without asking a removed manager to drain again.

Repeating `destroy` for an already absent exact ID under the verified binding
may acknowledge success or return typed `not_found`. Neither response alone
releases capacity: the controller requires later complete current inventory
after requested destruction. A `not_found` response alone does not prove drain
completion or authorize release outside that workflow.

## Adapter responsibilities

1. Validate configuration and authenticated account identity before mutations.
   Return complete inventory and accurate power states without exposing secrets.
2. Enforce ownership during health, configure, and drain. Refuse unrelated
   managers and fail closed on inspection errors. Keep IDs opaque.
3. Make resume, persist, configure, drain, stop, and destroy safe to repeat after
   lost replies. `configure` and `drain` may return `ready: false` while work is
   progressing. Never make repeated drain force-stop a running job.
4. Accept the durable creation operation ID. Use remote idempotency when
   available, but do not infer that a lost response means creation failed.
5. For retained resources, preserve required storage and manager identity before
   configuration and across stop/resume. For disposable resources, support
   destruction and authoritative later absence; the controller will not call
   resume, persist, or stop.
6. Honor deadlines and return safe error categories and backoff durations. Keep
   runner tokens in protected configuration and out of logs and commands.

Test adapters with local fakes and fixtures covering authentication, wrong
identity/scope, complete inventory, malformed envelopes, output bounds, lost
responses, cancellation, ownership, repeatable drain, and both lifecycle release
conditions. Repository tests must not require credentials or mutate real resources.
