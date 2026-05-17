# Steering — Security baseline

Three concentric layers per Articles C-I + C-II + C-III.

## Layer 1 — `X-HAL-Key` header

Every endpoint except `/health` and `/docs/*` requires `X-HAL-Key: <key>`. Implemented in `internal/middleware/auth.go`. Missing or unknown key = HTTP 401.

The expected keys live in:
- `HAL_ACL_KEYS` env var (inline JSON: `{"key1":"core","key2":"meshsat"}`)
- `HAL_ACL_KEYS_FILE` (default `/cubeos/config/hal-acl.json`)

If neither is set, HAL runs in **permissive mode** (no auth) — useful for dev + initial bootstrap, dangerous in production. Operator MUST configure ACL keys.

## Layer 2 — Role-based per-prefix permissions

Each key is mapped to a `config.Role` (`core` / `meshsat` / `readonly`). The middleware checks if the request's method + path-prefix is allowed for that role via the `rolePermissions` map in `middleware/auth.go`.

Insufficient role = HTTP 403.

| Role | Allowed surfaces |
|---|---|
| `core` | Everything (`/` prefix + all methods) |
| `meshsat` | `/gps/*`, `/meshtastic/*`, `/iridium/*` (all methods); `/network/wifi/status/`, `/network/status`, `/system/info`, `/system/temperature`, `/system/uptime`, `/power/status`, `/power/battery`, `/sensors/*` (GET only) |
| `readonly` | `/system/*`, `/power/status`, `/sensors/*` (GET only) |

## Layer 3 — `CUBEOS_TIER` full/container gating

Destructive endpoints wrap with `requireFullTier(handler)` (per ADR-0003). On container-tier HAL, those return HTTP 403 with body explaining the tier mismatch. Examples:

- `POST /system/service/{name}/recreate` — full only
- `POST /network/netplan` — full only
- `POST /firewall/nat/enable` / `disable` — full only
- `POST /network/ap/revert` — full only

## Defense-in-depth notes

- HAL is a **privileged container with host network mode** — losing X-HAL-Key + role + tier gates is catastrophic.
- The auth middleware NEVER logs the raw `X-HAL-Key` value (only the redacted form + the matched role).
- HAL binds on `0.0.0.0:6005` (host network) — there is NO loopback-only enforcement at the bind level (different from my previous incorrect spec). Defense lives at the X-HAL-Key + role layers.

## What HAL deliberately does NOT defend against

- A compromised PRIVILEGED container can break out of namespaces (this is fundamental to privileged + host network). The defense is preventing a compromise — limited inbound HAL surface, audit logging, code review.
- Operator with host shell access owns the host. Defense is SSH hardening + fail2ban (per parent project security-baseline).

## Operator security checklist

- [ ] `HAL_ACL_KEYS_FILE` set with at least one `core` key + one `meshsat` key + one `readonly` key
- [ ] Permissive mode NOT active in production (`config.Permissive == false`)
- [ ] cubeos-api configured with the `core` HAL_KEY
- [ ] MeshSat user-app (if installed) configured with the `meshsat` HAL_KEY
- [ ] Monitoring scripts (Prometheus exporter etc.) use the `readonly` HAL_KEY
- [ ] `CUBEOS_TIER=container` set on LXC deployments
