package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Geekworm X728 Driver (Pi 2/3/4)
// ============================================================================
//
// 18650 UPS & Power Management Board with Auto On & Safe Shutdown.
// Same MAX17040 fuel gauge as X1202 at I2C 0x36, plus onboard MCU for
// button/shutdown management and DS1307 RTC at 0x68.
//
// CRITICAL: GPIO 6 polarity is INVERTED compared to X1202:
//   - X1202: GPIO 6 HIGH = AC present
//   - X728:  GPIO 6 HIGH = AC LOST
//
// GPIO (BCM numbering):
//   - GPIO 6  input:  HIGH = AC LOST (inverted!)
//   - GPIO 5  input:  Shutdown/reboot signal from MCU (pulse-width encoded)
//   - GPIO 12 output: Boot-OK heartbeat — MUST set HIGH on boot
//   - GPIO 26 output: Software shutdown signal (V2.0+) — pulse HIGH 4s
//   - GPIO 20 output: Buzzer (V2.1+)
//
// IMPORTANT: This driver uses Linux sysfs GPIO (/sys/class/gpio/) instead of
// libgpiod tools (gpioset/gpioget). The reason is that gpioset releases GPIO
// lines on exit, which means GPIO 12 (boot-OK) goes LOW immediately after the
// gpioset process ends — defeating the purpose. Sysfs-exported pins stay
// exported (and held at their set value) for the lifetime of the export.
//
// sysfs GPIO uses absolute pin numbers: gpioBase + BCM pin number.
//   - Pi 4: gpioBase = 512  → GPIO 12 = pin 524, GPIO 26 = pin 538
//   - Pi 5: gpioBase = 570  → GPIO 12 = pin 582, GPIO 26 = pin 596
//
// Shutdown protocol (MANDATORY):
//  1. Set GPIO 26 HIGH for 4 seconds, then LOW (official default)
//  2. Execute systemctl poweroff
//  3. OS halts → GPIO 12 goes LOW → MCU cuts 5V → enters standby
//
// Calling shutdown without pulsing GPIO 26 leaves the X728 draining batteries.

// X728Driver implements UPSDriver for the Geekworm X728 UPS board.
type X728Driver struct {
	i2cBus   string // e.g. "1"
	gpioChip string // e.g. "gpiochip0" — kept for reference
	gpioBase int    // sysfs GPIO base offset (512 on Pi 4, 570 on Pi 5)
}

func (d *X728Driver) Name() string {
	return "Geekworm X728"
}

// ============================================================================
// sysfs GPIO Helpers
// ============================================================================
//
// These functions interact with /sys/class/gpio/ to export, configure, read,
// and write GPIO pins. Unlike gpioset/gpioget, exported pins persist and retain
// their value until unexported — critical for the boot-OK signal on GPIO 12.

// sysfsExport exports a GPIO pin via sysfs. If the pin is already exported
// (EBUSY), it is treated as success — the pin is usable.
func sysfsExport(pin int) error {
	err := os.WriteFile("/sys/class/gpio/export", []byte(strconv.Itoa(pin)), 0200)
	if err != nil {
		// EBUSY means pin is already exported — that's fine
		if os.IsExist(err) || strings.Contains(err.Error(), "device or resource busy") {
			return nil
		}
		return fmt.Errorf("sysfs export gpio%d: %w", pin, err)
	}
	// Small delay for sysfs node creation
	time.Sleep(50 * time.Millisecond)
	return nil
}

// sysfsSetDirection sets the direction of an exported GPIO pin ("in" or "out").
func sysfsSetDirection(pin int, dir string) error {
	path := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", pin)
	if err := os.WriteFile(path, []byte(dir), 0200); err != nil {
		return fmt.Errorf("sysfs set direction gpio%d=%s: %w", pin, dir, err)
	}
	return nil
}

// sysfsWrite writes a value ("0" or "1") to an exported GPIO pin.
func sysfsWrite(pin int, value string) error {
	path := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	if err := os.WriteFile(path, []byte(value), 0200); err != nil {
		return fmt.Errorf("sysfs write gpio%d=%s: %w", pin, value, err)
	}
	return nil
}

// sysfsRead reads the current value of an exported GPIO pin.
// Returns "0" or "1" (trimmed).
func sysfsRead(pin int) (string, error) {
	path := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sysfs read gpio%d: %w", pin, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ============================================================================
// GPIO Base Detection
// ============================================================================

// detectGPIOBase dynamically detects the sysfs GPIO base offset by reading
// /sys/class/gpio/gpiochipN/base for the detected GPIO chip.
//
// Known values:
//   - Pi 4 (gpiochip0): base = 512
//   - Pi 5 (gpiochip4): base = 570
//
// Falls back to hardcoded defaults if sysfs read fails.
func detectGPIOBase() int {
	chip := detectGPIOChip() // "gpiochip0" or "gpiochip4"
	chipNum := strings.TrimPrefix(chip, "gpiochip")
	basePath := fmt.Sprintf("/sys/class/gpio/gpiochip%s/base", chipNum)

	data, err := os.ReadFile(basePath)
	if err != nil {
		// Fallback to known defaults
		piVer := detectPiVersion()
		if piVer >= 5 {
			log.Printf("PowerMonitor: X728 GPIO base detection failed, defaulting to 570 (Pi 5)")
			return 570
		}
		log.Printf("PowerMonitor: X728 GPIO base detection failed, defaulting to 512 (Pi 4)")
		return 512
	}

	base, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		log.Printf("PowerMonitor: X728 GPIO base parse error for %q, defaulting to 512", string(data))
		return 512
	}

	log.Printf("PowerMonitor: X728 GPIO base detected: %d (from %s)", base, basePath)
	return base
}

// ============================================================================
// UPSDriver Interface Implementation
// ============================================================================

// ReadStatus reads battery status from MAX17040 and GPIO pins.
// GPIO 6 polarity is inverted compared to X1202.
// Uses sysfs for GPIO reads instead of gpioget.
func (d *X728Driver) ReadStatus(ctx context.Context) (*BatteryReading, error) {
	reading := &BatteryReading{
		Available:  false,
		DeviceName: d.Name(),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	// Read voltage from MAX17040 register 0x02 — same as X1202
	raw, err := readI2CWord(ctx, d.i2cBus, "0x36", "0x02")
	if err != nil {
		return reading, fmt.Errorf("read voltage: %w", err)
	}
	reading.Voltage = float64(raw>>4) * 1.25 / 1000.0
	reading.Available = true

	// Read SOC from MAX17040 register 0x04 — same as X1202
	socRaw, err := readI2CWord(ctx, d.i2cBus, "0x36", "0x04")
	if err != nil {
		log.Printf("PowerMonitor: X728 SOC read error: %v", err)
	} else {
		reading.Percentage = float64(socRaw>>8) + float64(socRaw&0xFF)/256.0
	}

	// Check GPIO 6 for AC power via sysfs: HIGH = AC LOST (inverted from X1202!)
	acPin := d.gpioBase + 6
	if err := sysfsExport(acPin); err == nil {
		// Set direction to "in" for reading
		if err := sysfsSetDirection(acPin, "in"); err == nil {
			if val, err := sysfsRead(acPin); err == nil {
				// X728: HIGH (1) means AC lost, so AC present = value is "0"
				reading.ACPresent = val == "0"
			} else {
				log.Printf("PowerMonitor: X728 GPIO 6 read error: %v", err)
			}
		} else {
			log.Printf("PowerMonitor: X728 GPIO 6 direction error: %v", err)
		}
	} else {
		log.Printf("PowerMonitor: X728 GPIO 6 export error: %v", err)
	}

	// X728 V2.5 has GPIO 16 charge control, but not all versions.
	// Default to charging enabled when AC is present.
	reading.ChargingEnabled = reading.ACPresent
	reading.IsCharging = reading.ACPresent

	return reading, nil
}

func (d *X728Driver) SupportsChargeControl() bool {
	// Only V2.5 has GPIO 16 charge control. Since we can't reliably
	// detect the hardware version, report false by default.
	return false
}

// InitiateShutdown performs the X728 shutdown protocol:
// Pulse GPIO 26 HIGH for 4 seconds (official default) to tell the MCU to
// prepare for power cutoff. Falls back to GPIO 13 for V1.x boards.
// Uses sysfs GPIO instead of gpioset.
func (d *X728Driver) InitiateShutdown(ctx context.Context) error {
	log.Printf("PowerMonitor: X728 shutdown — pulsing GPIO 26 HIGH for 4 seconds (sysfs)")

	shutdownPin := d.gpioBase + 26 // 538 on Pi 4, 596 on Pi 5

	// Try GPIO 26 first (V2.0+)
	if err := sysfsExport(shutdownPin); err != nil {
		return d.shutdownFallbackGPIO13(ctx)
	}
	if err := sysfsSetDirection(shutdownPin, "out"); err != nil {
		return d.shutdownFallbackGPIO13(ctx)
	}
	if err := sysfsWrite(shutdownPin, "1"); err != nil {
		return d.shutdownFallbackGPIO13(ctx)
	}

	// Hold HIGH for 4 seconds (official X728 default, not 3s)
	time.Sleep(4 * time.Second)

	if err := sysfsWrite(shutdownPin, "0"); err != nil {
		log.Printf("PowerMonitor: X728 GPIO 26 LOW failed (non-fatal): %v", err)
	}

	log.Printf("PowerMonitor: X728 shutdown signal complete (GPIO 26, sysfs)")
	return nil
}

// shutdownFallbackGPIO13 tries GPIO 13 for V1.x X728 boards via sysfs.
func (d *X728Driver) shutdownFallbackGPIO13(ctx context.Context) error {
	log.Printf("PowerMonitor: X728 GPIO 26 failed, trying GPIO 13 (V1.x fallback, sysfs)")

	fallbackPin := d.gpioBase + 13

	if err := sysfsExport(fallbackPin); err != nil {
		return fmt.Errorf("X728 shutdown signal failed on both GPIO 26 and 13: export 13: %w", err)
	}
	if err := sysfsSetDirection(fallbackPin, "out"); err != nil {
		return fmt.Errorf("X728 shutdown signal failed on both GPIO 26 and 13: direction 13: %w", err)
	}
	if err := sysfsWrite(fallbackPin, "1"); err != nil {
		return fmt.Errorf("X728 shutdown signal failed on both GPIO 26 and 13: write 13: %w", err)
	}

	time.Sleep(4 * time.Second)

	if err := sysfsWrite(fallbackPin, "0"); err != nil {
		log.Printf("PowerMonitor: X728 GPIO 13 LOW failed (non-fatal): %v", err)
	}

	log.Printf("PowerMonitor: X728 shutdown signal complete (GPIO 13 fallback, sysfs)")
	return nil
}

// OnBoot sets GPIO 12 HIGH via sysfs to signal the X728 MCU that the Pi has booted.
// The pin stays exported (and held HIGH) for the entire container lifetime.
// If this signal is not sent, the MCU may cut power thinking boot failed.
//
// CRITICAL: This is why we use sysfs instead of gpioset — gpioset releases the
// GPIO line on process exit, causing GPIO 12 to go LOW immediately. With sysfs,
// the pin remains exported and HIGH until explicitly unexported or system shutdown.
func (d *X728Driver) OnBoot(ctx context.Context) error {
	bootPin := d.gpioBase + 12 // 524 on Pi 4, 582 on Pi 5
	log.Printf("PowerMonitor: X728 boot — setting sysfs GPIO %d HIGH (boot-OK signal)", bootPin)

	if err := sysfsExport(bootPin); err != nil {
		return fmt.Errorf("X728 boot-OK signal failed: export: %w", err)
	}

	if err := sysfsSetDirection(bootPin, "out"); err != nil {
		return fmt.Errorf("X728 boot-OK signal failed: direction: %w", err)
	}

	if err := sysfsWrite(bootPin, "1"); err != nil {
		return fmt.Errorf("X728 boot-OK signal failed: write: %w", err)
	}

	log.Printf("PowerMonitor: X728 boot-OK signal sent (sysfs GPIO %d = HIGH, held)", bootPin)
	return nil
}
