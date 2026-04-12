# carta-list scaffold

This is a standalone scaffold for a new `services/carta-list` Go service.

It is intentionally conservative:
- no assumptions about your repo module path
- no assumptions about existing protobuf or gRPC packages
- no assumptions about how `carta-spawn` supervises child processes

## Intended role

`carta-list` is a per-user process that:
- is started by local `carta-spawn`
- runs as the authenticated local user
- connects back to the local `carta-ctl`
- answers listing requests for the local site's files
- does **not** launch `carta-backend`
- only provides file/directory listing metadata

## Current scaffold contents

- `main.go`: minimal process skeleton with:
  - flag parsing
  - heartbeat loop
  - stdout logging
  - placeholder registration and request loop
- no external dependencies

## Integration points to add in your backend repo

1. Define how `carta-spawn` launches this process
2. Define how it authenticates / proves identity to `carta-ctl`
3. Define the control channel:
   - WebSocket
   - gRPC stream
   - Unix socket
   - stdin/stdout bridge
4. Define the request/response schema for:
   - list directory
   - stat file
   - maybe bookmark roots / favorites
5. Add path policy enforcement so listing stays within allowed roots

## Suggested flags

- `--ctl-address`
- `--site-id`
- `--session-id`
- `--user`
- `--token`
- `--base-folder`

## Notes

This scaffold is meant to be dropped into your repo and then adapted to your real controller/spawner contracts.
