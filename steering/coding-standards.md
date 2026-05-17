# Steering — Coding standards

CGC-verified against 50 files / 888 functions / 171 classes / 47 handler files.

## Package layout

```
cmd/cubeos-hal/main.go       — entry point
internal/config/             — ACL configuration (1 file: acl.go)
internal/devices/            — low-level device helpers (1 file: i2c.go)
internal/middleware/         — HTTP middleware (auth.go — X-HAL-Key + role-ACL)
internal/handlers/           — all REST endpoints (~47 files, one file per domain)
```

New code MUST go in an existing package. Justify any new internal/ subpackage in the commit message + an ADR.

## Handler file naming

One file per `/<domain>/` route prefix in `routes.go`:

| Route prefix | Handler file |
|---|---|
| `/system/*` | `system.go` |
| `/power/*` | `power.go` + `power_monitor.go` |
| `/network/*` | `network.go` + `network_capabilities.go` + `network_ports.go` + `network_wifi_saved.go` + `network_wifi_status.go` + `wifi_ap.go` |
| `/firewall/*` | `firewall.go` |
| `/vpn/*` | `vpn.go` |
| `/storage/*` | `storage.go` + `usb_storage.go` |
| `/logs/*` | `logs.go` |
| `/gps/*` | `gps.go` |
| `/cellular/*` | `cellular.go` |
| `/meshtastic/*` | `meshtastic.go` + `meshtastic_driver.go` + `meshtastic_ble.go` + `meshtastic_serial.go` |
| `/iridium/*` | `iridium.go` + `iridium_driver.go` |
| `/camera/*` | `camera.go` |
| `/sensors/*` | `sensors.go` |
| `/audio/*` | `audio.go` |
| `/gpio/*` | `gpio.go` |
| `/bluetooth/*` | `bluetooth.go` |
| `/i2c/*` | `i2c.go` + `i2c_recovery.go` |
| `/usb/*` | `usb.go` |
| `/mounts/*` | `mounts.go` |
| `/hardware/*` | `hardware.go` |
| Route table | `routes.go` |
| Top-level handler struct | `handlers.go` |
| Input validation helpers | `validators.go` |
| Docs serving | `docs.go` |
| Device registry + supervisor | `device_registry.go` + `device_supervisor.go` |
| UPS drivers | `ups_detect.go` + `ups_pisugar3.go` + `ups_x1202.go` + `ups_x728.go` |

## Function naming

- `Get<Resource>` — read handler (GET)
- `Set<Resource>` — write handler (POST + state-change)
- `List<Resources>` — collection read
- `<Action><Resource>` — RPC-style action (Reboot, Shutdown, StartService, EnableNAT, ...)
- `<HW>Driver` — long-running driver struct (IridiumDriver, MeshtasticDriver)

## Error handling

- HTTP error responses go through `errorResponse(w, status, message)` helper (handlers/handlers.go OR per-handler local).
- Log + return on shell-out failures.
- NEVER `panic` in a handler — catch in middleware Recovery.

## Tests (colocated, Go convention)

Test files live alongside source: `internal/handlers/device_registry_test.go`, `internal/handlers/meshtastic_serial_test.go`, `internal/middleware/auth_test.go`. New tests follow the same `<source>_test.go` colocation.

## Logging

`log.Printf` via stdlib + per-handler `[HAL/<domain>]` prefix convention. NEVER log raw API keys; the auth middleware redacts.

## Forbidden patterns

- Calling `os/exec.Command("sudo", ...)` — HAL is already privileged
- Per-request driver construction — drivers go in HALHandler struct (Article C-VI)
- New routes without OpenAPI update (Article C-VII)
- Removing routes that ship clients depend on (Article C-V — 90-day deprecation window)
