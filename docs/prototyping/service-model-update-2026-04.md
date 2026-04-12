# Service model update for multisite prototyping

This note captures the corrected runtime model for multisite CARTA prototyping.

## Summary

- `carta-ctl` is a persistent site service.
- `carta-spawn` is a persistent site service.
- both may run under a site-managed system account.
- `carta-list` is launched only when needed.
- `carta-spawn` launches `carta-list` under the authenticated user's local execution identity.

## Why this matters

The previous wording was too close to a model where all runtime components might be started together.
For site deployment, the long-lived control-plane services should be separated from user-owned runtime processes.

This gives a cleaner privilege boundary:

- `carta-ctl`: site-facing control plane
- `carta-spawn`: trusted launcher and privilege boundary
- `carta-list`: unprivileged per-user runtime process

## Expected launch flow

1. user authenticates to `carta-ctl`
2. `carta-ctl` validates identity and authorization
3. `carta-ctl` sends a trusted launch request to `carta-spawn`
4. `carta-spawn` resolves the local execution identity
5. `carta-spawn` starts `carta-list` with the target user's uid/gid
6. `carta-list` accesses files with native OS permissions

## Design consequences

### Service management

`carta-ctl` and `carta-spawn` should be installed as boot-time services, for example under `systemd`.

`carta-list` should not be enabled as a boot-time site service.

### Security boundary

The browser must not directly decide the user identity used for spawn.
The authenticated identity passed from `carta-ctl` to `carta-spawn` must come from validated server-side auth state.

### Auditability

Spawner logs should record:

- authenticated identity
- resolved local account
- launch time
- executable path
- process id
- exit status

### Filesystem access

This model intentionally relies on normal OS permissions for home, project, and shared data access.

## Implementation TODOs

- add a trusted internal launch API from `carta-ctl` to `carta-spawn`
- add explicit user identity resolution and validation
- add controlled environment construction for per-user launch
- add auditing for create/reconnect/terminate flows
- document whether launch uses `sudo`, a helper binary, or another privilege-separation mechanism

## Open question

The name `carta-list` is used here as provided in prototyping discussions.
If the final worker or per-user process name changes, this document should be updated to keep the execution model the same.
