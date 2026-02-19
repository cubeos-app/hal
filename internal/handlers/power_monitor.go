package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// ============================================================================
// Power Monitor
// ============================================================================

const (
	defaultMonitorInterval      = 30 * time.Second
	defaultLowBatteryThreshold  = 15.0
	defaultCritBatteryThreshold = 5.0
	defaultShutdownDelay        = 30 * time.Second
	maxEvents                   = 50
)

// PowerEvent represents a notable power state change.
type PowerEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"` // started, stopped, ac_lost, ac_restored, low_battery, critical_battery, shutdown_pending, shutdown_cancelled
	Message   string `json:"message"`
}

// MonitorStatus is the response payload for GET /power/monitor/status.
type MonitorStatus struct {
	Running         bool              `json:"running"`
	DetectedDevice  string            `json:"detected_device"`
	IntervalSeconds int               `json:"interval_seconds"`
	LastReading     *BatteryReading   `json:"last_reading"`
	ACPowerLost     bool              `json:"ac_power_lost"`
	ShutdownPending bool              `json:"shutdown_pending"`
	ShutdownAt      *string           `json:"shutdown_at"`
	I2CRecovery     *I2CRecoveryStats `json:"i2c_recovery,omitempty"`
	Events          []PowerEvent      `json:"events"`
}

// PowerMonitor runs a background goroutine that periodically reads UPS status
// and takes action on power events (AC loss, low battery, critical shutdown).
type PowerMonitor struct {
	mu              sync.Mutex
	running         bool
	cancel          context.CancelFunc
	interval        time.Duration
	lowThreshold    float64
	critThreshold   float64
	shutdownDelay   time.Duration
	lastReading     *BatteryReading
	acPowerLost     bool
	shutdownPending bool
	shutdownAt      *time.Time
	shutdownCancel  context.CancelFunc
	events          []PowerEvent
	driver          UPSDriver
	i2cRecovery     *I2CRecovery
}

// NewPowerMonitor creates a PowerMonitor with configuration from environment variables.
func NewPowerMonitor() *PowerMonitor {
	interval := defaultMonitorInterval
	if v := os.Getenv("HAL_POWER_MONITOR_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 5*time.Second {
			interval = d
		}
	}

	lowThreshold := defaultLowBatteryThreshold
	if v := os.Getenv("HAL_UPS_LOW_BATTERY_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 100 {
			lowThreshold = f
		}
	}

	critThreshold := defaultCritBatteryThreshold
	if v := os.Getenv("HAL_UPS_CRITICAL_BATTERY_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 100 {
			critThreshold = f
		}
	}

	shutdownDelay := defaultShutdownDelay
	if v := os.Getenv("HAL_UPS_SHUTDOWN_DELAY"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			shutdownDelay = time.Duration(secs) * time.Second
		}
	}

	return &PowerMonitor{
		interval:      interval,
		lowThreshold:  lowThreshold,
		critThreshold: critThreshold,
		shutdownDelay: shutdownDelay,
		events:        make([]PowerEvent, 0, maxEvents),
		i2cRecovery:   NewI2CRecovery(),
	}
}

// Start begins background power monitoring. Auto-detects the UPS device
// (or uses HAL_UPS_MODEL override), calls driver.OnBoot(), and starts polling.
// Returns an error message if already running. Thread-safe.
func (pm *PowerMonitor) Start() (string, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.running {
		return "already running", nil
	}

	// Detect UPS device
	driver := DetectUPS()
	pm.driver = driver

	deviceName := "none"
	if driver != nil {
		deviceName = driver.Name()

		// Run boot-time initialization
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer bootCancel()
		if err := driver.OnBoot(bootCtx); err != nil {
			log.Printf("PowerMonitor: OnBoot error for %s: %v", deviceName, err)
			// Non-fatal — continue starting the monitor
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	pm.cancel = cancel
	pm.running = true

	pm.addEventLocked("started", fmt.Sprintf("power monitoring started (%s)", deviceName))
	log.Printf("PowerMonitor: started with %s, interval=%s, low=%g%%, crit=%g%%",
		deviceName, pm.interval, pm.lowThreshold, pm.critThreshold)

	go pm.pollLoop(ctx)

	return "power monitoring started", nil
}

// Stop halts the background monitoring goroutine and cancels any pending shutdown.
// Thread-safe.
func (pm *PowerMonitor) Stop() (string, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.running {
		return "not running", nil
	}

	if pm.cancel != nil {
		pm.cancel()
		pm.cancel = nil
	}

	// Cancel any pending shutdown countdown
	if pm.shutdownCancel != nil {
		pm.shutdownCancel()
		pm.shutdownCancel = nil
		pm.shutdownPending = false
		pm.shutdownAt = nil
		pm.addEventLocked("shutdown_cancelled", "pending shutdown cancelled (monitor stopped)")
	}

	pm.running = false
	pm.driver = nil
	pm.lastReading = nil
	pm.addEventLocked("stopped", "power monitoring stopped")
	log.Printf("PowerMonitor: stopped")

	return "power monitoring stopped", nil
}

// Status returns the current monitor status. Thread-safe.
func (pm *PowerMonitor) Status() MonitorStatus {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	status := MonitorStatus{
		Running:         pm.running,
		IntervalSeconds: int(pm.interval.Seconds()),
		LastReading:     pm.lastReading,
		ACPowerLost:     pm.acPowerLost,
		ShutdownPending: pm.shutdownPending,
		Events:          make([]PowerEvent, len(pm.events)),
	}

	copy(status.Events, pm.events)

	if pm.driver != nil {
		status.DetectedDevice = pm.driver.Name()
	} else {
		status.DetectedDevice = "none"
	}

	if pm.shutdownAt != nil {
		t := pm.shutdownAt.UTC().Format(time.RFC3339)
		status.ShutdownAt = &t
	}

	if pm.i2cRecovery != nil {
		stats := pm.i2cRecovery.Stats()
		status.I2CRecovery = &stats
	}

	return status
}

// Driver returns the detected UPS driver (or nil if none detected).
// Used by the system Shutdown handler to call InitiateShutdown before poweroff.
func (pm *PowerMonitor) Driver() UPSDriver {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.driver
}

// LastReading returns the most recent cached battery reading, or nil.
// Thread-safe.
func (pm *PowerMonitor) LastReading() *BatteryReading {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.lastReading
}

// Shutdown gracefully stops the monitor. Called during HAL shutdown.
func (pm *PowerMonitor) Shutdown() {
	pm.mu.Lock()
	if !pm.running {
		pm.mu.Unlock()
		return
	}
	pm.mu.Unlock()

	pm.Stop()
}

// ============================================================================
// UPS Configuration Handler
// ============================================================================

// ConfigureUPSRequest is the request body for POST /power/ups/configure.
// @Description UPS model configuration request
type ConfigureUPSRequest struct {
	Model string `json:"model" example:"x1202"` // "x1202", "x728", "pisugar3", "none"
}

// ConfigureUPSResponse is the response for POST /power/ups/configure.
// @Description UPS configuration result
type ConfigureUPSResponse struct {
	Status string `json:"status" example:"ok"`
	Model  string `json:"model" example:"x1202"`
	Driver string `json:"driver" example:"Geekworm X1202"`
}

// validUPSModels is the set of accepted UPS model identifiers.
var validUPSModels = map[string]bool{
	"none":     true,
	"x1202":    true,
	"x728":     true,
	"pisugar3": true,
}

// ConfigureUPS applies the user's confirmed UPS model selection.
// It stops any running power monitor, sets the HAL_UPS_MODEL env var,
// and (re)starts the power monitor with the correct driver.
// This endpoint is called by the API after the user confirms their selection.
//
// @Summary Configure UPS model
// @Description Apply user-confirmed UPS model selection. Stops existing monitor, loads correct driver, restarts monitoring.
// @Tags Power
// @Accept json
// @Produce json
// @Param request body ConfigureUPSRequest true "UPS model selection"
// @Success 200 {object} ConfigureUPSResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /power/ups/configure [post]
func (h *HALHandler) ConfigureUPS(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB
	var req ConfigureUPSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	model := req.Model
	if !validUPSModels[model] {
		errorResponse(w, http.StatusBadRequest, fmt.Sprintf("invalid UPS model %q, must be one of: none, x1202, x728, pisugar3", model))
		return
	}

	log.Printf("ConfigureUPS: received configuration request for model=%q", model)

	// Step 1: Stop power monitor if running
	if msg, err := h.powerMonitor.Stop(); err != nil {
		log.Printf("ConfigureUPS: error stopping monitor: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to stop power monitor: "+err.Error())
		return
	} else {
		log.Printf("ConfigureUPS: stop result: %s", msg)
	}

	// Step 2: If model is "none", we're done — monitor stays stopped
	if model == "none" {
		// Clear the env var so DetectUPS won't pick up a stale value
		os.Setenv("HAL_UPS_MODEL", "")
		log.Printf("ConfigureUPS: UPS set to none, power monitor disabled")
		jsonResponse(w, http.StatusOK, ConfigureUPSResponse{
			Status: "ok",
			Model:  "none",
			Driver: "none",
		})
		return
	}

	// Step 3: Set HAL_UPS_MODEL env var so DetectUPS() picks it up
	os.Setenv("HAL_UPS_MODEL", model)
	log.Printf("ConfigureUPS: HAL_UPS_MODEL set to %q", model)

	// Step 4: Start power monitor (will use forced model via env var)
	msg, err := h.powerMonitor.Start()
	if err != nil {
		log.Printf("ConfigureUPS: error starting monitor: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to start power monitor: "+err.Error())
		return
	}
	log.Printf("ConfigureUPS: start result: %s", msg)

	// Get the loaded driver name
	driverName := "none"
	if driver := h.powerMonitor.Driver(); driver != nil {
		driverName = driver.Name()
	}

	jsonResponse(w, http.StatusOK, ConfigureUPSResponse{
		Status: "ok",
		Model:  model,
		Driver: driverName,
	})
}

// ============================================================================
// Polling Loop
// ============================================================================

func (pm *PowerMonitor) pollLoop(ctx context.Context) {
	// Do an immediate first poll
	pm.poll(ctx)

	ticker := time.NewTicker(pm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("PowerMonitor: poll loop exiting")
			return
		case <-ticker.C:
			pm.poll(ctx)
		}
	}
}

func (pm *PowerMonitor) poll(ctx context.Context) {
	pm.mu.Lock()
	driver := pm.driver
	pm.mu.Unlock()

	if driver == nil {
		// No driver loaded — do NOT attempt re-detection.
		// The user must explicitly configure a model via POST /power/ups/configure.
		return
	}

	// Read battery status
	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()

	reading, err := driver.ReadStatus(readCtx)
	if err != nil {
		log.Printf("PowerMonitor: read error: %v", err)

		// Track consecutive errors and attempt I2C recovery if threshold reached
		if pm.i2cRecovery != nil && pm.i2cRecovery.RecordError() {
			pm.mu.Lock()
			pm.addEventLocked("i2c_recovery", fmt.Sprintf("attempting I2C bus recovery after %d consecutive errors", pm.i2cRecovery.Stats().ConsecutiveErrors))
			pm.mu.Unlock()

			if recoveryErr := pm.i2cRecovery.AttemptRecovery(); recoveryErr != nil {
				log.Printf("PowerMonitor: I2C recovery failed: %v", recoveryErr)
				pm.mu.Lock()
				pm.addEventLocked("i2c_recovery_failed", fmt.Sprintf("I2C recovery failed: %v", recoveryErr))
				pm.mu.Unlock()
			} else {
				log.Printf("PowerMonitor: I2C recovery succeeded, retrying read")
				pm.mu.Lock()
				pm.addEventLocked("i2c_recovery_ok", "I2C bus recovery succeeded")
				pm.mu.Unlock()

				// Retry the read immediately after recovery
				retryCtx, retryCancel := context.WithTimeout(ctx, 10*time.Second)
				defer retryCancel()
				reading, err = driver.ReadStatus(retryCtx)
				if err != nil {
					log.Printf("PowerMonitor: read still failing after recovery: %v", err)
					return
				}
				// Fall through to process the successful reading below
			}
		}

		if err != nil {
			return
		}
	}

	// Successful read — reset consecutive error counter
	if pm.i2cRecovery != nil {
		pm.i2cRecovery.RecordSuccess()
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	prevReading := pm.lastReading
	pm.lastReading = reading

	if !reading.Available {
		return
	}

	// Detect AC power state transitions
	if prevReading != nil && prevReading.Available {
		if prevReading.ACPresent && !reading.ACPresent {
			// AC power lost
			pm.acPowerLost = true
			pm.addEventLocked("ac_lost", fmt.Sprintf("AC power lost, running on battery (%.1f%%, %.2fV)", reading.Percentage, reading.Voltage))
			log.Printf("PowerMonitor: AC power lost — battery at %.1f%% (%.2fV)", reading.Percentage, reading.Voltage)
		} else if !prevReading.ACPresent && reading.ACPresent {
			// AC power restored
			pm.acPowerLost = false
			pm.addEventLocked("ac_restored", fmt.Sprintf("AC power restored (%.1f%%)", reading.Percentage))
			log.Printf("PowerMonitor: AC power restored — battery at %.1f%%", reading.Percentage)

			// Cancel any pending shutdown
			if pm.shutdownPending && pm.shutdownCancel != nil {
				pm.shutdownCancel()
				pm.shutdownCancel = nil
				pm.shutdownPending = false
				pm.shutdownAt = nil
				pm.addEventLocked("shutdown_cancelled", "pending shutdown cancelled (AC power restored)")
				log.Printf("PowerMonitor: pending shutdown cancelled — AC power restored")
			}
		}
	} else if prevReading == nil {
		// First reading — initialize AC state
		pm.acPowerLost = !reading.ACPresent
	}

	// Skip battery threshold checks if AC is present
	if reading.ACPresent {
		return
	}

	// Low battery warning (event deduplication — only on transition)
	if reading.Percentage < pm.lowThreshold {
		if prevReading == nil || prevReading.Percentage >= pm.lowThreshold {
			pm.addEventLocked("low_battery", fmt.Sprintf("battery low: %.1f%% (threshold: %.0f%%)", reading.Percentage, pm.lowThreshold))
			log.Printf("PowerMonitor: LOW BATTERY — %.1f%% (threshold: %.0f%%)", reading.Percentage, pm.lowThreshold)
		}
	}

	// Critical battery — initiate shutdown countdown
	if reading.Percentage < pm.critThreshold && !pm.shutdownPending {
		pm.shutdownPending = true
		shutdownTime := time.Now().Add(pm.shutdownDelay)
		pm.shutdownAt = &shutdownTime
		pm.addEventLocked("critical_battery", fmt.Sprintf("CRITICAL battery: %.1f%% — shutdown in %s", reading.Percentage, pm.shutdownDelay))
		log.Printf("PowerMonitor: CRITICAL BATTERY — %.1f%% — initiating shutdown in %s", reading.Percentage, pm.shutdownDelay)

		// Launch shutdown countdown goroutine
		shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
		pm.shutdownCancel = shutdownCancel

		go pm.shutdownCountdown(shutdownCtx, driver)
	}
}

// ============================================================================
// Shutdown Countdown
// ============================================================================

func (pm *PowerMonitor) shutdownCountdown(ctx context.Context, driver UPSDriver) {
	pm.mu.Lock()
	delay := pm.shutdownDelay
	pm.mu.Unlock()

	select {
	case <-ctx.Done():
		// Shutdown was cancelled (AC restored or monitor stopped)
		return
	case <-time.After(delay):
		// Countdown expired — execute shutdown
	}

	// Use the powerActionInProgress guard to prevent concurrent shutdown/reboot
	if !powerActionInProgress.CompareAndSwap(false, true) {
		log.Printf("PowerMonitor: shutdown aborted — another power action in progress")
		pm.mu.Lock()
		pm.shutdownPending = false
		pm.shutdownAt = nil
		pm.addEventLocked("shutdown_cancelled", "shutdown aborted (another power action in progress)")
		pm.mu.Unlock()
		return
	}

	log.Printf("PowerMonitor: executing critical battery shutdown")

	pm.mu.Lock()
	pm.addEventLocked("shutdown_executing", "critical battery shutdown executing")
	pm.mu.Unlock()

	// Device-specific shutdown sequence
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := driver.InitiateShutdown(shutdownCtx); err != nil {
		log.Printf("PowerMonitor: driver shutdown error: %v", err)
		// Continue with system shutdown anyway
	}

	// Execute system poweroff via nsenter (Alpine container has no systemctl)
	log.Printf("PowerMonitor: executing systemctl poweroff via nsenter")
	if _, err := execWithTimeout(context.Background(), "nsenter", "-t", "1", "-m", "--", "systemctl", "poweroff"); err != nil {
		log.Printf("PowerMonitor: nsenter poweroff failed: %v, trying poweroff -f", err)
		if _, err2 := execWithTimeout(context.Background(), "poweroff", "-f"); err2 != nil {
			log.Printf("PowerMonitor: poweroff -f also failed: %v", err2)
		}
		powerActionInProgress.Store(false)
	}
}

// ============================================================================
// Event Management
// ============================================================================

// addEventLocked appends an event to the ring buffer. Caller must hold pm.mu.
func (pm *PowerMonitor) addEventLocked(eventType, message string) {
	event := PowerEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      eventType,
		Message:   message,
	}

	if len(pm.events) >= maxEvents {
		// Ring buffer: drop oldest event
		copy(pm.events, pm.events[1:])
		pm.events[len(pm.events)-1] = event
	} else {
		pm.events = append(pm.events, event)
	}
}

// ============================================================================
// Autostart Helper
// ============================================================================

// ShouldAutostart returns true if HAL_POWER_MONITOR_AUTOSTART is not "false".
func ShouldAutostart() bool {
	v := os.Getenv("HAL_POWER_MONITOR_AUTOSTART")
	return v != "false" && v != "0" && v != "no"
}
