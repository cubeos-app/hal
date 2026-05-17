# Requirements — Iridium + Meshtastic driver supervisor (spec/004 — RETROSPECTIVE)

Source: `internal/handlers/iridium_driver.go`, `meshtastic_driver.go`, `meshtastic_ble.go`, `meshtastic_serial.go`, `device_supervisor.go`, `device_registry.go` (CGC-verified).

> ID convention: 300-block.

## Driver lifecycle

REQ-300: The system shall instantiate `IridiumDriver` once in `NewHALHandler()` and hold it as `h.iridium *IridiumDriver`.
REQ-301: The system shall instantiate `MeshtasticDriver` once in `NewHALHandler()` and hold it as `h.meshtastic *MeshtasticDriver`.
REQ-302: When `HAL_DISABLE_IRIDIUM=true`, the system shall NOT instantiate IridiumDriver and shall log "Iridium driver disabled" once at startup.
REQ-303: When `HAL_DISABLE_MESHTASTIC=true`, the system shall NOT instantiate MeshtasticDriver and shall log "Meshtastic driver disabled" once at startup.
REQ-304: While `h.iridium == nil`, the system shall return HTTP 501 with body "Iridium driver disabled (HAL_DISABLE_IRIDIUM=true)" on `/iridium/*` routes.
REQ-305: While `h.meshtastic == nil`, the system shall return HTTP 501 with body "Meshtastic driver disabled (HAL_DISABLE_MESHTASTIC=true)" on `/meshtastic/*` routes.

## DeviceSupervisor

REQ-306: The system shall hold a single `DeviceSupervisor` instance on `h.supervisor *DeviceSupervisor`.
REQ-307: The system shall route every udev / BLE / hot-plug event through the supervisor.
REQ-308: While a driver is initialised, the driver shall subscribe to supervisor events for its device class.

## Meshtastic transport multiplex

REQ-309: The system shall support Meshtastic over both BLE (`handlers/meshtastic_ble.go`) and serial (`handlers/meshtastic_serial.go`).
REQ-310: While a Meshtastic device is connected, the driver shall expose: `/meshtastic/devices`, `/meshtastic/status`, `/meshtastic/nodes`, `/meshtastic/position`, `/meshtastic/connect`, `/meshtastic/disconnect`.
REQ-311: The system shall expose Meshtastic admin commands: reboot, factory_reset, traceroute, remove_node, config radio, config module.
REQ-312: The system shall expose Meshtastic messaging: send, send_raw, channel switch, waypoints.
REQ-313: The system shall expose Meshtastic events as an SSE stream at `/meshtastic/events`.

## Iridium driver

REQ-314: The system shall expose Iridium endpoints: `/iridium/devices`, `/iridium/status`, `/iridium/signal`, `/iridium/signal/fast`, `/iridium/connect`, `/iridium/disconnect`, `/iridium/send`, `/iridium/mailbox_check`, `/iridium/receive`, `/iridium/messages`, `/iridium/clear`, `/iridium/at`.
REQ-315: The system shall expose Iridium events as an SSE stream at `/iridium/events`.
REQ-316: The system shall provide an AT command escape hatch (`POST /iridium/at`) for operator-driven debugging.

## Crash recovery

REQ-317: If the driver crashes (panic), then the recovery middleware shall log + return 500 without taking down HAL.
REQ-318: When the driver becomes unhealthy (serial port dropped), the driver shall surface this via `/iridium/status` or `/meshtastic/status` `connected: false`.

## Tests

REQ-319: The system shall test the disabled-driver 501 path via colocated `_test.go` (e.g. `meshtastic_serial_test.go` shipped).
REQ-320: The system shall test serial-port mocking via the `SerialPort` interface that real drivers use.
