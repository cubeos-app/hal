# Requirements — Watchdog + RTC subsystem (spec/002 — RETROSPECTIVE)

Source: `internal/handlers/routes.go` `/watchdog/*` and `/rtc/*` route blocks (CGC-verified).

> ID convention: 100-block.

## Watchdog

REQ-100: The system shall expose `GET /watchdog/status` returning current watchdog state including pet interval + hardware timeout.
REQ-101: The system shall expose `POST /watchdog/pet` triggering a one-shot pet of `/dev/watchdog`.
REQ-102: The system shall expose `POST /watchdog/enable` activating the hardware watchdog (writes to config + opens `/dev/watchdog` for the lifetime of HAL).
REQ-103: While watchdog is enabled, the system shall pet `/dev/watchdog` every 5 seconds with a hardware timeout of 15 seconds.
REQ-104: If `/dev/watchdog` is unavailable, then the system shall return HTTP 503 from `POST /watchdog/enable` with an actionable error body explaining the missing kernel module.

## RTC

REQ-105: The system shall expose `GET /rtc/status` returning RTC presence + current RTC time vs system time + drift.
REQ-106: The system shall expose `POST /rtc/sync-to-rtc` writing current system time to the RTC chip.
REQ-107: The system shall expose `POST /rtc/sync-from-rtc` reading RTC time and setting system clock.
REQ-108: The system shall expose `POST /rtc/wakealarm` accepting a Unix timestamp; on success the system shall wake from suspend at that time.
REQ-109: The system shall expose `DELETE /rtc/wakealarm` clearing any pending wake alarm.
REQ-110: If no RTC chip is detected, then the system shall return HTTP 503 on every `/rtc/*` endpoint with body explaining no RTC hardware.

## Operational guarantees

REQ-111: When HAL exits cleanly (SIGTERM), the system shall release `/dev/watchdog` so the kernel doesn't trigger a hardware reset.
REQ-112: While HAL is running, the system shall persist the most recent watchdog pet timestamp for `GET /watchdog/status` reporting.
REQ-113: The system shall NOT use `os.Exit(0)` on the watchdog enable path — releasing the fd properly requires defer.

## Tests

REQ-114: The system shall test the watchdog pet loop via a mock `/dev/watchdog` writer in `internal/handlers/system_test.go`.
REQ-115: The system shall test the RTC parsers (hwclock output parsing) in `internal/handlers/system_test.go`.
