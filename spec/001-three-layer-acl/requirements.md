# Requirements — Three-layer ACL (spec/001 — RETROSPECTIVE)

Source: ADR-0002 + ADR-0003 + `internal/middleware/auth.go` + `internal/config/acl.go` (CGC-verified 2026-05-18).

> ID convention: 001-block.

## Layer 1 — X-HAL-Key header

REQ-001: The system shall require the HTTP header `X-HAL-Key` on every endpoint except `/health` and the `/docs/*` subtree.
REQ-002: When a request lacks `X-HAL-Key`, the system shall return HTTP 401.
REQ-003: When `X-HAL-Key` is present but its value is not in the loaded ACL, the system shall return HTTP 401.
REQ-004: The system shall expose the header name as `middleware.HeaderHALKey = "X-HAL-Key"` constant.

## ACL config loading

REQ-005: The system shall load ACL configuration in priority order: `HAL_ACL_KEYS` env var (inline JSON), then `HAL_ACL_KEYS_FILE` (default `/cubeos/config/hal-acl.json`).
REQ-006: When neither source provides keys, the system shall enter permissive mode (`ACLConfig.Permissive = true`) and log a WARN at startup.
REQ-007: While in permissive mode, the system shall accept any request without role checks.
REQ-008: The system shall expose `config.LoadACLConfig() *ACLConfig` as the canonical loader.

## Layer 2 — Role-based per-prefix permissions

REQ-009: The system shall define exactly three roles in `config/acl.go`: `RoleCore = "core"`, `RoleMeshSat = "meshsat"`, `RoleReadOnly = "readonly"`.
REQ-010: The system shall hold the role → permission table in `middleware/auth.go` `rolePermissions` map.
REQ-011: While the request's role is `core`, the system shall allow all methods on all paths.
REQ-012: While the request's role is `meshsat`, the system shall allow all methods on `/gps/*`, `/meshtastic/*`, `/iridium/*` and read-only methods on `/network/wifi/status/`, `/network/status`, `/system/info`, `/system/temperature`, `/system/uptime`, `/power/status`, `/power/battery`, `/sensors/*`.
REQ-013: While the request's role is `readonly`, the system shall allow GET methods on `/system/*`, `/power/status`, `/sensors/*`.
REQ-014: When the requested path or method is not permitted for the request's role, the system shall return HTTP 403.

## Layer 3 — `CUBEOS_TIER` gating

REQ-015: The system shall read `CUBEOS_TIER` env var at startup with default `full`.
REQ-016: The system shall provide a `requireFullTier(handler)` wrapper that returns HTTP 403 when `CUBEOS_TIER != "full"`.
REQ-017: The system shall wrap destructive endpoints in `requireFullTier`: `/system/service/{name}/recreate`, `/network/netplan`, `/firewall/nat/enable`, `/firewall/nat/disable`, `/network/ap/revert`.

## Permissive-mode posture

REQ-018: While the system runs in permissive mode, the system shall log "HAL ACL: PERMISSIVE MODE — no auth!" once per process at startup and again on every 1000th request as a reminder.
