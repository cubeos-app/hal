# Constitution — CubeOS HAL

Inherits the full CubeOS project-level constitution at `/home/claude-runner/gitlab/products/cubeos/docs/constitution.md` (Articles I-XIX). This file adds HAL-specific articles (C-I through C-IX). All facts CGC-verified 2026-05-18.

## Article C-I — `X-HAL-Key` header on every endpoint except `/health` and `/docs`

The system shall require the `X-HAL-Key` header on every endpoint except `/health` and the `/docs/*` Swagger-UI subtree. Missing or unknown key returns HTTP 401. The header constant is `middleware.HeaderHALKey = "X-HAL-Key"`. Authentication is performed by `middleware/auth.go`.

## Article C-II — Three-role ACL (`core` / `meshsat` / `readonly`)

The system shall enforce role-based per-prefix permissions. The three canonical roles are:

- **`core`** — full access to all endpoints. Used by cubeos-api.
- **`meshsat`** — communication HW (gps, meshtastic, iridium) + read-only system/power/sensors.
- **`readonly`** — GET-only system, power status, sensors.

Roles are declared at `internal/config/acl.go` (`Role` typed string). Per-role permission tables live in `internal/middleware/auth.go` `rolePermissions` map and are NOT runtime-configurable. Adding a new role requires editing this map.

## Article C-III — `CUBEOS_TIER` full/container gating

The system shall recognize `CUBEOS_TIER` env var ∈ `{"full", "container"}` (default `full`). Destructive endpoints (compose recreate, netplan write, NAT enable/disable, AP revert) wrap their handlers with `requireFullTier(...)` which denies in container tier with HTTP 403. The tier is set once at HAL startup; never per-request.

## Article C-IV — Per-driver disable via env var

The system shall expose runtime-disable env vars for cost-heavy or hardware-optional drivers:

- `HAL_DISABLE_IRIDIUM=true` → IridiumDriver not initialised; `/iridium/*` routes return HTTP 501
- `HAL_DISABLE_MESHTASTIC=true` → MeshtasticDriver not initialised; `/meshtastic/*` routes return HTTP 501

This lets a HAL run on hardware without the corresponding radios without log spam or initialisation failures.

## Article C-V — Append-only handler routes

The system shall NEVER remove an existing route from `internal/handlers/routes.go`. Deprecated routes return HTTP 410 Gone (or continue to serve with a `Sunset` response header) but the path is not removed for at least 90 days after deprecation. Reason: external automation (operator scripts) depends on URL stability.

## Article C-VI — Long-lived drivers live in `HALHandler` struct

The system shall hold every long-running driver (IridiumDriver, MeshtasticDriver, PowerMonitor, DeviceSupervisor) as a field on the `HALHandler` struct in `handlers/handlers.go`. Driver initialisation happens once in `NewHALHandler()`. Per-request handler functions reach the driver via `h.<driver>` — never construct a new driver per request.

## Article C-VII — OpenAPI is the source of truth for routes

The system shall keep `/docs/openapi.yaml` (served by `handlers/docs.go ServeOpenAPISpec`) in lock-step with `internal/handlers/routes.go`. Adding/removing/changing a route requires updating the OpenAPI spec in the same commit. Diverging the two breaks the api-side proxy + operator-facing docs.

## Article C-VIII — Hot-plug events flow via `DeviceSupervisor`

The system shall route hardware hot-plug events (USB device add/remove, BLE device discovery, GPS reattach) through the single `DeviceSupervisor` instance held on HALHandler. Drivers subscribe to supervisor events; do NOT instantiate per-driver udev watchers.

## Article C-IX — `/cubeos/config/` is the only persistent state location

The system shall persist its own state (WiFi AP whitelist/blacklist, saved WiFi networks, UPS configuration) under `/cubeos/config/`. Examples:

- `/cubeos/config/wifi-ap-whitelist.json`
- `/cubeos/config/wifi-ap-blacklist.json`
- `/cubeos/config/wifi-networks.json` (saved network credentials)
- `/cubeos/config/ups.json` (UPS configuration)

The persistent volume is operator-managed (survives container restart but is wiped on full-image reflash).
