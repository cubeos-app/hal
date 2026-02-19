package handlers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// ============================================================================
// Geekworm X1202 Driver (Pi 5)
// ============================================================================
//
// 4-Cell 18650 5.1V 5A UPS HAT for Raspberry Pi 5.
// Uses MAX17040G+ fuel gauge at I2C 0x36.
// Four 18650 cells in parallel → pack voltage = single-cell voltage (3.0–4.2V).
// Connects to Pi 5 via pogo pins (not GPIO header).
//
// GPIO (BCM, gpiochip4 on Pi 5):
//   - GPIO 6 input:  HIGH = AC present, LOW = AC lost
//   - GPIO 16 output: LOW = charging enabled, HIGH = charging disabled
//
// No shutdown handshake needed — X1202 MCU auto-detects Pi halt via current
// draw and cuts power. Requires POWER_OFF_ON_HALT=1 in Pi 5 EEPROM config.

// X1202Driver implements UPSDriver for the Geekworm X1202 UPS HAT.
type X1202Driver struct {
	i2cBus   string // e.g. "1"
	gpioChip string // e.g. "gpiochip4"
}

func (d *X1202Driver) Name() string {
	return "Geekworm X1202"
}

// ReadStatus reads battery status from the MAX17040 fuel gauge and GPIO pins.
// This is the same logic previously in getBatteryStatus() but normalized to
// the BatteryReading type.
func (d *X1202Driver) ReadStatus(ctx context.Context) (*BatteryReading, error) {
	reading := &BatteryReading{
		Available:  false,
		DeviceName: d.Name(),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	// Read voltage from MAX17040 register 0x02
	// Formula: voltage = (raw >> 4) * 0.00125 V
	raw, err := readI2CWord(ctx, d.i2cBus, "0x36", "0x02")
	if err != nil {
		return reading, fmt.Errorf("read voltage: %w", err)
	}
	reading.Voltage = float64(raw>>4) * 1.25 / 1000.0
	reading.Available = true

	// Read SOC from MAX17040 register 0x04
	// Formula: percentage = raw / 256.0 (high byte = integer, low byte = fraction)
	socRaw, err := readI2CWord(ctx, d.i2cBus, "0x36", "0x04")
	if err != nil {
		log.Printf("PowerMonitor: X1202 SOC read error: %v", err)
	} else {
		reading.Percentage = float64(socRaw>>8) + float64(socRaw&0xFF)/256.0
	}

	// Check GPIO 6 for AC power: HIGH = AC present, LOW = AC lost
	if output, err := execWithTimeout(ctx, "gpioget", d.gpioChip, "6"); err == nil {
		reading.ACPresent = strings.TrimSpace(output) == "1"
	}

	// Check GPIO 16 for charging enabled: LOW (0) = charging, HIGH (1) = not charging
	if output, err := execWithTimeout(ctx, "gpioget", d.gpioChip, "16"); err == nil {
		reading.ChargingEnabled = strings.TrimSpace(output) == "0"
	}

	// Charging = AC present AND charging enabled
	reading.IsCharging = reading.ACPresent && reading.ChargingEnabled

	return reading, nil
}

func (d *X1202Driver) SupportsChargeControl() bool {
	return true
}

// InitiateShutdown is a no-op for X1202.
// The MCU auto-detects Pi halt via current draw and cuts power.
func (d *X1202Driver) InitiateShutdown(ctx context.Context) error {
	log.Printf("PowerMonitor: X1202 shutdown — no handshake needed, MCU auto-detects halt")
	return nil
}

// OnBoot is a no-op for X1202 — no boot-OK signal required.
func (d *X1202Driver) OnBoot(ctx context.Context) error {
	log.Printf("PowerMonitor: X1202 boot — no initialization needed")
	return nil
}
