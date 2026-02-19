package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ============================================================================
// UPS Driver Interface
// ============================================================================

// UPSDriver abstracts hardware differences between UPS HAT devices.
// Each supported UPS implements this interface so the PowerMonitor
// can work identically regardless of underlying hardware.
type UPSDriver interface {
	// Name returns the human-readable device name (e.g. "Geekworm X1202")
	Name() string

	// ReadStatus reads current battery status from hardware.
	// Returns a normalized BatteryReading regardless of underlying hardware.
	ReadStatus(ctx context.Context) (*BatteryReading, error)

	// SupportsChargeControl returns true if charging can be toggled.
	SupportsChargeControl() bool

	// InitiateShutdown performs device-specific shutdown sequence
	// (e.g., X728 GPIO 26 pulse, PiSugar output switch, X1202 no-op).
	InitiateShutdown(ctx context.Context) error

	// OnBoot performs any boot-time initialization
	// (e.g., X728 GPIO 12 boot-OK signal).
	OnBoot(ctx context.Context) error
}

// BatteryReading is the normalized output from any UPS driver.
type BatteryReading struct {
	Available       bool    `json:"available"`
	Voltage         float64 `json:"voltage"`    // Volts
	Percentage      float64 `json:"percentage"` // 0-100
	ACPresent       bool    `json:"ac_present"` // normalized: true = power connected
	IsCharging      bool    `json:"is_charging"`
	ChargingEnabled bool    `json:"charging_enabled"`
	Temperature     float64 `json:"temperature,omitempty"` // °C, PiSugar only
	DeviceName      string  `json:"device_name"`
	Timestamp       string  `json:"timestamp"`
}

// ============================================================================
// UPS Detection Result (Suggestion Only)
// ============================================================================

// UPSDetectionResult is the response for GET /power/ups/detect.
// This is a read-only probe result — it does NOT activate any driver
// or start the power monitor. The user must explicitly confirm via
// POST /power/ups/configure before any driver is loaded.
// @Description UPS auto-detection probe result (suggestion only, no activation)
type UPSDetectionResult struct {
	SuggestedModel string   `json:"suggested_model" example:"x1202"`                    // "x1202", "x728", "pisugar3", "none"
	SuggestedName  string   `json:"suggested_name" example:"Geekworm X1202"`            // Human-readable name
	PiModel        int      `json:"pi_model" example:"5"`                               // 4, 5, etc.
	GPIOChip       string   `json:"gpio_chip" example:"gpiochip4"`                      // "gpiochip0", "gpiochip4"
	I2CDevices     []string `json:"i2c_devices_found"`                                  // ["0x36", "0x68"]
	Confidence     string   `json:"confidence" example:"high"`                          // "high", "medium", "low"
	Warning        string   `json:"warning,omitempty"`                                  // e.g. "X1202 on Pi 4 is unusual"
	Details        string   `json:"details" example:"MAX17040 at 0x36, no RTC at 0x68"` // Human-readable explanation
}

// ============================================================================
// UPS Detection Probe Handler
// ============================================================================

// DetectUPSProbe probes the I2C bus to suggest which UPS HAT is installed.
// This is read-only — it does NOT activate any driver or start the power monitor.
// The user must explicitly confirm their selection via POST /power/ups/configure.
//
// @Summary Detect UPS HAT
// @Description Probes I2C bus to suggest which UPS HAT is installed (does not activate driver)
// @Tags Power
// @Produce json
// @Success 200 {object} UPSDetectionResult
// @Failure 500 {object} ErrorResponse
// @Router /power/ups/detect [get]
func (h *HALHandler) DetectUPSProbe(w http.ResponseWriter, r *http.Request) {
	piModel := detectPiVersion()
	gpioChip := detectGPIOChip()
	i2cBus := getEnvOrDefault("HAL_I2C_BUS", "1")

	result := UPSDetectionResult{
		SuggestedModel: "none",
		SuggestedName:  "None detected",
		PiModel:        piModel,
		GPIOChip:       gpioChip,
		I2CDevices:     []string{},
		Confidence:     "high",
		Details:        "No UPS HAT detected on I2C bus",
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Step 1: Probe PiSugar 3 at 0x57
	if probeI2CDevice(ctx, i2cBus, "0x57") {
		result.I2CDevices = append(result.I2CDevices, "0x57")

		// Verify by reading SOC register 0x2A — should return 0-100
		if output, err := execWithTimeout(ctx, "i2cget", "-y", i2cBus, "0x57", "0x2a"); err == nil {
			valStr := strings.TrimSpace(output)
			if val, parseErr := parseHexInt(valStr); parseErr == nil && val >= 0 && val <= 100 {
				result.SuggestedModel = "pisugar3"
				result.SuggestedName = "PiSugar 3"
				result.Details = fmt.Sprintf("PiSugar 3 at 0x57 (SOC=%d%%)", val)
				// PiSugar works on any Pi, so confidence stays high
				jsonResponse(w, http.StatusOK, result)
				return
			}
		}
		log.Printf("UPS detect probe: device at 0x57 did not pass PiSugar 3 validation")
	}

	// Step 2: Probe MAX17040 at 0x36
	if probeI2CDevice(ctx, i2cBus, "0x36") {
		result.I2CDevices = append(result.I2CDevices, "0x36")

		// Step 3: Disambiguate X1202 vs X728 by probing RTC at 0x68
		hasRTC := probeI2CDevice(ctx, i2cBus, "0x68")
		if hasRTC {
			result.I2CDevices = append(result.I2CDevices, "0x68")
		}

		if hasRTC {
			// RTC present → X728
			result.SuggestedModel = "x728"
			result.SuggestedName = "Geekworm X728"
			result.Details = "MAX17040 fuel gauge at 0x36 + DS1307 RTC at 0x68"

			// X728 is designed for Pi 2/3/4 — warn if on Pi 5
			if piModel >= 5 {
				result.Confidence = "low"
				result.Warning = fmt.Sprintf("X728 is designed for Pi 2/3/4, but this is a Pi %d. Please verify your hardware.", piModel)
			}
		} else {
			// No RTC → X1202
			result.SuggestedModel = "x1202"
			result.SuggestedName = "Geekworm X1202"
			result.Details = "MAX17040 fuel gauge at 0x36, no RTC at 0x68"

			// X1202 is designed for Pi 5 — warn if on Pi 4 or lower
			if piModel > 0 && piModel < 5 {
				result.Confidence = "low"
				result.Warning = fmt.Sprintf("X1202 is designed for Pi 5, but this is a Pi %d. Please verify your hardware.", piModel)
			}
		}

		jsonResponse(w, http.StatusOK, result)
		return
	}

	// Nothing detected
	result.Details = fmt.Sprintf("No UPS HAT detected on I2C bus %s", i2cBus)
	jsonResponse(w, http.StatusOK, result)
}

// ============================================================================
// UPS Auto-Detection
// ============================================================================

// DetectUPS probes I2C bus 1 to determine which UPS HAT is present.
// Detection order:
//  1. Probe 0x57 — if ACK + register 0x2A returns 0–100 → PiSugar 3
//  2. Probe 0x36 — if ACK → MAX17040 present (X1202 or X728)
//  3. Disambiguate: probe 0x68 for RTC. If responds → X728. Otherwise → X1202
//  4. If nothing responds → nil (no UPS detected)
//
// The HAL_UPS_MODEL env var can override auto-detection.
func DetectUPS() UPSDriver {
	model := strings.ToLower(strings.TrimSpace(os.Getenv("HAL_UPS_MODEL")))
	i2cBus := getEnvOrDefault("HAL_I2C_BUS", "1")
	gpioChip := detectGPIOChip()

	// Manual override
	switch model {
	case "x1202":
		log.Printf("PowerMonitor: UPS model forced to X1202 via HAL_UPS_MODEL")
		return &X1202Driver{i2cBus: i2cBus, gpioChip: gpioChip}
	case "x728":
		log.Printf("PowerMonitor: UPS model forced to X728 via HAL_UPS_MODEL")
		return &X728Driver{i2cBus: i2cBus, gpioChip: gpioChip, gpioBase: detectGPIOBase()}
	case "pisugar3":
		log.Printf("PowerMonitor: UPS model forced to PiSugar 3 via HAL_UPS_MODEL")
		return &PiSugar3Driver{i2cBus: i2cBus}
	case "auto", "":
		// Fall through to auto-detection
	default:
		log.Printf("PowerMonitor: unknown HAL_UPS_MODEL=%q, falling back to auto-detection", model)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Step 1: Probe PiSugar 3 at 0x57
	if probeI2CDevice(ctx, i2cBus, "0x57") {
		// Verify by reading SOC register 0x2A — should return 0-100
		if output, err := execWithTimeout(ctx, "i2cget", "-y", i2cBus, "0x57", "0x2a"); err == nil {
			valStr := strings.TrimSpace(output)
			if val, parseErr := parseHexInt(valStr); parseErr == nil && val >= 0 && val <= 100 {
				log.Printf("PowerMonitor: detected PiSugar 3 at 0x57 (SOC=%d%%)", val)
				return &PiSugar3Driver{i2cBus: i2cBus}
			}
		}
		log.Printf("PowerMonitor: device at 0x57 did not pass PiSugar 3 validation")
	}

	// Step 2: Probe MAX17040 at 0x36
	if probeI2CDevice(ctx, i2cBus, "0x36") {
		// Step 3: Disambiguate X1202 vs X728 by probing RTC at 0x68
		if probeI2CDevice(ctx, i2cBus, "0x68") {
			log.Printf("PowerMonitor: detected Geekworm X728 (MAX17040 at 0x36 + RTC at 0x68)")
			return &X728Driver{i2cBus: i2cBus, gpioChip: gpioChip, gpioBase: detectGPIOBase()}
		}
		log.Printf("PowerMonitor: detected Geekworm X1202 (MAX17040 at 0x36, no RTC)")
		return &X1202Driver{i2cBus: i2cBus, gpioChip: gpioChip}
	}

	log.Printf("PowerMonitor: no UPS detected on I2C bus %s", i2cBus)
	return nil
}

// ============================================================================
// Pi Model & GPIO Detection
// ============================================================================

// detectPiVersion reads /sys/firmware/devicetree/base/model to determine
// the Raspberry Pi version. Returns 5 for Pi 5, 4 for Pi 4, etc.
// Returns 0 if detection fails.
func detectPiVersion() int {
	data, err := os.ReadFile("/sys/firmware/devicetree/base/model")
	if err != nil {
		return 0
	}
	model := strings.TrimRight(string(data), "\x00\n")
	if strings.Contains(model, "Raspberry Pi 5") {
		return 5
	}
	if strings.Contains(model, "Raspberry Pi 4") {
		return 4
	}
	if strings.Contains(model, "Raspberry Pi 3") {
		return 3
	}
	if strings.Contains(model, "Raspberry Pi 2") {
		return 2
	}
	if strings.Contains(model, "Raspberry Pi Zero 2") {
		return 2 // Zero 2 W uses same SoC as Pi 3 but we treat as "2" tier
	}
	if strings.Contains(model, "Raspberry Pi Zero") {
		return 1
	}
	return 0
}

// detectGPIOChip returns the appropriate GPIO chip for the detected Pi model.
// Pi 5 uses gpiochip4, Pi 0-4 use gpiochip0.
// Can be overridden via HAL_GPIO_CHIP env var.
func detectGPIOChip() string {
	if chip := os.Getenv("HAL_GPIO_CHIP"); chip != "" {
		return chip
	}
	if detectPiVersion() >= 5 {
		return "gpiochip4"
	}
	return "gpiochip0"
}

// ============================================================================
// I2C Helpers
// ============================================================================

// probeI2CDevice checks if a device responds on the given I2C bus and address.
// Uses i2cget with a dummy read — exit code 0 means device ACKed.
func probeI2CDevice(ctx context.Context, bus, addr string) bool {
	_, err := execWithTimeout(ctx, "i2cget", "-y", bus, addr)
	return err == nil
}

// readI2CWord reads a 16-bit big-endian word from an I2C register.
// i2cget returns the value in little-endian format (bytes swapped), so we swap.
func readI2CWord(ctx context.Context, bus, addr, reg string) (uint16, error) {
	output, err := execWithTimeout(ctx, "i2cget", "-y", bus, addr, reg, "w")
	if err != nil {
		return 0, fmt.Errorf("i2c read %s/%s: %w", addr, reg, err)
	}
	val, err := parseHexInt(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parse i2c value %q: %w", output, err)
	}
	// i2cget -y returns bytes in wire order for "w" mode, swap for big-endian
	swapped := uint16(((val & 0xFF) << 8) | ((val >> 8) & 0xFF))
	return swapped, nil
}

// readI2CByte reads a single byte from an I2C register.
func readI2CByte(ctx context.Context, bus, addr, reg string) (byte, error) {
	output, err := execWithTimeout(ctx, "i2cget", "-y", bus, addr, reg)
	if err != nil {
		return 0, fmt.Errorf("i2c read %s/%s: %w", addr, reg, err)
	}
	val, err := parseHexInt(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parse i2c value %q: %w", output, err)
	}
	return byte(val), nil
}

// parseHexInt parses a hex string like "0x1A" or "0x001A" to an int64.
func parseHexInt(s string) (int64, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return 0, fmt.Errorf("empty hex string")
	}
	var val int64
	for _, c := range s {
		val <<= 4
		switch {
		case c >= '0' && c <= '9':
			val |= int64(c - '0')
		case c >= 'a' && c <= 'f':
			val |= int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			val |= int64(c-'A') + 10
		default:
			return 0, fmt.Errorf("invalid hex char %q", c)
		}
	}
	return val, nil
}

// getEnvOrDefault returns the value of an environment variable or a default.
func getEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
