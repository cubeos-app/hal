# 5. Long-lived drivers live in `HALHandler` struct fields

Date: 2026-05-18 (codifying shipped design)

## Status

Accepted

## Context

A naive approach would construct an IridiumDriver / MeshtasticDriver per-request in the handler function. That breaks:

- AT command serialisation (multiple driver instances racing on the same serial port).
- Buffered state (recent messages, signal strength history).
- Long-running connect/disconnect cycles.

The Go stdlib idiom would be sync.Once / global, but that's worse than a struct field — globals are hard to test.

## Decision

`HALHandler` (in `handlers/handlers.go`) holds long-lived drivers as struct fields:

```go
type HALHandler struct {
  powerMonitor *PowerMonitor
  iridium      *IridiumDriver
  meshtastic   *MeshtasticDriver
  supervisor   *DeviceSupervisor
  tier         string
  streamMu     sync.Mutex
  streamCmd    *exec.Cmd
  streamCancel func()
}
```

`NewHALHandler()` initialises them once. Per-request handler functions reach the driver via `h.iridium.SendMessage(...)` — never construct a new driver.

## Consequences

**Positive:**
- One driver per HAL process. Serial ports + AT command queues are coherent.
- Testable: pass a `*HALHandler` with mock drivers to handler functions.
- Initialisation order is explicit (NewHALHandler sets env-disable flags + driver-init order).

**Negative:**
- HALHandler accumulates fields as new drivers ship. Mitigated by grouping related state (e.g. `streamMu/streamCmd/streamCancel` are camera-stream state).

**Enforced by:** Component Article C-VI + `handlers.go` struct definition.
