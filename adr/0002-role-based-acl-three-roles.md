# 2. Three-role ACL (`core` / `meshsat` / `readonly`)

Date: 2026-05-18 (codifying shipped design)

## Status

Accepted

## Context

HAL is privileged. Different consumers need different access:

- **cubeos-api** needs everything (lifecycle ops + system config).
- **MeshSat user-app** needs comms hardware (GPS, Meshtastic, Iridium) + some basic system info; should NOT control NAT or netplan.
- **Monitoring scripts** need GET-only access to system + power + sensors; should NEVER POST anything.

A boolean "key valid Y/N" gate would over-permission MeshSat + monitoring.

## Decision

3-role per-prefix ACL in `internal/config/acl.go` + `internal/middleware/auth.go`:

```go
RoleCore     // = "core"     — full access, used by cubeos-api
RoleMeshSat  // = "meshsat"  — communication HW + read-only system
RoleReadOnly // = "readonly" — GET-only system, power, sensors
```

Permissions are hardcoded in `middleware/auth.go` `rolePermissions` map. Role is assigned per-API-key in `/cubeos/config/hal-acl.json` (managed by operator). Adding a new role requires code change.

## Consequences

**Positive:**
- Hardcoded permissions = security boundary; not runtime-tamperable.
- Single review surface (one map in auth.go).
- Roles match real consumers; new consumers usually fit one of the three.

**Negative:**
- Adding a role needs a code change + redeploy. Acceptable given how rare that is.
- Less flexible than RBAC with operator-defined roles. Not needed for the current consumer set.

**Enforced by:** Component Article C-II + `middleware/auth.go` rolePermissions map.
