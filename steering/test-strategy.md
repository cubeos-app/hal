# Steering — Test strategy

## Convention: colocated `<source>_test.go`

Go-standard. Test files live alongside source in the same package:

```
internal/handlers/device_registry.go      + device_registry_test.go
internal/handlers/meshtastic_serial.go    + meshtastic_serial_test.go
internal/middleware/auth.go               + auth_test.go
```

New tests for new code MUST follow the same colocation.

## What to test

| Surface | Test approach |
|---|---|
| Middleware (auth, ACL, tier-gate) | Unit tests with httptest.NewRequest + ResponseRecorder. Cover: missing key, wrong key, sufficient role, insufficient role, tier-gated endpoint in container mode. |
| Handlers (per-domain) | Table-driven tests with mocked driver / shelled-out command. Cover: happy path, validation rejects, downstream error surfaces as HTTP 5xx. |
| Drivers (IridiumDriver, MeshtasticDriver) | Mock-serial test harness — drivers use a `SerialPort` interface that tests replace with an in-memory fake. |
| DeviceSupervisor | Unit test the event subscription + dispatch logic with a fake event source. |
| UPS drivers (x728, x1202, pisugar3, generic) | I2C-mock test harness — driver speaks an `I2CBus` interface, tests inject a fake. |
| Validators (validators.go) | Pure-function table-driven tests. Cover: every validator function with positive + negative cases. |

## Running

```bash
cd hal
go test ./...                          # all packages
go test ./internal/handlers/           # just handlers
go test -run TestMeshtasticSerial ./internal/handlers/   # single test
go test -race ./...                    # with race detector (recommended)
go test -cover ./...                   # coverage
```

## Coverage expectation

Per CubeOS project rule: every PR touching `handlers/`, `middleware/`, or `config/` SHALL include corresponding `_test.go` changes. PRs without tests fail review.

## Hardware-dependent paths

Some HAL surfaces (real serial-port driver, real I2C, real GPIO) require physical hardware to exercise end-to-end. These have:

- Unit tests via mock interfaces (CI runs these)
- Integration tests via `//go:build integration` build tag (CI runs only when explicitly requested; operator runs against real Pi via `go test -tags=integration ./...`)

## Anti-patterns

- Tests that require an LXC / Docker / Pi to pass `go test ./...` — those go behind the `integration` build tag.
- Tests that rely on `time.Sleep` to wait for race conditions — use `sync.WaitGroup` + channels instead.
- `t.Fatal` after partial work without cleanup — use `t.Cleanup(...)` to register teardown.
- Test files that share state via package-level vars across `t.Run` blocks — use per-subtest fresh state.
