# 4. Per-driver env-disable via `HAL_DISABLE_*`

Date: 2026-05-18 (codifying shipped design)

## Status

Accepted

## Context

IridiumDriver + MeshtasticDriver each:

- Try to open serial ports at startup.
- Start a long-running poll loop (signal strength, AT commands, etc.).
- Log noisy errors when the hardware isn't present.

On a Pi without an Iridium modem, the driver fails to claim its port, then retries every 30 seconds forever. Same for Meshtastic on hardware without a radio. The log noise drowns useful events.

## Decision

Each cost-heavy / hardware-optional driver checks an env var on startup:

```go
if os.Getenv("HAL_DISABLE_IRIDIUM") != "true" {
  iridium = NewIridiumDriver()
}
// otherwise: log "Iridium driver disabled" once at startup
```

When the driver is nil, the corresponding routes return HTTP 501:

```go
r.Route("/iridium", func(r chi.Router) {
  if h.iridium != nil {
    // register full route set
  } else {
    r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
      errorResponse(w, http.StatusNotImplemented, "Iridium driver disabled (HAL_DISABLE_IRIDIUM=true)")
    })
  }
})
```

Same pattern for `HAL_DISABLE_MESHTASTIC`.

## Consequences

**Positive:**
- Clean log output on hardware-less hosts (no retry spam).
- 501 response is explicit + actionable for callers.
- One env var per driver — simple operator config.

**Negative:**
- Operator must remember to set the env var on hardware-less hosts. Mitigation: documented in `hal/CLAUDE.md` + this ADR.

**Enforced by:** Component Article C-IV + `NewHALHandler()` checks in `handlers/handlers.go`.
