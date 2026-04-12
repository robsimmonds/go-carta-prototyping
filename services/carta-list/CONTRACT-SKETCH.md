# Suggested contract for carta-list

This is a design sketch, not code.

## Spawn flow

1. Browser authenticates with home `carta-ctl`
2. Home `carta-ctl` decides local site listing is needed
3. Local `carta-ctl` asks local `carta-spawn` to start `carta-list`
4. `carta-spawn` launches `carta-list` as the authenticated local user
5. `carta-list` connects back to local `carta-ctl`
6. Local `carta-ctl` marks `hasCartaList=true` for that site
7. Frontend can now request listings through home `carta-ctl`

## Minimal requests

### Register
- site id
- session id
- username
- token
- capability: listing

### ListDirectory
- path
- include hidden?
- sort key
- sort direction

### ListDirectoryResponse
- path
- entries[]:
  - name
  - type
  - size
  - modified
  - readable
  - writable

### StatPath
- path

### Error
- code
- message

## Security rules

- canonicalize path
- enforce base-folder / allowed roots
- reject traversal outside allowed roots
- run as the authenticated user so filesystem permissions are natural
