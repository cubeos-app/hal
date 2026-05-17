# Project Charter — CubeOS HAL

> Component-scoped charter. Parent project: `/home/claude-runner/gitlab/products/cubeos/docs/PROJECT.md`. Authored 2026-05-18 against CGC ground truth (50 files / 888 functions / 171 classes — `claude-gateway/docs/sdd-audits/hal-cgc-2026-05-18.md`).

## Role in the CubeOS family

`hal/` (project 22) is the **privileged container** that owns every host interaction. It exists to operationalise CubeOS project-level **Article I (HAL Owns the Host)**: unprivileged containers (cubeos-api, cubeos-dashboard, all user apps) NEVER touch host services directly. Everything routes through HAL's REST API.

- **Container shape:** privileged + host network mode. Binds `/dev`, `/sys`, `/proc`, `/etc`, etc.
- **Listens on:** port `6005` (per HAL_URL convention).
- **Auth:** `X-HAL-Key` header + role-based per-prefix ACL (3 roles) on every endpoint except `/health` and `/docs`.
- **OpenAPI:** served at `/docs/openapi.yaml` (handlers/docs.go `ServeOpenAPISpec`).

## What this repo owns (CGC-verified scope, 2026-05-18)

The shipped surface spans ~47 handler files grouped by 22 functional domains under `internal/handlers/`. Major realms:

- **System control** — info, CPU, memory, disk, temperature, throttle, EEPROM, boot config, uptime, hostname, OS info, reboot, shutdown, systemd service start/stop/restart, container service recreate (full tier only).
- **Power** — status, battery, UPS (auto-detect across 4 driver classes: PiSugar3, X1202, X728, generic), charging control, power monitor start/stop, UPS configuration.
- **RTC** — status, sync-to/from RTC, wake-alarm set/clear.
- **Watchdog** — status, pet, enable.
- **Network** — interfaces enumeration + traffic stats, WiFi AP setup + revert, DHCP request, static IP, netplan write, listening-ports scan, capability detection (DHCP, proxy, Ethernet DHCP).
- **Firewall** — rules CRUD, NAT enable/disable, IP forwarding, save/restore/reset.
- **VPN** — WireGuard up/down, OpenVPN up/down, Tor status/config/start/stop/newcircuit.
- **Storage** — device list, SMART info, USB mount/unmount/eject, usage.
- **Logs** — kernel, journal, hardware.
- **Hardware detection** — interfaces, WiFi-AP whitelist + blacklist (persisted to `/cubeos/config/wifi-ap-{whitelist,blacklist}.json`), retest.
- **Support bundle** — `/support/bundle.zip` for diagnostics export.
- **GPS** — devices, status, position.
- **Cellular** — modems, signal, connect/disconnect, Android tethering.
- **Meshtastic** — full driver with admin commands (reboot, factory reset, traceroute, remove node, config radio/module, waypoints). Disabled via `HAL_DISABLE_MESHTASTIC=true`.
- **Iridium** — full driver with AT command escape hatch. Disabled via `HAL_DISABLE_IRIDIUM=true`.
- **Camera** — capture + stream start/stop.
- **Sensors** — all, 1-Wire devices + temperatures, BME280.
- **Audio** — devices, volume, mute, test tone.
- **GPIO / I2C / Bluetooth / USB / Mounts** — per-domain handlers.

## What this repo does NOT own

- **REST surface for operators** — `api/` (project 13) owns those routes and proxies relevant ones into HAL.
- **Business logic / sagas** — `api/`'s FlowEngine owns saga orchestration.
- **App lifecycle** — `api/`'s Orchestrator owns.
- **Pi-hole DHCP enable/disable** — lives in `api/`, NOT HAL. (Previous spec incorrectly claimed a `/hal/pihole/dhcp` endpoint.)

## Constitutional inheritance

Inherits the full CubeOS project-level constitution. Of the 19 project articles, these are most directly load-bearing:

- **Article I** — HAL Owns the Host (this repo IS the operationalisation)
- **Article VII** — Hardware detection drives capabilities (this repo does the detection)
- **Article IX** — Serial port ownership (per-driver in this repo's `handlers/iridium_driver.go` + `meshtastic_serial.go` + `gps.go`)
- **Article XI** — `CGO_ENABLED=0` (pure Go)
- **Article XII** — Swagger annotations (HAL has its own OpenAPI at `/docs/openapi.yaml`)
- **Article XVII** — Claude Code files gitignored

This repo's own `constitution.md` adds HAL-specific articles.

## Build (CGC-verified)

```bash
cd hal
go build ./cmd/cubeos-hal     # → cubeos-hal binary
go test ./...                  # all packages
golangci-lint run              # lint
```

Note: `cmd/cubeos-hal/main.go` is the entry point (NOT `cmd/hal/main.go`).

## Tiers

`CUBEOS_TIER` ∈ `{"full", "container"}` (default `full`). The `requireFullTier(handler)` wrapper denies endpoints in container tier. Examples of full-only endpoints: `RecreateComposeService`, `WriteNetplan`, `EnableNAT`, `DisableNAT`, `RevertToAP`.

## Driver pattern

`HALHandler` struct holds long-lived drivers:
- `iridium *IridiumDriver` (handlers/iridium_driver.go)
- `meshtastic *MeshtasticDriver` (handlers/meshtastic_driver.go)
- `powerMonitor *PowerMonitor` (handlers/power_monitor.go)
- `supervisor *DeviceSupervisor` (handlers/device_supervisor.go)

When a driver is disabled (env var), its routes return HTTP 501 with explanatory body.

## Source trace

- `hal/CLAUDE.md` (local-only operator notes)
- `hal/OPENAPI_DOCS.md` (canonical API docs)
- Real source: `internal/handlers/routes.go` (375 lines) for the full endpoint catalog
- CGC audit: `claude-gateway/docs/sdd-audits/hal-cgc-2026-05-18.md`
- Parent: `/home/claude-runner/gitlab/products/cubeos/docs/architecture/02_ARCHITECTURE.md` §HAL boundary
- Parent: `/home/claude-runner/gitlab/products/cubeos/CLAUDE.md` §"Critical Rules" rule 1 + 4
