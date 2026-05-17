# Design — Iridium + Meshtastic supervisor (spec/004)

Retrospective. The shipped driver pattern: long-lived structs on `HALHandler`, env-disable, transport-agnostic via interfaces, SSE event streams.

## Driver init (excerpt of NewHALHandler)

```go
func NewHALHandler() *HALHandler {
  tier := os.Getenv("CUBEOS_TIER")
  if tier == "" { tier = "full" }

  var iridium *IridiumDriver
  var meshtastic *MeshtasticDriver

  if os.Getenv("HAL_DISABLE_IRIDIUM") != "true" {
    iridium = NewIridiumDriver()
  } else {
    log.Printf("HAL: Iridium driver disabled (HAL_DISABLE_IRIDIUM=true)")
  }
  if os.Getenv("HAL_DISABLE_MESHTASTIC") != "true" {
    meshtastic = NewMeshtasticDriver()
  } else {
    log.Printf("HAL: Meshtastic driver disabled (HAL_DISABLE_MESHTASTIC=true)")
  }

  return &HALHandler{
    powerMonitor: NewPowerMonitor(),
    iridium:      iridium,
    meshtastic:   meshtastic,
    supervisor:   NewDeviceSupervisor(),
    tier:         tier,
  }
}
```

## Meshtastic transport multiplex

`handlers/meshtastic.go` is the routing layer that decides whether to use the BLE adapter or the serial adapter based on the device address format:

- BLE: `meshtastic://ble/<mac-address>`
- Serial: `meshtastic://serial/<device-path>` (e.g. `/dev/ttyUSB0`)

`MeshtasticDriver` holds both adapters and dispatches per connection.

## SSE event streams

`/meshtastic/events` + `/iridium/events` use `text/event-stream`:

```go
func (h *HALHandler) StreamMeshtasticEvents(w http.ResponseWriter, r *http.Request) {
  w.Header().Set("Content-Type", "text/event-stream")
  w.Header().Set("Cache-Control", "no-cache")
  flusher := w.(http.Flusher)
  events := h.meshtastic.SubscribeEvents(r.Context())
  for evt := range events {
    fmt.Fprintf(w, "data: %s\n\n", evt.JSON())
    flusher.Flush()
  }
}
```

Client disconnect closes `r.Context()`; subscriber goroutine cleans up.

## DeviceSupervisor

Single goroutine that watches:
- udev for `/dev/tty*` add/remove
- BLE scan results (when meshtastic_ble is active)
- USB topology changes

Subscribers register a function called on each event. Drivers register at init time:

```go
h.supervisor.Subscribe(EventTypeUSBAdd, func(e Event) {
  if isMeshtasticSerial(e.Device) { h.meshtastic.OnDeviceAdded(e) }
  if isIridiumSerial(e.Device)    { h.iridium.OnDeviceAdded(e)    }
})
```

## Out of scope

- Long-term message archival (drivers hold in-memory ring; persistent storage is api/'s concern).
- Cross-driver routing (api/'s FlowEngine activities own that).
- Per-message encryption (transport-agnostic; meshsat/ handles).
