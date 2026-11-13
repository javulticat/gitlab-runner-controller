# Security policy

## Supported versions

| Version | Security support |
| --- | --- |
| v0.1.x | Latest patch supported |

## Reporting a vulnerability

Report vulnerabilities through
[GitHub private vulnerability reporting](https://github.com/javulticat/gitlab-runner-controller/security/advisories/new).
Include the affected version, reproduction steps, and impact. Keep exploit
details, credentials, and affected user data out of public issues. Coordinate
public disclosure through the private report.

## Deployment security

The controller and its external adapter have powerful access to runner hosts.
Use HTTPS ingress, independent random webhook/admin secrets, least-privilege
GitLab read access, and credentials scoped to the required hosting account.
Keep state, backups, environment files, and runner configuration private.

Install only trusted adapter executables and dependencies. They receive explicitly
mapped credentials and transient runner tokens through stdin. The controller
executes argv directly and does not evaluate a shell. Keep credentials out of
arguments, resource IDs, inventory, errors, and logs. Environment allowlisting
limits inherited secrets; it does not sandbox an adapter's filesystem or network
access. Use an unprivileged account and grant only required deployment permissions.

Job isolation, bootstrap security, runtime ownership, and protected runner
configuration belong to the selected adapter. Choose an executor and hosting
policy suitable for your CI workload's trust level. Verify drain and ownership
checks before deploying an adapter; the controller does not impose a default
container runtime or isolation model.

Report suspected secret exposure promptly. Revoke or rotate exposed credentials
and replace affected runner configurations; deleting a committed secret does not
repair exposure. Avoid attaching raw provider output, state, or private adapter
diagnostics to public reports.
