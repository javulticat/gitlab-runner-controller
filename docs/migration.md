# State compatibility and resource adoption

The controller uses state schema version 1. It accepts only
complete snapshots in that format, including provider account/scope bindings,
pinned placement options, and numeric runner IDs. Unsupported versions,
unversioned files, unknown fields, and incomplete snapshots are rejected without
rewriting them. No automatic conversion is provided for unsupported snapshots.

Keep rejected snapshots and their matching configuration private. Do not edit
state to bypass ownership checks or start a fresh empty state directory over
resources another controller still owns.

## Taking over existing resources

Use a controlled cutover when adopting resources from another controller or
management system:

1. Stop the previous owner and verify no other process manages the resources.
   Retain its private snapshot and configuration outside the new application
   volume. Record each pool's ordered resource IDs and audit any uncertain
   creation before proceeding. An unknown creation outcome cannot be treated as
   proof that no resource exists.
2. Confirm exclusive ownership of each resource in the intended account and
   inventory scope. Let jobs finish and drain managers before changing bootstrap
   or runtime layout. Preserve required files and manager identity according to
   the selected adapter's documented cutover procedure.
3. Install a compatible external adapter. Configure its command, allowlisted
   environment mappings, stable expected account identity, lifecycle, compute
   `options`, and opaque runner `runner` settings. The controller has no default
   executor, image, or hosting endpoint. Preserve the intended resource lifecycle.
4. Prepare a fresh configuration and state directory. Set ordered `adopt_ids`
   for the verified resources, preserving slot order. These IDs are a one-time
   bootstrap and cannot replace saved IDs. Do not adopt unrelated account
   resources or copy another controller's snapshot into the new state directory.
5. Set complete GitLab runner tags to match pool `tags`, each pool's unique
   numeric `runner_id` and matching authentication token, and every project those
   runners serve. Give the API token `read_api` scope, access to each registered
   runner, and at least the Maintainer or Auditor role in every target project
   for runner listings. Disable untagged jobs on each registered runner.
6. Validate the configuration using the intended environment:

   ```sh
   ./bin/gitlab-runner-controller check-config
   ```

   This is local validation; it does not execute the adapter or prove remote
   ownership.
7. Start the controller and inspect protected status, account bindings, complete
   inventory, runner metadata, and readiness. Submit a test job and confirm state
   persists through a controlled restart before retiring the previous deployment.
   Update the webhook destination and secret as needed. Remove obsolete manager
   credentials and runtime artifacts only after verifying they are no longer
   required.

Manager identity files are not generally portable across images, users,
executors, or filesystem layouts. The adapter and selected runtime define that
compatibility. Retained stop/resume cycles must preserve the adapter's documented
manager identity contract.

## Preserving current ownership

Keep current state with its controller ID, GitLab project binding, provider
account/scope bindings, numeric runner IDs, and matching configuration. Compute
placement, lifecycle, and runner bindings cannot change while saved resource IDs
or unresolved operations remain. A provider binding may change after all its
saved handles and unresolved operations have been released.

Preserve pending-attempt records with the snapshot. Restart, webhook replay,
and pool renaming cannot grant a spent job another attempt. An audited additional
attempt uses [`retry-pending`](operations.md#protected-pending-jobs). A confirmed
missing known ID uses [`release-missing`](operations.md#retained-and-disposable-release).
An uncertain creation uses [`resolve-create`](operations.md#recovering-an-ambiguous-creation);
do not bypass those workflows through manual snapshot edits or adoption settings.

See [configuration](configuration.md) for current settings and limits, and
[operations](operations.md) for recovery and backup procedures.
