# CubeOS HAL (Hardware Abstraction Layer)

Privileged service providing hardware access to unprivileged CubeOS containers.

## Architecture

HAL runs as a Docker Compose service with host networking and full hardware access.
The CubeOS API communicates with HAL via REST — no other service talks to HAL directly.

```
Dashboard → API → HAL → Hardware (GPIO, I2C, network, storage, etc.)
```

## Endpoints

80+ REST API endpoints across 14 hardware categories:
- System (uptime, temperature, throttle, EEPROM, boot config)
- Power (battery, UPS, RTC, watchdog)
- Storage (devices, SMART, USB storage, mounts)
- Network (interfaces, AP, WiFi scan/connect, firewall, ports)
- VPN (WireGuard, OpenVPN, Tor)
- GPS, Cellular, Meshtastic, Iridium
- Camera, Audio, Sensors (1-Wire, BME280, SDR)
- GPIO, I2C, USB, Bluetooth

## Development

```bash
go mod download
go build ./cmd/cubeos-hal/
./cubeos-hal  # listens on :6005
```

## API Docs

Swagger annotations generate OpenAPI spec automatically in CI:
```
http://cubeos.cube:6005/hal/docs
```

## Testing

```bash
./tests/hal-test.sh            # Full 40+ endpoint test suite
./tests/test-power.sh          # Power/UPS subsystem tests
```
