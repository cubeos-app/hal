package handlers

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// ============================================================================
// I2C Bus Recovery
// ============================================================================
//
// I2C controllers can enter a stuck state where all transactions time out.
// Recovery is achieved by unbinding and rebinding the platform driver via sysfs.
//
// Pi 5 (RP1 DesignWare controller):
//   echo "1f00074000.i2c" > /sys/bus/platform/drivers/i2c_designware/unbind
//   echo "1f00074000.i2c" > /sys/bus/platform/drivers/i2c_designware/bind
//
// Pi 4 (BCM2835 controller):
//   echo "fe804000.i2c" > /sys/bus/platform/drivers/bcm2835-i2c/unbind
//   echo "fe804000.i2c" > /sys/bus/platform/drivers/bcm2835-i2c/bind
//
// Pi model is auto-detected via /sys/firmware/devicetree/base/model.
// This module tracks consecutive I2C read errors and triggers the recovery
// automatically when the threshold is reached, with rate limiting to prevent
// reset storms.
//
// B81 FIX: In containerized environments, the sysfs driver paths may not exist.
// On first use, we check if the unbind/bind paths are accessible. If not, we
// log a single warning and permanently disable recovery for this session to
// avoid spamming logs every 5 minutes.

const (
	// Pi 5 (RP1 DesignWare I2C controller)
	pi5I2CDevicePath = "1f00074000.i2c"
	pi5I2CDriverPath = "/sys/bus/platform/drivers/i2c_designware"

	// Pi 4 and earlier (Broadcom BCM2835 I2C controller)
	pi4I2CDevicePath = "fe804000.i2c"
	pi4I2CDriverPath = "/sys/bus/platform/drivers/bcm2835-i2c"

	defaultRecoveryThreshold   = 3               // consecutive errors before recovery
	defaultRecoveryMinInterval = 5 * time.Minute // minimum time between recovery attempts
	defaultRecoverySettleTime  = 2 * time.Second // wait after rebind before retrying
)

// I2CRecovery handles automatic recovery of stuck DesignWare I2C controllers.
type I2CRecovery struct {
	mu                sync.Mutex
	devicePath        string        // e.g. "1f00074000.i2c"
	driverPath        string        // e.g. "/sys/bus/platform/drivers/i2c_designware"
	threshold         int           // consecutive errors before attempting recovery
	minInterval       time.Duration // minimum time between recovery attempts
	settleTime        time.Duration // wait after rebind
	consecutiveErrors int           // current consecutive error count
	lastAttemptAt     time.Time     // when we last attempted recovery
	totalRecoveries   int           // lifetime recovery count
	lastRecoveryOK    bool          // whether last recovery succeeded
	recoveryAvailable bool          // false if sysfs paths don't exist (B81 fix)
	unavailableLogged bool          // true after we've logged the "not available" warning once
}

// NewI2CRecovery creates an I2CRecovery with configuration from environment variables.
//
// Auto-detects Pi version for correct default paths:
//   - Pi 5: i2c_designware at 1f00074000.i2c
//   - Pi 4 and earlier: bcm2835-i2c at fe804000.i2c
//
// Environment variables (override autodetection):
//   - HAL_I2C_DEVICE:             platform device path
//   - HAL_I2C_DRIVER_PATH:        sysfs driver path
//   - HAL_I2C_RECOVERY_THRESHOLD: consecutive errors before recovery (default: 3)
func NewI2CRecovery() *I2CRecovery {
	// Auto-detect defaults based on Pi model
	defaultDevicePath := pi5I2CDevicePath
	defaultDriverPath := pi5I2CDriverPath
	piVer := detectPiVersion()
	if piVer > 0 && piVer < 5 {
		defaultDevicePath = pi4I2CDevicePath
		defaultDriverPath = pi4I2CDriverPath
	}
	log.Printf("I2CRecovery: Pi %d detected, device=%s driver=%s", piVer, defaultDevicePath, defaultDriverPath)

	devicePath := getEnvOrDefault("HAL_I2C_DEVICE", defaultDevicePath)
	driverPath := getEnvOrDefault("HAL_I2C_DRIVER_PATH", defaultDriverPath)

	threshold := defaultRecoveryThreshold
	if v := os.Getenv("HAL_I2C_RECOVERY_THRESHOLD"); v != "" {
		if n := parseIntOrDefault(v, defaultRecoveryThreshold); n > 0 && n <= 20 {
			threshold = n
		}
	}

	// B81 fix: Check if the sysfs driver path actually exists in this environment.
	// In containers without host sysfs mounted, these paths won't be available.
	available := checkRecoveryPathsExist(driverPath)
	if available {
		log.Printf("I2CRecovery: sysfs recovery paths verified (available)")
	} else {
		log.Printf("I2CRecovery: sysfs recovery paths not accessible in this environment — I2C bus recovery disabled for this session")
	}

	return &I2CRecovery{
		devicePath:        devicePath,
		driverPath:        driverPath,
		threshold:         threshold,
		minInterval:       defaultRecoveryMinInterval,
		settleTime:        defaultRecoverySettleTime,
		recoveryAvailable: available,
	}
}

// checkRecoveryPathsExist verifies that the sysfs unbind/bind paths exist.
// Returns false if the paths are not accessible (e.g., in a container).
func checkRecoveryPathsExist(driverPath string) bool {
	unbindPath := driverPath + "/unbind"
	bindPath := driverPath + "/bind"

	if _, err := os.Stat(unbindPath); err != nil {
		return false
	}
	if _, err := os.Stat(bindPath); err != nil {
		return false
	}
	return true
}

// RecordError increments the consecutive error counter.
// Returns true if recovery should be attempted (threshold reached, rate limit allows,
// and recovery paths are available).
func (r *I2CRecovery) RecordError() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.consecutiveErrors++

	// B81 fix: If recovery isn't available, never trigger it.
	// Log a single warning the first time we would have tried recovery.
	if !r.recoveryAvailable {
		if r.consecutiveErrors >= r.threshold && !r.unavailableLogged {
			r.unavailableLogged = true
			log.Printf("I2CRecovery: recovery would be triggered (%d consecutive errors) but sysfs paths are not available in this environment — suppressing further attempts", r.consecutiveErrors)
		}
		return false
	}

	if r.consecutiveErrors < r.threshold {
		return false
	}

	// Check rate limit
	if !r.lastAttemptAt.IsZero() && time.Since(r.lastAttemptAt) < r.minInterval {
		return false
	}

	return true
}

// RecordSuccess resets the consecutive error counter.
func (r *I2CRecovery) RecordSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecutiveErrors = 0
}

// AttemptRecovery performs the I2C controller unbind/bind sequence.
// Returns nil on success. Thread-safe and rate-limited.
// Returns an error immediately if recovery paths are not available (B81 fix).
func (r *I2CRecovery) AttemptRecovery() error {
	r.mu.Lock()

	// B81 fix: bail out immediately if sysfs paths don't exist
	if !r.recoveryAvailable {
		r.mu.Unlock()
		return fmt.Errorf("I2C recovery not available (sysfs paths not accessible)")
	}

	// Double-check rate limit under lock
	if !r.lastAttemptAt.IsZero() && time.Since(r.lastAttemptAt) < r.minInterval {
		r.mu.Unlock()
		return fmt.Errorf("recovery rate-limited (last attempt %s ago, minimum interval %s)",
			time.Since(r.lastAttemptAt).Round(time.Second), r.minInterval)
	}
	r.lastAttemptAt = time.Now()
	r.consecutiveErrors = 0 // Reset counter regardless of outcome
	device := r.devicePath
	driver := r.driverPath
	settle := r.settleTime
	r.mu.Unlock()

	unbindPath := driver + "/unbind"
	bindPath := driver + "/bind"

	log.Printf("I2CRecovery: attempting controller reset for %s", device)

	// Step 1: Unbind the device
	if err := os.WriteFile(unbindPath, []byte(device), 0200); err != nil {
		r.mu.Lock()
		r.lastRecoveryOK = false
		r.mu.Unlock()
		return fmt.Errorf("unbind %s: %w", device, err)
	}
	log.Printf("I2CRecovery: unbound %s", device)

	// Wait for controller to release
	time.Sleep(1 * time.Second)

	// Step 2: Rebind the device
	if err := os.WriteFile(bindPath, []byte(device), 0200); err != nil {
		r.mu.Lock()
		r.lastRecoveryOK = false
		r.mu.Unlock()
		return fmt.Errorf("bind %s: %w", device, err)
	}
	log.Printf("I2CRecovery: rebound %s", device)

	// Wait for controller to settle before any I2C access
	time.Sleep(settle)

	r.mu.Lock()
	r.totalRecoveries++
	r.lastRecoveryOK = true
	r.mu.Unlock()

	log.Printf("I2CRecovery: controller reset complete (total recoveries: %d)", r.totalRecoveries)
	return nil
}

// Stats returns recovery statistics for inclusion in status responses.
func (r *I2CRecovery) Stats() I2CRecoveryStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	stats := I2CRecoveryStats{
		ConsecutiveErrors: r.consecutiveErrors,
		TotalRecoveries:   r.totalRecoveries,
		DevicePath:        r.devicePath,
		RecoveryAvailable: r.recoveryAvailable,
	}

	if !r.lastAttemptAt.IsZero() {
		t := r.lastAttemptAt.UTC().Format(time.RFC3339)
		stats.LastAttemptAt = &t
		stats.LastRecoveryOK = r.lastRecoveryOK
	}

	return stats
}

// I2CRecoveryStats is the JSON-serializable recovery status.
type I2CRecoveryStats struct {
	ConsecutiveErrors int     `json:"consecutive_errors"`
	TotalRecoveries   int     `json:"total_recoveries"`
	DevicePath        string  `json:"device_path"`
	LastAttemptAt     *string `json:"last_attempt_at"`
	LastRecoveryOK    bool    `json:"last_recovery_ok"`
	RecoveryAvailable bool    `json:"recovery_available"` // B81: false if sysfs paths not in container
}

// parseIntOrDefault parses a string as int, returning defaultVal on failure.
func parseIntOrDefault(s string, defaultVal int) int {
	var val int
	for _, c := range s {
		if c < '0' || c > '9' {
			return defaultVal
		}
		val = val*10 + int(c-'0')
	}
	return val
}
