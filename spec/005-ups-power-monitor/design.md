# Design — UPS + PowerMonitor (spec/005)

Retrospective. Real files: 6 in `internal/handlers/`:

```
ups_detect.go      — generic detector probing I2C
ups_pisugar3.go    — PiSugar3 driver
ups_x1202.go       — Geekworm X1202 driver
ups_x728.go        — Geekworm X728 driver
power.go           — generic /power/* handlers
power_monitor.go   — background polling service
```

## Detection flow

```
GET /power/ups/detect
   │
   ▼
probe I2C addr 0x57 (PiSugar3)
   ↓ if present + responds correctly → driver=pisugar3
probe I2C addr 0x36 (X1202 + X728 share)
   ↓ if present, distinguish via extra register reads
   ↓ → driver=x1202 OR driver=x728
return {model, i2c_addr, driver}
```

## Per-driver interface (Go-typical)

```go
type UPSDriver interface {
  Detect(bus i2c.Bus) (bool, error)
  Connect(bus i2c.Bus, addr int) error
  Disconnect() error
  ReadBattery() (Battery, error)
  ReadVoltage() (float64, error)
  SetCharging(enabled bool) error  // returns ErrNotSupported if driver doesn't support
  QuickStart() error                // returns ErrNotSupported if not applicable
}

type Battery struct {
  PercentSOC      float64
  Voltage_V       float64
  Charging        bool
  OnBattery       bool
  TimeToEmpty_min int  // -1 if charging
  TimeToFull_min  int  // -1 if discharging
}
```

Each of pisugar3 / x1202 / x728 implements this.

## PowerMonitor service

```go
type PowerMonitor struct {
  driver       UPSDriver
  interval     time.Duration  // default 5s
  ctx          context.Context
  cancel       context.CancelFunc
  lastReading  Battery
  lastReadAt   time.Time
  criticalPct  float64        // default 5.0
}

func (pm *PowerMonitor) Start() error
func (pm *PowerMonitor) Stop() error
func (pm *PowerMonitor) Snapshot() (Battery, time.Time)
```

Logged events: every poll at DEBUG; critical-battery at CRITICAL.

## Persistence

`/cubeos/config/ups.json`:

```json
{
  "active_driver": "x1202",
  "i2c_addr": 54,
  "polling_interval_seconds": 5,
  "critical_pct": 5.0
}
```

Configured via `POST /power/ups/configure`.

## Out of scope

- UPS firmware updates (drivers may know the firmware version; flashing is OOB).
- Beyond-3-driver support — operator can add a driver file + register in ups_detect.
