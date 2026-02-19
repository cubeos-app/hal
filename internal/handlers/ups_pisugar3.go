package handlers

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ============================================================================
// PiSugar 3 Driver (Pi Zero / Pi 3/4/5 via Plus variant)
// ============================================================================
//
// Portable 1200mAh UPS (standard) / 5000mAh (Plus variant).
// Custom MCU at I2C 0x57. No GPIO needed — everything is register-based.
// Also exposes DS3231-compatible RTC at 0x68.
//
// Key registers:
//   0x02 - Status/control bitfield
//   0x04 - MCU temperature (raw - 40 = °C)
//   0x22 - Voltage high byte  }  (high<<8 | low) = millivolts
//   0x23 - Voltage low byte   }
//   0x2A - Battery percentage (direct 0-100)
//
// Register 0x02 bitfield:
//   Bit 7 (R):   External power — 1 = USB-C connected
//   Bit 6 (RW):  Charging switch — 1 = enabled
//   Bit 5 (RW):  Output delay control
//   Bit 4 (RW):  Auto power-on when external power restored
//   Bit 2 (RW):  Output switch — 1 = 5V on, 0 = 5V off
//   Bit 0 (R):   Button state — 1 = pressed
//
// Write protection (firmware ≥v1.2.4):
//   Before ANY register write: i2cset -y 1 0x57 0x0B 0x29 (unlock)
//   After writes:              i2cset -y 1 0x57 0x0B 0xFF (lock)

const (
	pisugar3Addr        = "0x57"
	pisugar3RegStatus   = "0x02"
	pisugar3RegTemp     = "0x04"
	pisugar3RegVoltHigh = "0x22"
	pisugar3RegVoltLow  = "0x23"
	pisugar3RegSOC      = "0x2a"
	pisugar3RegUnlock   = "0x0b"
	pisugar3UnlockVal   = "0x29"
	pisugar3LockVal     = "0xff"
)

// PiSugar3Driver implements UPSDriver for the PiSugar 3 UPS.
type PiSugar3Driver struct {
	i2cBus string // e.g. "1"
}

func (d *PiSugar3Driver) Name() string {
	return "PiSugar 3"
}

// ReadStatus reads battery status from PiSugar 3 registers.
func (d *PiSugar3Driver) ReadStatus(ctx context.Context) (*BatteryReading, error) {
	reading := &BatteryReading{
		Available:  false,
		DeviceName: d.Name(),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	// Read voltage: high byte at 0x22, low byte at 0x23
	// Combined value is millivolts
	voltHigh, err := readI2CByte(ctx, d.i2cBus, pisugar3Addr, pisugar3RegVoltHigh)
	if err != nil {
		return reading, fmt.Errorf("read voltage high: %w", err)
	}
	voltLow, err := readI2CByte(ctx, d.i2cBus, pisugar3Addr, pisugar3RegVoltLow)
	if err != nil {
		return reading, fmt.Errorf("read voltage low: %w", err)
	}
	millivolts := (uint16(voltHigh) << 8) | uint16(voltLow)
	reading.Voltage = float64(millivolts) / 1000.0
	reading.Available = true

	// Read SOC: direct 0-100 at register 0x2A
	soc, err := readI2CByte(ctx, d.i2cBus, pisugar3Addr, pisugar3RegSOC)
	if err != nil {
		log.Printf("PowerMonitor: PiSugar3 SOC read error: %v", err)
	} else {
		reading.Percentage = float64(soc)
	}

	// Read status register 0x02 for AC power and charging
	status, err := readI2CByte(ctx, d.i2cBus, pisugar3Addr, pisugar3RegStatus)
	if err != nil {
		log.Printf("PowerMonitor: PiSugar3 status read error: %v", err)
	} else {
		// Bit 7: External power (1 = USB-C connected)
		reading.ACPresent = (status & 0x80) != 0
		// Bit 6: Charging switch (1 = enabled)
		reading.ChargingEnabled = (status & 0x40) != 0
		// Charging = external power present AND charging switch enabled
		reading.IsCharging = reading.ACPresent && reading.ChargingEnabled
	}

	// Read temperature: register 0x04, raw - 40 = °C
	temp, err := readI2CByte(ctx, d.i2cBus, pisugar3Addr, pisugar3RegTemp)
	if err == nil {
		reading.Temperature = float64(temp) - 40.0
	}

	return reading, nil
}

func (d *PiSugar3Driver) SupportsChargeControl() bool {
	return true
}

// InitiateShutdown clears the output switch (bit 2 of register 0x02) to cut 5V.
// Uses read-modify-write to preserve other bits in the status register.
// Unlocks write protection before writing, re-locks after.
func (d *PiSugar3Driver) InitiateShutdown(ctx context.Context) error {
	log.Printf("PowerMonitor: PiSugar3 shutdown — clearing output switch")

	// Read current status register
	status, err := readI2CByte(ctx, d.i2cBus, pisugar3Addr, pisugar3RegStatus)
	if err != nil {
		return fmt.Errorf("read status register: %w", err)
	}

	// Clear bit 2 (output switch) — read-modify-write
	newStatus := status &^ 0x04

	// Unlock write protection
	if _, err := execWithTimeout(ctx, "i2cset", "-y", d.i2cBus, pisugar3Addr, pisugar3RegUnlock, pisugar3UnlockVal); err != nil {
		return fmt.Errorf("unlock write protection: %w", err)
	}

	// Write modified status
	writeVal := fmt.Sprintf("0x%02x", newStatus)
	if _, err := execWithTimeout(ctx, "i2cset", "-y", d.i2cBus, pisugar3Addr, pisugar3RegStatus, writeVal); err != nil {
		// Re-lock even on error
		execWithTimeout(ctx, "i2cset", "-y", d.i2cBus, pisugar3Addr, pisugar3RegUnlock, pisugar3LockVal) //nolint: errcheck
		return fmt.Errorf("write status register: %w", err)
	}

	// Re-lock write protection
	if _, err := execWithTimeout(ctx, "i2cset", "-y", d.i2cBus, pisugar3Addr, pisugar3RegUnlock, pisugar3LockVal); err != nil {
		log.Printf("PowerMonitor: PiSugar3 re-lock failed (non-fatal): %v", err)
	}

	log.Printf("PowerMonitor: PiSugar3 output switch cleared")
	return nil
}

// OnBoot is a no-op for PiSugar 3 — no boot-OK signal required.
func (d *PiSugar3Driver) OnBoot(ctx context.Context) error {
	log.Printf("PowerMonitor: PiSugar3 boot — no initialization needed")
	return nil
}
