package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/go-chi/chi/v5"
)

// powerActionInProgress guards against double-invocation of reboot/shutdown.
var powerActionInProgress atomic.Bool

// ============================================================================
// System Types
// ============================================================================

// TemperatureResponse represents CPU temperature.
// @Description CPU temperature reading
type TemperatureResponse struct {
	Temperature float64 `json:"temperature" example:"56.5"`
	Unit        string  `json:"unit" example:"celsius"`
	Source      string  `json:"source" example:"sysfs"`
}

// ThrottleStatus represents throttling status.
// @Description CPU throttling status flags
type ThrottleStatus struct {
	UnderVoltageOccurred         bool   `json:"under_voltage_occurred" example:"false"`
	ArmFrequencyCappedOccurred   bool   `json:"arm_frequency_capped_occurred" example:"false"`
	CurrentlyThrottled           bool   `json:"currently_throttled" example:"false"`
	SoftTemperatureLimitOccurred bool   `json:"soft_temperature_limit_occurred" example:"false"`
	UnderVoltageNow              bool   `json:"under_voltage_now" example:"false"`
	ArmFrequencyCappedNow        bool   `json:"arm_frequency_capped_now" example:"false"`
	ThrottledNow                 bool   `json:"throttled_now" example:"false"`
	SoftTemperatureLimitNow      bool   `json:"soft_temperature_limit_now" example:"false"`
	RawHex                       string `json:"raw_hex" example:"0x0"`
	Source                       string `json:"source,omitempty" example:"sysfs"`
}

// EEPROMInfo represents Raspberry Pi EEPROM information.
// @Description Raspberry Pi EEPROM/firmware information
type EEPROMInfo struct {
	Version    string `json:"version" example:"2024-01-15"`
	Bootloader string `json:"bootloader,omitempty"`
	VL805      string `json:"vl805,omitempty"`
	Model      string `json:"model,omitempty" example:"Raspberry Pi 5 Model B Rev 1.0"`
	Serial     string `json:"serial,omitempty" example:"10000000abcd1234"`
	Revision   string `json:"revision,omitempty"`
}

// BootConfig represents boot configuration.
// @Description Boot configuration from config.txt
type BootConfig struct {
	Config map[string]string `json:"config"`
	Raw    string            `json:"raw,omitempty"`
}

// HostnameResponse represents the system hostname.
// @Description System hostname
type HostnameResponse struct {
	Hostname string `json:"hostname" example:"cubeos"`
}

// SetHostnameRequest represents a request to set the hostname.
// @Description Set hostname request
type SetHostnameRequest struct {
	Hostname string `json:"hostname" example:"my-cubeos"`
}

// OSInfoResponse represents the host OS information.
// @Description Host operating system information
type OSInfoResponse struct {
	Name    string `json:"name" example:"Debian GNU/Linux"`
	Version string `json:"version" example:"12"`
	ID      string `json:"id" example:"debian"`
	Pretty  string `json:"pretty_name" example:"Debian GNU/Linux 12 (bookworm)"`
}

// ServiceStatus represents a systemd service status.
// @Description Systemd service status
type ServiceStatus struct {
	Name        string `json:"name" example:"cubeos-hal"`
	Active      bool   `json:"active" example:"true"`
	Running     bool   `json:"running" example:"true"`
	Enabled     bool   `json:"enabled" example:"true"`
	Description string `json:"description,omitempty"`
	LoadState   string `json:"load_state" example:"loaded"`
	ActiveState string `json:"active_state" example:"active"`
	SubState    string `json:"sub_state" example:"running"`
	MainPID     int    `json:"main_pid,omitempty" example:"1234"`
}

// ============================================================================
// System Control Handlers
// ============================================================================

// Reboot reboots the system.
// @Summary Reboot system
// @Description Initiates a system reboot
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/reboot [post]
func (h *HALHandler) Reboot(w http.ResponseWriter, r *http.Request) {
	if !powerActionInProgress.CompareAndSwap(false, true) {
		errorResponse(w, http.StatusConflict, "power action already in progress")
		return
	}
	successResponse(w, "system rebooting...")
	go func() {
		time.Sleep(1 * time.Second)
		// nsenter into host PID 1 mount namespace — Alpine container has no systemctl
		if _, err := execWithTimeout(context.Background(), "nsenter", "-t", "1", "-m", "--", "systemctl", "reboot"); err != nil {
			log.Printf("reboot via nsenter failed: %v, trying reboot -f", err)
			// Fallback: BusyBox reboot -f uses reboot() syscall directly
			if _, err2 := execWithTimeout(context.Background(), "reboot", "-f"); err2 != nil {
				log.Printf("reboot -f also failed: %v", err2)
			}
			powerActionInProgress.Store(false)
		}
	}()
}

// Shutdown shuts down the system.
// @Summary Shutdown system
// @Description Initiates a system shutdown
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/shutdown [post]
func (h *HALHandler) Shutdown(w http.ResponseWriter, r *http.Request) {
	if !powerActionInProgress.CompareAndSwap(false, true) {
		errorResponse(w, http.StatusConflict, "power action already in progress")
		return
	}
	successResponse(w, "system shutting down...")
	go func() {
		time.Sleep(1 * time.Second)

		// Call UPS driver shutdown sequence before system halt.
		// On X728 (Pi 4): pulses GPIO 26 HIGH for 4s — MANDATORY or UPS drains battery.
		// On X1202 (Pi 5): no-op (MCU auto-detects halt via current draw).
		// On PiSugar3: clears output switch to cut power.
		// On nil (no UPS): skip.
		var driver UPSDriver
		if pm := h.powerMonitor; pm != nil {
			driver = pm.Driver()
		}
		if driver == nil {
			// Power monitor not started yet — run one-shot UPS detection
			log.Printf("Shutdown: power monitor driver nil, running one-shot UPS detection")
			driver = DetectUPS()
			if driver != nil {
				log.Printf("Shutdown: detected %s via one-shot probe", driver.Name())
			}
		}
		if driver != nil {
			log.Printf("Shutdown: calling UPS driver InitiateShutdown (%s)", driver.Name())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := driver.InitiateShutdown(ctx); err != nil {
				log.Printf("Shutdown: UPS InitiateShutdown error (non-fatal): %v", err)
			}
			cancel()
			log.Printf("Shutdown: UPS driver shutdown complete, proceeding with system halt")
		}

		// nsenter into host PID 1 mount namespace — Alpine container has no systemctl
		if _, err := execWithTimeout(context.Background(), "nsenter", "-t", "1", "-m", "--", "systemctl", "poweroff"); err != nil {
			log.Printf("shutdown via nsenter failed: %v, trying poweroff -f", err)
			if _, err2 := execWithTimeout(context.Background(), "poweroff", "-f"); err2 != nil {
				log.Printf("poweroff -f also failed: %v", err2)
			}
			powerActionInProgress.Store(false)
		}
	}()
}

// ============================================================================
// System Information Handlers
// ============================================================================

// GetCPUTemp returns CPU temperature.
// @Summary Get CPU temperature
// @Description Returns current CPU temperature from sysfs thermal zone
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} TemperatureResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/temperature [get]
func (h *HALHandler) GetCPUTemp(w http.ResponseWriter, r *http.Request) {
	// Read from sysfs thermal zone (works in containers without vcgencmd)
	// Returns millidegrees, e.g., 57850 = 57.85°C
	thermalPaths := []string{
		"/sys/class/thermal/thermal_zone0/temp",
		"/sys/devices/virtual/thermal/thermal_zone0/temp",
	}

	var temp float64
	var source string
	var found bool

	for _, path := range thermalPaths {
		data, err := os.ReadFile(path)
		if err == nil {
			milliTemp, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			if parseErr == nil {
				temp = float64(milliTemp) / 1000.0
				source = "sysfs"
				found = true
				break
			}
		}
	}

	if !found {
		errorResponse(w, http.StatusInternalServerError, "failed to read temperature from sysfs")
		return
	}

	jsonResponse(w, http.StatusOK, TemperatureResponse{
		Temperature: temp,
		Unit:        "celsius",
		Source:      source,
	})
}

// GetThrottleStatus returns throttling status.
// @Summary Get throttle status
// @Description Returns CPU throttling status flags from sysfs
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} ThrottleStatus
// @Failure 500 {object} ErrorResponse
// @Router /system/throttle [get]
func (h *HALHandler) GetThrottleStatus(w http.ResponseWriter, r *http.Request) {
	// Try sysfs first (Raspberry Pi kernel exposes this)
	throttlePaths := []string{
		"/sys/devices/platform/soc/soc:firmware/get_throttled",
		"/sys/class/hwmon/hwmon0/throttled",
	}

	var hexVal string
	var source string
	var found bool

	for _, path := range throttlePaths {
		data, err := os.ReadFile(path)
		if err == nil {
			hexVal = strings.TrimSpace(string(data))
			source = "sysfs"
			found = true
			break
		}
	}

	// If sysfs not available, return zeros (no throttling detected)
	if !found {
		// Return empty throttle status rather than error
		// This allows the endpoint to work in containers/VMs
		status := ThrottleStatus{
			RawHex: "0x0",
			Source: "unavailable",
		}
		jsonResponse(w, http.StatusOK, status)
		return
	}

	// Parse hex value (may be "0x0" or just "0")
	hexVal = strings.TrimPrefix(hexVal, "0x")
	val, err := strconv.ParseInt(hexVal, 16, 64)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to parse throttle status")
		return
	}

	status := ThrottleStatus{
		UnderVoltageOccurred:         val&(1<<16) != 0,
		ArmFrequencyCappedOccurred:   val&(1<<17) != 0,
		CurrentlyThrottled:           val&(1<<18) != 0,
		SoftTemperatureLimitOccurred: val&(1<<19) != 0,
		UnderVoltageNow:              val&(1<<0) != 0,
		ArmFrequencyCappedNow:        val&(1<<1) != 0,
		ThrottledNow:                 val&(1<<2) != 0,
		SoftTemperatureLimitNow:      val&(1<<3) != 0,
		RawHex:                       "0x" + hexVal,
		Source:                       source,
	}

	jsonResponse(w, http.StatusOK, status)
}

// GetEEPROMInfo returns Raspberry Pi EEPROM/firmware information.
// @Summary Get EEPROM info
// @Description Returns Raspberry Pi EEPROM/firmware version and hardware info
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} EEPROMInfo
// @Failure 500 {object} ErrorResponse
// @Router /system/eeprom [get]
func (h *HALHandler) GetEEPROMInfo(w http.ResponseWriter, r *http.Request) {
	info := EEPROMInfo{}

	// Try to get bootloader version from /proc/device-tree (works without vcgencmd)
	if data, err := os.ReadFile("/proc/device-tree/system/linux,revision"); err == nil {
		info.Revision = fmt.Sprintf("%x", data)
	}

	// Get model info from /proc/cpuinfo
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "Model") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					info.Model = strings.TrimSpace(parts[1])
				}
			}
			if strings.HasPrefix(line, "Serial") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					info.Serial = strings.TrimSpace(parts[1])
				}
			}
			if strings.HasPrefix(line, "Revision") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					info.Revision = strings.TrimSpace(parts[1])
				}
			}
		}
	}

	// Try to get bootloader info from /proc/device-tree
	if data, err := os.ReadFile("/proc/device-tree/chosen/bootloader/version"); err == nil {
		info.Version = strings.TrimSpace(strings.TrimRight(string(data), "\x00"))
	}

	jsonResponse(w, http.StatusOK, info)
}

// GetBootConfig returns boot configuration.
// @Summary Get boot configuration
// @Description Returns boot configuration from config.txt
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} BootConfig
// @Failure 500 {object} ErrorResponse
// @Router /system/bootconfig [get]
func (h *HALHandler) GetBootConfig(w http.ResponseWriter, r *http.Request) {
	config := BootConfig{
		Config: make(map[string]string),
	}

	// Try common config.txt locations
	configPaths := []string{
		"/boot/firmware/config.txt",
		"/boot/config.txt",
	}

	for _, path := range configPaths {
		if data, err := os.ReadFile(path); err == nil {
			config.Raw = string(data)
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					config.Config[parts[0]] = parts[1]
				}
			}
			break
		}
	}

	jsonResponse(w, http.StatusOK, config)
}

// ============================================================================
// Hostname Handlers
// ============================================================================

// GetHostname returns the system hostname.
// @Summary Get hostname
// @Description Returns the real host hostname. HAL runs with network_mode: host, so os.Hostname() returns the actual host hostname.
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} HostnameResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/hostname [get]
func (h *HALHandler) GetHostname(w http.ResponseWriter, r *http.Request) {
	// Try /etc/hostname first for consistency
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		hostname := strings.TrimSpace(string(data))
		if hostname != "" {
			jsonResponse(w, http.StatusOK, HostnameResponse{Hostname: hostname})
			return
		}
	}

	// Fall back to os.Hostname() — works because HAL has network_mode: host
	hostname, err := os.Hostname()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to get hostname: "+err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, HostnameResponse{Hostname: hostname})
}

// SetHostname sets the system hostname.
// @Summary Set hostname
// @Description Sets the system hostname using hostnamectl. HAL has host PID namespace + privileged mode, so systemd commands work.
// @Tags System
// @Accept json
// @Produce json
// @Param request body SetHostnameRequest true "Hostname to set"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/hostname [post]
func (h *HALHandler) SetHostname(w http.ResponseWriter, r *http.Request) {
	var req SetHostnameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	hostname := strings.TrimSpace(req.Hostname)
	if hostname == "" {
		errorResponse(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if len(hostname) > 253 {
		errorResponse(w, http.StatusBadRequest, "hostname too long (max 253 characters)")
		return
	}
	// Validate DNS-safe characters: lowercase letters, digits, hyphens; no leading/trailing hyphen
	for i, c := range hostname {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
			errorResponse(w, http.StatusBadRequest, fmt.Sprintf("invalid character '%c' at position %d; only letters, digits, hyphens, and dots allowed", c, i))
			return
		}
	}
	if hostname[0] == '-' || hostname[len(hostname)-1] == '-' {
		errorResponse(w, http.StatusBadRequest, "hostname cannot start or end with a hyphen")
		return
	}

	// Use hostnamectl (HAL has host PID namespace + privileged mode)
	if _, err := execWithTimeout(r.Context(), "hostnamectl", "set-hostname", hostname); err != nil {
		log.Printf("SetHostname: hostnamectl failed: %v, falling back to /etc/hostname", err)
		// Fall back to writing /etc/hostname directly
		if writeErr := os.WriteFile("/etc/hostname", []byte(hostname+"\n"), 0644); writeErr != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to set hostname: "+writeErr.Error())
			return
		}
	}

	successResponse(w, "hostname set to "+hostname)
}

// GetOSInfo returns host operating system information.
// @Summary Get OS info
// @Description Returns host OS name and version from /etc/os-release. HAL runs with network_mode: host, so this returns the real host OS, not the container OS.
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} OSInfoResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/os [get]
func (h *HALHandler) GetOSInfo(w http.ResponseWriter, r *http.Request) {
	// Try host-mounted os-release first (container filesystem is Alpine, not the host OS)
	var data []byte
	var err error
	for _, path := range []string{"/host/etc/os-release", "/etc/os-release"} {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to read os-release: "+err.Error())
		return
	}

	info := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "="); idx > 0 {
			key := line[:idx]
			value := strings.Trim(line[idx+1:], `"`)
			info[key] = value
		}
	}

	jsonResponse(w, http.StatusOK, OSInfoResponse{
		Name:    info["NAME"],
		Version: info["VERSION_ID"],
		ID:      info["ID"],
		Pretty:  info["PRETTY_NAME"],
	})
}

// ============================================================================
// Service Management Handlers
// ============================================================================

// serviceValidationError handles the validateServiceName error, returning 501
// for allowlist misses (B70) and 400 for invalid names.
func serviceValidationError(w http.ResponseWriter, name string, err error) {
	if errors.Is(err, ErrServiceNotInAllowlist) {
		errorResponse(w, http.StatusNotImplemented, "service not in allowlist: "+name)
	} else {
		errorResponse(w, http.StatusBadRequest, err.Error())
	}
}

// ServiceStatus returns the status of a systemd service.
// @Summary Get service status
// @Description Returns the status of a systemd service
// @Tags System
// @Accept json
// @Produce json
// @Param name path string true "Service name" example(cubeos-hal)
// @Success 200 {object} ServiceStatus
// @Failure 400 {object} ErrorResponse "Invalid service name"
// @Failure 501 {object} ErrorResponse "Service not in allowlist"
// @Failure 500 {object} ErrorResponse
// @Router /system/service/{name}/status [get]
func (h *HALHandler) ServiceStatus(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateServiceName(name); err != nil {
		serviceValidationError(w, name, err)
		return
	}

	// Ensure .service suffix
	if !strings.HasSuffix(name, ".service") {
		name = name + ".service"
	}

	ctx := context.Background()
	conn, err := dbus.NewWithContext(ctx)
	if err != nil {
		log.Printf("ServiceStatus: dbus connection failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to connect to system manager")
		return
	}
	defer conn.Close()

	status := ServiceStatus{Name: name}

	// Get unit properties
	props, err := conn.GetUnitPropertiesContext(ctx, name)
	if err != nil {
		log.Printf("ServiceStatus: GetUnitProperties failed for %s: %v", name, err)
		errorResponse(w, http.StatusInternalServerError, "failed to get service status")
		return
	}

	if v, ok := props["ActiveState"].(string); ok {
		status.ActiveState = v
		status.Active = v == "active"
	}
	if v, ok := props["SubState"].(string); ok {
		status.SubState = v
		status.Running = v == "running"
	}
	if v, ok := props["LoadState"].(string); ok {
		status.LoadState = v
	}
	if v, ok := props["Description"].(string); ok {
		status.Description = v
	}
	if v, ok := props["MainPID"].(uint32); ok {
		status.MainPID = int(v)
	}

	// Check if enabled using systemctl
	if enabledOutput, err := execWithTimeout(r.Context(), "systemctl", "is-enabled", name); err == nil {
		status.Enabled = strings.TrimSpace(enabledOutput) == "enabled"
	}

	jsonResponse(w, http.StatusOK, status)
}

// RestartService restarts a systemd service.
// @Summary Restart service
// @Description Restarts a systemd service
// @Tags System
// @Accept json
// @Produce json
// @Param name path string true "Service name" example(cubeos-hal)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse "Invalid service name"
// @Failure 501 {object} ErrorResponse "Service not in allowlist"
// @Failure 500 {object} ErrorResponse
// @Router /system/service/{name}/restart [post]
func (h *HALHandler) RestartService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateServiceName(name); err != nil {
		serviceValidationError(w, name, err)
		return
	}

	if !strings.HasSuffix(name, ".service") {
		name = name + ".service"
	}

	ctx := context.Background()
	conn, err := dbus.NewWithContext(ctx)
	if err != nil {
		log.Printf("RestartService: dbus connection failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to connect to system manager")
		return
	}
	defer conn.Close()

	resultChan := make(chan string, 1)
	_, err = conn.RestartUnitContext(ctx, name, "replace", resultChan)
	if err != nil {
		log.Printf("RestartService: RestartUnit failed for %s: %v", name, err)
		errorResponse(w, http.StatusInternalServerError, "failed to restart service")
		return
	}

	result := <-resultChan
	if result != "done" {
		errorResponse(w, http.StatusInternalServerError, "service restart failed")
		return
	}

	successResponse(w, fmt.Sprintf("service %s restarted", name))
}

// StartService starts a systemd service.
// @Summary Start service
// @Description Starts a systemd service
// @Tags System
// @Accept json
// @Produce json
// @Param name path string true "Service name" example(cubeos-hal)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse "Invalid service name"
// @Failure 501 {object} ErrorResponse "Service not in allowlist"
// @Failure 500 {object} ErrorResponse
// @Router /system/service/{name}/start [post]
func (h *HALHandler) StartService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateServiceName(name); err != nil {
		serviceValidationError(w, name, err)
		return
	}

	if !strings.HasSuffix(name, ".service") {
		name = name + ".service"
	}

	ctx := context.Background()
	conn, err := dbus.NewWithContext(ctx)
	if err != nil {
		log.Printf("StartService: dbus connection failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to connect to system manager")
		return
	}
	defer conn.Close()

	resultChan := make(chan string, 1)
	_, err = conn.StartUnitContext(ctx, name, "replace", resultChan)
	if err != nil {
		log.Printf("StartService: StartUnit failed for %s: %v", name, err)
		errorResponse(w, http.StatusInternalServerError, "failed to start service")
		return
	}

	result := <-resultChan
	if result != "done" {
		errorResponse(w, http.StatusInternalServerError, "service start failed")
		return
	}

	successResponse(w, fmt.Sprintf("service %s started", name))
}

// StopService stops a systemd service.
// @Summary Stop service
// @Description Stops a systemd service
// @Tags System
// @Accept json
// @Produce json
// @Param name path string true "Service name" example(cubeos-hal)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse "Invalid service name"
// @Failure 501 {object} ErrorResponse "Service not in allowlist"
// @Failure 500 {object} ErrorResponse
// @Router /system/service/{name}/stop [post]
func (h *HALHandler) StopService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateServiceName(name); err != nil {
		serviceValidationError(w, name, err)
		return
	}

	if !strings.HasSuffix(name, ".service") {
		name = name + ".service"
	}

	ctx := context.Background()
	conn, err := dbus.NewWithContext(ctx)
	if err != nil {
		log.Printf("StopService: dbus connection failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to connect to system manager")
		return
	}
	defer conn.Close()

	resultChan := make(chan string, 1)
	_, err = conn.StopUnitContext(ctx, name, "replace", resultChan)
	if err != nil {
		log.Printf("StopService: StopUnit failed for %s: %v", name, err)
		errorResponse(w, http.StatusInternalServerError, "failed to stop service")
		return
	}

	result := <-resultChan
	if result != "done" {
		errorResponse(w, http.StatusInternalServerError, "service stop failed")
		return
	}

	successResponse(w, fmt.Sprintf("service %s stopped", name))
}

// ============================================================================
// Docker Compose Service Recreate
// ============================================================================

// composeServiceAllowlist restricts which Docker Compose services can be recreated.
var composeServiceAllowlist = map[string]string{
	"pihole": "/cubeos/coreapps/pihole/appconfig",
}

// RecreateComposeService runs `docker compose up -d` for a whitelisted service.
// @Summary Recreate Docker Compose service
// @Description Recreates a Docker Compose service by running `docker compose up -d` in its appconfig directory. Used when environment changes require container recreation (e.g. Pi-hole password sync).
// @Tags System
// @Accept json
// @Produce json
// @Param name path string true "Service name (must be in allowlist)" example(pihole)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse "Invalid service name"
// @Failure 403 {object} ErrorResponse "Service not in allowlist"
// @Failure 500 {object} ErrorResponse
// @Router /system/service/{name}/recreate [post]
func (h *HALHandler) RecreateComposeService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		errorResponse(w, http.StatusBadRequest, "service name is required")
		return
	}

	composeDir, ok := composeServiceAllowlist[name]
	if !ok {
		errorResponse(w, http.StatusForbidden, fmt.Sprintf("service %q is not in the compose recreate allowlist", name))
		return
	}

	output, err := execWithTimeout(r.Context(), "docker", "compose", "-f", composeDir+"/docker-compose.yml", "up", "-d")
	if err != nil {
		log.Printf("RecreateComposeService: docker compose up -d failed for %s: %v (output: %s)", name, err, output)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("recreate compose service", err))
		return
	}

	successResponse(w, fmt.Sprintf("service %s recreated via docker compose", name))
}

// ============================================================================
// Extended System Information Handlers (Fix #13)
// ============================================================================

// SystemInfoResponse represents combined system information.
// @Description Combined system hardware and software information
type SystemInfoResponse struct {
	Model        string `json:"model" example:"Raspberry Pi 5 Model B Rev 1.0"`
	Serial       string `json:"serial,omitempty" example:"10000000abcd1234"`
	Revision     string `json:"revision,omitempty" example:"d04170"`
	Kernel       string `json:"kernel" example:"6.6.31+rpt-rpi-2712"`
	Architecture string `json:"architecture" example:"aarch64"`
	MemoryTotal  int64  `json:"memory_total" example:"8589934592"`
	MemoryHuman  string `json:"memory_human" example:"8.0 GB"`
	PiVersion    int    `json:"pi_version" example:"5"`
}

// GetSystemInfo returns combined system information.
// @Summary Get system info
// @Description Returns hardware model, serial, kernel, architecture, and memory total
// @Tags System
// @Produce json
// @Success 200 {object} SystemInfoResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/info [get]
func (h *HALHandler) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	info := SystemInfoResponse{
		PiVersion: detectPiVersion(),
	}

	// Model from device tree (Raspberry Pi, ARM SBCs)
	if data, err := os.ReadFile("/sys/firmware/devicetree/base/model"); err == nil {
		info.Model = strings.TrimRight(string(data), "\x00\n")
	}

	// Fallback: detect virtualization or physical x86 when device tree is absent
	if info.Model == "" {
		info.Model = detectDeviceModel(r.Context())
	}

	// Serial from device tree
	if data, err := os.ReadFile("/sys/firmware/devicetree/base/serial-number"); err == nil {
		info.Serial = strings.TrimRight(string(data), "\x00\n")
	}

	// Revision from /proc/cpuinfo
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Revision") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					info.Revision = strings.TrimSpace(parts[1])
				}
			}
		}
	}

	// Kernel version
	if data, err := os.ReadFile("/proc/version"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 3 {
			info.Kernel = parts[2]
		}
	}

	// Architecture
	if output, err := execWithTimeout(r.Context(), "uname", "-m"); err == nil {
		info.Architecture = strings.TrimSpace(output)
	}

	// Total memory from /proc/meminfo
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						info.MemoryTotal = kb * 1024
						info.MemoryHuman = formatBytes(info.MemoryTotal)
					}
				}
				break
			}
		}
	}

	jsonResponse(w, http.StatusOK, info)
}

// detectDeviceModel returns a human-readable device name for non-Pi systems.
// Detection order:
//  1. CUBEOS_TIER env var (set by installer, always available in container)
//  2. Host PID 1 environment (HAL has pid:host, so /proc/1/environ is the host's init)
//  3. systemd-detect-virt (works on bare-metal hosts, not inside Docker)
//  4. DMI product name (physical x86 hardware)
//  5. CPU model name as last resort
func detectDeviceModel(ctx context.Context) string {
	// 1. Check CUBEOS_TIER env var — fastest, always set by installer
	if tier := os.Getenv("CUBEOS_TIER"); tier == "container" {
		return detectContainerHost()
	}

	// 2. Check host PID 1 environment (HAL runs with pid:host)
	if virt := detectVirtFromProc(); virt != "" {
		return virt
	}

	// 3. Try systemd-detect-virt (available on bare-metal hosts, not in Docker)
	if output, err := exec.CommandContext(ctx, "systemd-detect-virt").Output(); err == nil {
		virt := strings.TrimSpace(string(output))
		switch virt {
		case "lxc":
			return "LXC Container (" + getArchName() + ")"
		case "kvm":
			return "KVM Virtual Machine"
		case "qemu":
			return "QEMU Virtual Machine"
		case "vmware":
			return "VMware Virtual Machine"
		case "oracle":
			return "VirtualBox Virtual Machine"
		case "microsoft":
			return "Hyper-V Virtual Machine"
		case "none":
			// Physical hardware — try DMI
		default:
			if virt != "" {
				return "Virtual Machine (" + virt + ")"
			}
		}
	}

	// 4. Physical hardware: try DMI product name
	if data, err := os.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		name := strings.TrimSpace(string(data))
		if name != "" && name != "System Product Name" && name != "To Be Filled By O.E.M." {
			return name
		}
	}

	// 5. Fallback: CPU model name
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
	}

	return "Unknown " + getArchName() + " System"
}

// getArchName returns the runtime CPU architecture (e.g. "x86_64", "aarch64").
func getArchName() string {
	if out, err := exec.Command("uname", "-m").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return "unknown"
}

// detectContainerHost identifies the host virtualization type when CUBEOS_TIER=container.
// HAL has pid:host, so /proc/1/environ contains the host init's environment.
func detectContainerHost() string {
	arch := getArchName()
	// Read host PID 1 environment for container= indicator
	if data, err := os.ReadFile("/proc/1/environ"); err == nil {
		// environ uses NUL bytes as separators
		for _, entry := range strings.Split(string(data), "\x00") {
			if strings.HasPrefix(entry, "container=") {
				virt := strings.TrimPrefix(entry, "container=")
				switch virt {
				case "lxc":
					return "LXC Container (" + arch + ")"
				case "docker":
					return "Docker Container (" + arch + ")"
				case "podman":
					return "Podman Container (" + arch + ")"
				default:
					if virt != "" {
						return "Container (" + virt + ")"
					}
				}
			}
		}
	}

	return arch + " Container"
}

// detectVirtFromProc reads /proc/1/environ for container= on systems with pid:host.
func detectVirtFromProc() string {
	data, err := os.ReadFile("/proc/1/environ")
	if err != nil {
		return ""
	}
	arch := getArchName()
	for _, entry := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(entry, "container=") {
			virt := strings.TrimPrefix(entry, "container=")
			switch virt {
			case "lxc":
				return "LXC Container (" + arch + ")"
			case "docker":
				return "Docker Container (" + arch + ")"
			default:
				if virt != "" {
					return "Container (" + virt + ")"
				}
			}
		}
	}
	return ""
}

// CPUInfoResponse represents CPU information.
// @Description CPU information including cores, frequency, and usage
type CPUInfoResponse struct {
	Model      string  `json:"model" example:"Cortex-A76"`
	Cores      int     `json:"cores" example:"4"`
	CurFreqMHz float64 `json:"cur_freq_mhz" example:"2400.0"`
	MaxFreqMHz float64 `json:"max_freq_mhz" example:"2400.0"`
	MinFreqMHz float64 `json:"min_freq_mhz" example:"1500.0"`
	Governor   string  `json:"governor,omitempty" example:"ondemand"`
}

// GetCPUInfo returns CPU information.
// @Summary Get CPU info
// @Description Returns CPU model, core count, and frequency information
// @Tags System
// @Produce json
// @Success 200 {object} CPUInfoResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/cpu [get]
func (h *HALHandler) GetCPUInfo(w http.ResponseWriter, r *http.Request) {
	info := CPUInfoResponse{}

	// Count online CPUs
	if data, err := os.ReadFile("/sys/devices/system/cpu/online"); err == nil {
		rangeStr := strings.TrimSpace(string(data))
		// Parse "0-3" format
		if parts := strings.SplitN(rangeStr, "-", 2); len(parts) == 2 {
			if end, err := strconv.Atoi(parts[1]); err == nil {
				info.Cores = end + 1
			}
		} else {
			// Single CPU or comma-separated
			info.Cores = len(strings.Split(rangeStr, ","))
		}
	}

	// CPU model from /proc/cpuinfo (ARM uses "model name" or "CPU part")
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") || strings.HasPrefix(line, "Model") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					info.Model = strings.TrimSpace(parts[1])
					break
				}
			}
		}
	}

	// Current frequency (kHz -> MHz)
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq"); err == nil {
		if khz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			info.CurFreqMHz = khz / 1000.0
		}
	}

	// Max frequency
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq"); err == nil {
		if khz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			info.MaxFreqMHz = khz / 1000.0
		}
	}

	// Min frequency
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq"); err == nil {
		if khz, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			info.MinFreqMHz = khz / 1000.0
		}
	}

	// Governor
	if data, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"); err == nil {
		info.Governor = strings.TrimSpace(string(data))
	}

	jsonResponse(w, http.StatusOK, info)
}

// MemoryInfoResponse represents memory information.
// @Description System memory information
type MemoryInfoResponse struct {
	Total      int64  `json:"total" example:"8589934592"`
	Free       int64  `json:"free" example:"4294967296"`
	Available  int64  `json:"available" example:"6442450944"`
	Used       int64  `json:"used" example:"2147483648"`
	SwapTotal  int64  `json:"swap_total" example:"2147483648"`
	SwapFree   int64  `json:"swap_free" example:"2147483648"`
	TotalHuman string `json:"total_human" example:"8.0 GB"`
	UsedHuman  string `json:"used_human" example:"2.0 GB"`
	AvailHuman string `json:"avail_human" example:"6.0 GB"`
	UsePercent int    `json:"use_percent" example:"25"`
}

// GetMemoryInfo returns memory information.
// @Summary Get memory info
// @Description Returns RAM and swap usage from /proc/meminfo
// @Tags System
// @Produce json
// @Success 200 {object} MemoryInfoResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/memory [get]
func (h *HALHandler) GetMemoryInfo(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to read /proc/meminfo: "+err.Error())
		return
	}

	info := MemoryInfoResponse{}
	values := make(map[string]int64)

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			values[key] = kb * 1024 // Convert kB to bytes
		}
	}

	info.Total = values["MemTotal"]
	info.Free = values["MemFree"]
	info.Available = values["MemAvailable"]
	info.Used = info.Total - info.Available
	info.SwapTotal = values["SwapTotal"]
	info.SwapFree = values["SwapFree"]
	info.TotalHuman = formatBytes(info.Total)
	info.UsedHuman = formatBytes(info.Used)
	info.AvailHuman = formatBytes(info.Available)
	if info.Total > 0 {
		info.UsePercent = int(float64(info.Used) / float64(info.Total) * 100)
	}

	jsonResponse(w, http.StatusOK, info)
}

// DiskInfoResponse represents disk usage (delegates to storage/usage).
// @Description Root filesystem and storage usage
type DiskInfoResponse struct {
	Filesystems []FilesystemUsage `json:"filesystems"`
	RootUsed    int64             `json:"root_used,omitempty"`
	RootTotal   int64             `json:"root_total,omitempty"`
	RootPercent int               `json:"root_percent,omitempty"`
}

// GetDiskInfo returns disk usage information.
// @Summary Get disk info
// @Description Returns filesystem usage with root partition highlighted
// @Tags System
// @Produce json
// @Success 200 {object} DiskInfoResponse
// @Failure 500 {object} ErrorResponse
// @Router /system/disk [get]
func (h *HALHandler) GetDiskInfo(w http.ResponseWriter, r *http.Request) {
	output, err := execWithTimeout(r.Context(), "df", "-B1", "--output=target,source,size,used,avail,pcent")
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("disk usage", err))
		return
	}

	resp := DiskInfoResponse{}

	for i, line := range strings.Split(output, "\n") {
		if i == 0 { // Skip header
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}

		mountpoint := fields[0]
		// Skip pseudo filesystems
		if strings.HasPrefix(mountpoint, "/sys") || strings.HasPrefix(mountpoint, "/proc") ||
			strings.HasPrefix(mountpoint, "/dev") || strings.HasPrefix(mountpoint, "/run") ||
			mountpoint == "/etc/resolv.conf" || mountpoint == "/etc/hostname" || mountpoint == "/etc/hosts" {
			continue
		}

		size, _ := strconv.ParseInt(fields[2], 10, 64)
		used, _ := strconv.ParseInt(fields[3], 10, 64)
		avail, _ := strconv.ParseInt(fields[4], 10, 64)
		pctStr := strings.TrimSuffix(fields[5], "%")
		pct, _ := strconv.Atoi(pctStr)

		fs := FilesystemUsage{
			Mountpoint: mountpoint,
			Filesystem: fields[1],
			Size:       size,
			Used:       used,
			Available:  avail,
			UsePercent: pct,
			SizeHuman:  formatBytes(size),
			UsedHuman:  formatBytes(used),
			AvailHuman: formatBytes(avail),
		}
		resp.Filesystems = append(resp.Filesystems, fs)

		// Highlight root
		if mountpoint == "/" {
			resp.RootUsed = used
			resp.RootTotal = size
			resp.RootPercent = pct
		}
	}

	jsonResponse(w, http.StatusOK, resp)
}
