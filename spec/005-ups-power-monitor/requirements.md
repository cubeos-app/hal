# Requirements — UPS + PowerMonitor (spec/005 — RETROSPECTIVE)

Source: `internal/handlers/ups_detect.go`, `ups_pisugar3.go`, `ups_x1202.go`, `ups_x728.go`, `power.go`, `power_monitor.go` (CGC-verified — 6 files).

> ID convention: 400-block.

## UPS detection

REQ-400: The system shall expose `GET /power/ups/detect` probing I2C for any of: PiSugar3, X1202, X728.
REQ-401: When detection finds a known UPS, the system shall report `{model: "<name>", i2c_addr: "<addr>", driver: "<driver_name>"}`.
REQ-402: When detection finds no known UPS, the system shall report `{detected: false}`.

## Per-driver implementations

REQ-403: The system shall include a PiSugar3 driver in `internal/handlers/ups_pisugar3.go` for the PiSugar3 hat.
REQ-404: The system shall include an X1202 driver in `internal/handlers/ups_x1202.go` for the Geekworm X1202 hat.
REQ-405: The system shall include an X728 driver in `internal/handlers/ups_x728.go` for the Geekworm X728 hat.
REQ-406: The system shall expose battery percentage, voltage, charging-state, time-on-battery, and time-to-empty or time-to-full (where the driver supports it) for every shipped UPS driver.

## Configure UPS

REQ-407: The system shall expose `POST /power/ups/configure` accepting `{driver, i2c_addr}` to lock the active UPS driver.
REQ-408: The system shall persist the active UPS configuration at `/cubeos/config/ups.json`.

## Power monitor

REQ-409: The system shall provide a `PowerMonitor` background service that polls UPS state on a configurable interval (default 5 seconds).
REQ-410: The system shall expose `POST /power/monitor/start` and `POST /power/monitor/stop`.
REQ-411: The system shall expose `GET /power/monitor/status` returning whether the monitor is running + last-poll timestamp + last-poll values.
REQ-412: When the monitor detects critical battery (< 5% configurable), the system shall log a CRITICAL event.

## Generic power surface

REQ-413: The system shall expose `GET /power/status` returning aggregate power state (PSU + UPS + battery + charging).
REQ-414: The system shall expose `GET /power/battery` returning battery details only.
REQ-415: The system shall expose `POST /power/charging` accepting `{enabled: bool}` to control charging where the UPS supports it.
REQ-416: The system shall expose `POST /power/battery/quickstart` for UPS drivers that require explicit quick-start (X728).

## Multi-driver coexistence

REQ-417: While multiple UPS hats are detected (rare but possible), the system shall pick the first match in priority order: PiSugar3, X1202, X728.
REQ-418: The system shall NEVER drive two UPS hats simultaneously (would I2C-conflict).
