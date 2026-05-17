# Design — Watchdog + RTC subsystem (spec/002)

Retrospective. Real handlers grouped in `internal/handlers/system.go` (or a related file in the same package). Routes registered in `routes.go` under `/watchdog/*` and `/rtc/*`.

## Watchdog flow

```
POST /watchdog/enable
   │
   ▼
open("/dev/watchdog", O_WRONLY)
   │  on EACCES / ENOENT → 503
   ▼
start goroutine:
  ticker := time.Tick(5*time.Second)
  for {
    select {
    case <-ticker:
      fd.Write([]byte{'X'})    // pet
      lastPetAt = time.Now()
    case <-ctx.Done():
      fd.Close()                // release on shutdown
      return
    }
  }
```

Kernel hardware watchdog timeout: 15s. If HAL crashes / hangs / is OOM-killed, the kernel resets the board.

## RTC flow

`/rtc/status` queries `hwclock --get` + parses; computes drift vs `time.Now()`.

`/rtc/wakealarm POST` writes the requested epoch seconds to `/sys/class/rtc/rtc0/wakealarm` via `echo 0 > wakealarm; echo <epoch> > wakealarm` sequence (kernel quirk requires clearing before setting).

`/rtc/wakealarm DELETE` writes `0` to `/sys/class/rtc/rtc0/wakealarm`.

## Hardware presence detection

On startup:
- `/dev/watchdog` exists + writable → watchdog available
- `/sys/class/rtc/rtc0/wakealarm` exists → RTC + wakealarm available

Both reported by `GET /system/info`'s capability set.

## Out of scope

- Software watchdog timers in user-space (use kernel watchdog only).
- RTC chip-specific quirks (DS3231 vs PCF8523) — abstracted by the kernel rtc subsystem.
- Wake-on-LAN — different subsystem (handled by /system/bootconfig or netplan).
