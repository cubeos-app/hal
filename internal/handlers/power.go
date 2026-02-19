package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Constants - UPS / Battery
// ============================================================================

const (
	DefaultI2CBus      = 1
	MAX17040Address    = 0x36
	MAX17040RegVCELL   = 0x02
	MAX17040RegSOC     = 0x04
	MAX17040RegMODE    = 0x06
	MAX17040RegVERSION = 0x08

	GPIOPowerLoss  = 6  // Input: polarity depends on UPS model — NEVER read directly
	GPIOChargeCtrl = 16 // Output: LOW = charging, HIGH = not charging

	LowBatteryThreshold      = 15.0
	CriticalBatteryThreshold = 5.0
)

// ============================================================================
// Power Types
// ============================================================================

// BatteryStatus represents current battery state.
// @Description Battery status information from UPS HAT
type BatteryStatus struct {
	Available           bool    `json:"available" example:"true"`
	Voltage             float64 `json:"voltage" example:"4.12"`
	VoltageRaw          uint16  `json:"voltage_raw,omitempty"`
	Percentage          float64 `json:"percentage" example:"85.5"`
	PercentageEstimated float64 `json:"percentage_estimated,omitempty" example:"87.0"`
	PercentageRaw       uint16  `json:"percentage_raw,omitempty"`
	IsCharging          bool    `json:"is_charging" example:"true"`
	ChargingEnabled     bool    `json:"charging_enabled" example:"true"`
	ACPresent           bool    `json:"ac_present" example:"true"`
	IsLow               bool    `json:"is_low" example:"false"`
	IsCritical          bool    `json:"is_critical" example:"false"`
	LastUpdated         string  `json:"last_updated"`
}

// UPSInfo contains UPS hardware information.
// @Description UPS hardware detection information
type UPSInfo struct {
	Model       string `json:"model" example:"Geekworm X1202"`
	Detected    bool   `json:"detected" example:"true"`
	I2CAddress  string `json:"i2c_address" example:"0x36"`
	I2CBus      int    `json:"i2c_bus" example:"1"`
	FuelGauge   string `json:"fuel_gauge" example:"MAX17040"`
	GPIOChip    string `json:"gpio_chip" example:"gpiochip4"`
	PiVersion   int    `json:"pi_version" example:"5"`
	ChipVersion uint16 `json:"chip_version,omitempty"`
}

// PowerStatus combines all power-related information.
// @Description Complete power status including UPS, battery, uptime, RTC, and watchdog
type PowerStatus struct {
	UPS         UPSInfo       `json:"ups"`
	Battery     BatteryStatus `json:"battery"`
	Uptime      UptimeInfo    `json:"uptime"`
	RTC         RTCStatus     `json:"rtc"`
	Watchdog    WatchdogInfo  `json:"watchdog"`
	LastUpdated string        `json:"last_updated"`
}

// UptimeInfo contains system uptime information.
// @Description System uptime information
type UptimeInfo struct {
	Seconds     float64   `json:"seconds" example:"593949.26"`
	Formatted   string    `json:"formatted" example:"6d 21h 5m 49s"`
	BootTime    string    `json:"boot_time" example:"2026-01-27T19:00:00Z"`
	LoadAverage []float64 `json:"load_average"`
}

// RTCStatus contains RTC information.
// @Description Real-Time Clock status
type RTCStatus struct {
	Available    bool   `json:"available" example:"true"`
	Time         string `json:"time" example:"2026-02-03T16:15:30Z"`
	Synchronized bool   `json:"synchronized" example:"true"`
	BatteryOK    bool   `json:"battery_ok" example:"true"`
	Device       string `json:"device,omitempty" example:"/dev/rtc0"`
}

// WatchdogInfo contains watchdog information.
// @Description Hardware watchdog status
type WatchdogInfo struct {
	Device  string `json:"device" example:"/dev/watchdog"`
	Enabled bool   `json:"enabled" example:"true"`
	Timeout int    `json:"timeout" example:"15"`
}

// ChargingRequest represents charging control request.
// @Description Charging control parameters
type ChargingRequest struct {
	Enabled bool `json:"enabled" example:"true"`
}

// WakeAlarmRequest represents wake alarm request.
// @Description Wake alarm parameters
type WakeAlarmRequest struct {
	Time string `json:"time" example:"2026-02-04T08:00:00Z"`
}

// ============================================================================
// Power Status Handlers
// ============================================================================

// GetPowerStatus returns complete power status.
// @Summary Get power status
// @Description Returns complete power status including UPS, battery, uptime, RTC, and watchdog
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} PowerStatus
// @Failure 500 {object} ErrorResponse
// @Router /power/status [get]
func (h *HALHandler) GetPowerStatus(w http.ResponseWriter, r *http.Request) {
	status := PowerStatus{
		UPS:         h.getUPSInfo(),
		Battery:     h.getBatteryStatus(),
		Uptime:      h.getUptimeInfo(),
		RTC:         h.getRTCStatus(),
		Watchdog:    h.getWatchdogInfo(),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	}

	jsonResponse(w, http.StatusOK, status)
}

// GetBatteryStatus returns battery status.
// @Summary Get battery status
// @Description Returns battery voltage, percentage, and charging status from configured UPS driver
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} BatteryStatus
// @Failure 500 {object} ErrorResponse
// @Router /power/battery [get]
func (h *HALHandler) GetBatteryStatus(w http.ResponseWriter, r *http.Request) {
	status := h.getBatteryStatus()
	jsonResponse(w, http.StatusOK, status)
}

// GetUPSInfo returns UPS hardware info.
// @Summary Get UPS info
// @Description Returns UPS hardware detection information from configured driver
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} UPSInfo
// @Failure 500 {object} ErrorResponse
// @Router /power/ups [get]
func (h *HALHandler) GetUPSInfo(w http.ResponseWriter, r *http.Request) {
	info := h.getUPSInfo()
	jsonResponse(w, http.StatusOK, info)
}

// SetChargingEnabled controls battery charging.
// Delegates to the active UPS driver. Returns 501 if no UPS is configured
// or if the configured UPS does not support charge control.
//
// @Summary Control charging
// @Description Enables or disables battery charging via GPIO (requires configured UPS with charge control support)
// @Tags Power
// @Accept json
// @Produce json
// @Param request body ChargingRequest true "Charging state"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Failure 501 {object} ErrorResponse "No UPS configured or charge control not supported"
// @Router /power/charging [post]
func (h *HALHandler) SetChargingEnabled(w http.ResponseWriter, r *http.Request) {
	// Check if a UPS driver is loaded
	driver := h.powerMonitor.Driver()
	if driver == nil {
		errorResponse(w, http.StatusNotImplemented, "no UPS configured — configure a UPS model first")
		return
	}

	// Check if this driver supports charge control
	if !driver.SupportsChargeControl() {
		errorResponse(w, http.StatusNotImplemented, fmt.Sprintf("%s does not support charge control", driver.Name()))
		return
	}

	r = limitBody(r, 1<<20) // 1MB
	var req ChargingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// GPIO16: LOW = charging, HIGH = not charging
	value := 0
	if !req.Enabled {
		value = 1
	}

	gpioChip := detectGPIOChip()
	if _, err := execWithTimeout(r.Context(), "gpioset", gpioChip, fmt.Sprintf("%d=%d", GPIOChargeCtrl, value)); err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("set charging state", err))
		return
	}

	state := "enabled"
	if !req.Enabled {
		state = "disabled"
	}
	successResponse(w, fmt.Sprintf("charging %s", state))
}

// QuickStartBattery performs fuel gauge quick-start.
// @Summary Quick-start battery
// @Description Performs MAX17040 fuel gauge quick-start for re-calibration
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /power/battery/quickstart [post]
func (h *HALHandler) QuickStartBattery(w http.ResponseWriter, r *http.Request) {
	if _, err := execWithTimeout(r.Context(), "i2cset", "-y", "1", "0x36", "0x06", "0x40", "0x00", "i"); err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("battery quick-start", err))
		return
	}

	successResponse(w, "battery fuel gauge quick-start initiated")
}

// StartPowerMonitor starts power monitoring.
// @Summary Start power monitor
// @Description Starts background power monitoring with UPS auto-detection
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /power/monitor/start [post]
func (h *HALHandler) StartPowerMonitor(w http.ResponseWriter, r *http.Request) {
	msg, err := h.powerMonitor.Start()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	successResponse(w, msg)
}

// StopPowerMonitor stops power monitoring.
// @Summary Stop power monitor
// @Description Stops background power monitoring and cancels pending shutdown
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /power/monitor/stop [post]
func (h *HALHandler) StopPowerMonitor(w http.ResponseWriter, r *http.Request) {
	msg, err := h.powerMonitor.Stop()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	successResponse(w, msg)
}

// GetMonitorStatus returns power monitor status.
// @Summary Get power monitor status
// @Description Returns monitor state, detected UPS device, last reading, and power events
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} MonitorStatus
// @Router /power/monitor/status [get]
func (h *HALHandler) GetMonitorStatus(w http.ResponseWriter, r *http.Request) {
	status := h.powerMonitor.Status()
	jsonResponse(w, http.StatusOK, status)
}

// ============================================================================
// Uptime Handler
// ============================================================================

// GetUptime returns system uptime.
// @Summary Get system uptime
// @Description Returns system uptime in seconds, formatted string, boot time, and load average
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} UptimeInfo
// @Failure 500 {object} ErrorResponse
// @Router /system/uptime [get]
func (h *HALHandler) GetUptime(w http.ResponseWriter, r *http.Request) {
	info := h.getUptimeInfo()
	jsonResponse(w, http.StatusOK, info)
}

// ============================================================================
// RTC Handlers
// ============================================================================

// GetRTCStatus returns RTC status.
// @Summary Get RTC status
// @Description Returns Real-Time Clock status and current time
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} RTCStatus
// @Failure 500 {object} ErrorResponse
// @Router /rtc/status [get]
func (h *HALHandler) GetRTCStatus(w http.ResponseWriter, r *http.Request) {
	status := h.getRTCStatus()
	jsonResponse(w, http.StatusOK, status)
}

// SetRTCTime sets RTC from system time.
// @Summary Set RTC time
// @Description Sets the RTC time from system clock (hwclock -w)
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Failure 501 {object} ErrorResponse "RTC not available"
// @Router /rtc/sync-to-rtc [post]
func (h *HALHandler) SetRTCTime(w http.ResponseWriter, r *http.Request) {
	if !h.isRTCAvailable() {
		errorResponse(w, http.StatusNotImplemented, "RTC not available on this device")
		return
	}

	if _, err := execWithTimeout(r.Context(), "hwclock", "-w"); err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("set RTC time", err))
		return
	}

	successResponse(w, "RTC time set from system clock")
}

// SyncTimeFromRTC syncs system time from RTC.
// @Summary Sync from RTC
// @Description Sets system time from RTC (hwclock -s)
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Failure 501 {object} ErrorResponse "RTC not available"
// @Router /rtc/sync-from-rtc [post]
func (h *HALHandler) SyncTimeFromRTC(w http.ResponseWriter, r *http.Request) {
	if !h.isRTCAvailable() {
		errorResponse(w, http.StatusNotImplemented, "RTC not available on this device")
		return
	}

	if _, err := execWithTimeout(r.Context(), "hwclock", "-s"); err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("sync time from RTC", err))
		return
	}

	successResponse(w, "system time synced from RTC")
}

// SetWakeAlarm sets RTC wake alarm.
// @Summary Set wake alarm
// @Description Sets RTC wake alarm for scheduled wake-up
// @Tags Power
// @Accept json
// @Produce json
// @Param request body WakeAlarmRequest true "Wake time"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Failure 501 {object} ErrorResponse "RTC wake alarm not supported"
// @Router /rtc/wakealarm [post]
func (h *HALHandler) SetWakeAlarm(w http.ResponseWriter, r *http.Request) {
	if !h.isRTCAvailable() {
		errorResponse(w, http.StatusNotImplemented, "RTC wake alarm not supported on this device")
		return
	}

	r = limitBody(r, 1<<20) // 1MB
	var req WakeAlarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	t, err := time.Parse(time.RFC3339, req.Time)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid time format, use RFC3339")
		return
	}

	alarmPath := "/sys/class/rtc/rtc0/wakealarm"

	// Check if wakealarm sysfs exists
	if _, err := os.Stat(alarmPath); err != nil {
		errorResponse(w, http.StatusNotImplemented, "RTC wake alarm not supported on this device")
		return
	}

	// Clear existing alarm first
	os.WriteFile(alarmPath, []byte("0"), 0644)

	// Set new alarm (unix timestamp)
	if err := os.WriteFile(alarmPath, []byte(strconv.FormatInt(t.Unix(), 10)), 0644); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to set wake alarm: "+err.Error())
		return
	}

	successResponse(w, fmt.Sprintf("wake alarm set for %s", req.Time))
}

// ClearWakeAlarm clears RTC wake alarm.
// @Summary Clear wake alarm
// @Description Clears the RTC wake alarm
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Failure 501 {object} ErrorResponse "RTC wake alarm not supported"
// @Router /rtc/wakealarm [delete]
func (h *HALHandler) ClearWakeAlarm(w http.ResponseWriter, r *http.Request) {
	alarmPath := "/sys/class/rtc/rtc0/wakealarm"

	if _, err := os.Stat(alarmPath); err != nil {
		errorResponse(w, http.StatusNotImplemented, "RTC wake alarm not supported on this device")
		return
	}

	if err := os.WriteFile(alarmPath, []byte("0"), 0644); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to clear wake alarm: "+err.Error())
		return
	}

	successResponse(w, "wake alarm cleared")
}

// ============================================================================
// Watchdog Handlers
// ============================================================================

// GetWatchdogStatus returns watchdog status.
// @Summary Get watchdog status
// @Description Returns hardware watchdog status and configuration
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} WatchdogInfo
// @Failure 500 {object} ErrorResponse
// @Router /watchdog/status [get]
func (h *HALHandler) GetWatchdogStatus(w http.ResponseWriter, r *http.Request) {
	info := h.getWatchdogInfo()
	jsonResponse(w, http.StatusOK, info)
}

// PetWatchdog pets the watchdog.
// @Summary Pet watchdog
// @Description Writes to watchdog device to prevent system reset
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Failure 501 {object} ErrorResponse "Watchdog device not available"
// @Router /watchdog/pet [post]
func (h *HALHandler) PetWatchdog(w http.ResponseWriter, r *http.Request) {
	f, err := os.OpenFile("/dev/watchdog", os.O_WRONLY, 0)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			errorResponse(w, http.StatusNotImplemented, "watchdog device not available on this system")
			return
		}
		errorResponse(w, http.StatusInternalServerError, "failed to open watchdog: "+err.Error())
		return
	}
	defer f.Close()

	if _, err := f.Write([]byte{0}); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to pet watchdog: "+err.Error())
		return
	}

	successResponse(w, "watchdog petted")
}

// EnableWatchdog enables the watchdog.
// @Summary Enable watchdog
// @Description Enables the hardware watchdog
// @Tags Power
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Router /watchdog/enable [post]
func (h *HALHandler) EnableWatchdog(w http.ResponseWriter, r *http.Request) {
	successResponse(w, "watchdog enabled (managed by systemd)")
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) getUptimeInfo() UptimeInfo {
	info := UptimeInfo{}

	data, err := os.ReadFile("/proc/uptime")
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 1 {
			info.Seconds, _ = strconv.ParseFloat(fields[0], 64)
		}
	}

	duration := time.Duration(info.Seconds) * time.Second
	days := int(duration.Hours() / 24)
	hours := int(duration.Hours()) % 24
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	if days > 0 {
		info.Formatted = fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	} else if hours > 0 {
		info.Formatted = fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	} else if minutes > 0 {
		info.Formatted = fmt.Sprintf("%dm %ds", minutes, seconds)
	} else {
		info.Formatted = fmt.Sprintf("%ds", seconds)
	}

	bootTime := time.Now().Add(-duration)
	info.BootTime = bootTime.UTC().Format(time.RFC3339)

	loadData, err := os.ReadFile("/proc/loadavg")
	if err == nil {
		fields := strings.Fields(string(loadData))
		if len(fields) >= 3 {
			info.LoadAverage = make([]float64, 3)
			info.LoadAverage[0], _ = strconv.ParseFloat(fields[0], 64)
			info.LoadAverage[1], _ = strconv.ParseFloat(fields[1], 64)
			info.LoadAverage[2], _ = strconv.ParseFloat(fields[2], 64)
		}
	}

	return info
}

// getBatteryStatus returns battery status by delegating to the active UPS driver.
// Priority order:
//  1. Use cached reading from PowerMonitor (if running and has a reading)
//  2. Do a one-shot read via the active driver (if driver loaded but no cached reading)
//  3. Return Available:false if no driver is loaded (no UPS configured)
//
// SAFETY: This function NEVER reads GPIO6 directly. All GPIO reads
// go through the driver's ReadStatus() method which applies the correct
// polarity for the detected UPS model.
func (h *HALHandler) getBatteryStatus() BatteryStatus {
	status := BatteryStatus{
		Available:   false,
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	}

	// Try cached reading from power monitor first
	if cached := h.powerMonitor.LastReading(); cached != nil && cached.Available {
		status.Available = true
		status.Voltage = cached.Voltage
		status.Percentage = cached.Percentage
		status.ACPresent = cached.ACPresent
		status.IsCharging = cached.IsCharging
		status.ChargingEnabled = cached.ChargingEnabled
		status.IsLow = cached.Percentage < LowBatteryThreshold
		status.IsCritical = cached.Percentage < CriticalBatteryThreshold
		status.LastUpdated = cached.Timestamp
		return status
	}

	// No cached reading — try one-shot read via driver
	driver := h.powerMonitor.Driver()
	if driver == nil {
		// No UPS configured — return available:false (safe default)
		return status
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reading, err := driver.ReadStatus(ctx)
	if err != nil {
		// I2C read failed — return available:false, not a crash
		return status
	}

	if reading != nil && reading.Available {
		status.Available = true
		status.Voltage = reading.Voltage
		status.Percentage = reading.Percentage
		status.ACPresent = reading.ACPresent
		status.IsCharging = reading.IsCharging
		status.ChargingEnabled = reading.ChargingEnabled
		status.IsLow = reading.Percentage < LowBatteryThreshold
		status.IsCritical = reading.Percentage < CriticalBatteryThreshold
		status.LastUpdated = reading.Timestamp
	}

	return status
}

// getUPSInfo returns UPS hardware information from the active driver.
// Returns dynamic info based on the currently loaded driver, not hardcoded values.
func (h *HALHandler) getUPSInfo() UPSInfo {
	driver := h.powerMonitor.Driver()
	if driver == nil {
		// No UPS configured — return safe default
		return UPSInfo{
			Model:     "none",
			Detected:  false,
			GPIOChip:  detectGPIOChip(),
			PiVersion: detectPiVersion(),
		}
	}

	info := UPSInfo{
		Model:     driver.Name(),
		Detected:  true,
		GPIOChip:  detectGPIOChip(),
		PiVersion: detectPiVersion(),
	}

	// Set driver-specific fields based on model type
	switch driver.Name() {
	case "Geekworm X1202", "Geekworm X728":
		info.I2CAddress = "0x36"
		info.I2CBus = DefaultI2CBus
		info.FuelGauge = "MAX17040"

		// Try to read chip version for extra info
		if output, err := execWithTimeout(context.Background(), "i2cget", "-y", "1", "0x36", "0x08", "w"); err == nil {
			valStr := strings.TrimSpace(output)
			if val, err := strconv.ParseInt(strings.TrimPrefix(valStr, "0x"), 16, 64); err == nil {
				info.ChipVersion = uint16(val)
			}
		}
	case "PiSugar 3":
		info.I2CAddress = "0x57"
		info.I2CBus = DefaultI2CBus
		info.FuelGauge = "Custom MCU"
	}

	return info
}

func (h *HALHandler) getRTCStatus() RTCStatus {
	status := RTCStatus{
		Available: false,
		Device:    "/dev/rtc0",
	}

	if _, err := os.Stat("/dev/rtc0"); err == nil {
		// /dev/rtc0 may exist from the watchdog timer on Pi4 (no real RTC).
		// Verify by actually reading the clock — hwclock -r fails if no real RTC.
		if output, err := execWithTimeout(context.Background(), "hwclock", "-r"); err == nil {
			status.Available = true
			status.Time = strings.TrimSpace(output)
			status.Synchronized = true
			status.BatteryOK = true
		}
	}

	return status
}

// isRTCAvailable checks whether a functional RTC is present (not just /dev/rtc0 from watchdog).
func (h *HALHandler) isRTCAvailable() bool {
	_, err := execWithTimeout(context.Background(), "hwclock", "-r")
	return err == nil
}

func (h *HALHandler) getWatchdogInfo() WatchdogInfo {
	info := WatchdogInfo{
		Device:  "/dev/watchdog",
		Enabled: false,
		Timeout: 15,
	}

	if _, err := os.Stat("/dev/watchdog"); err == nil {
		info.Enabled = true
	}

	if data, err := os.ReadFile("/sys/class/watchdog/watchdog0/timeout"); err == nil {
		if timeout, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			info.Timeout = timeout
		}
	}

	return info
}
