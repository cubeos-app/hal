package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// Bluetooth Types
// ============================================================================

// BluetoothDevice represents a Bluetooth device.
// @Description Bluetooth device information
type BluetoothDevice struct {
	Address   string `json:"address" example:"DC:A6:32:12:34:56"`
	Name      string `json:"name" example:"My Phone"`
	Paired    bool   `json:"paired" example:"true"`
	Connected bool   `json:"connected" example:"false"`
	Trusted   bool   `json:"trusted" example:"true"`
	Class     string `json:"class,omitempty" example:"Phone"`
	RSSI      int    `json:"rssi,omitempty" example:"-50"`
}

// BluetoothDevicesResponse represents Bluetooth devices list.
// @Description List of Bluetooth devices
type BluetoothDevicesResponse struct {
	Paired    []BluetoothDevice `json:"paired"`
	Available []BluetoothDevice `json:"available,omitempty"`
}

// BluetoothStatus represents Bluetooth adapter status.
// @Description Bluetooth adapter status
type BluetoothStatus struct {
	Available    bool   `json:"available" example:"true"`
	Powered      bool   `json:"powered" example:"true"`
	Discoverable bool   `json:"discoverable" example:"false"`
	Pairable     bool   `json:"pairable" example:"true"`
	Name         string `json:"name" example:"CubeOS"`
	Address      string `json:"address" example:"DC:A6:32:AA:BB:CC"`
	Alias        string `json:"alias,omitempty" example:"CubeOS"`
}

// BluetoothConnectRequest represents a Bluetooth connect request.
// @Description Bluetooth connect parameters
type BluetoothConnectRequest struct {
	Address string `json:"address" example:"DC:A6:32:12:34:56"`
}

// ============================================================================
// Bluetooth Adapter Handlers
// ============================================================================

// GetBluetoothStatus returns Bluetooth adapter status.
// @Summary Get Bluetooth status
// @Description Returns Bluetooth adapter status
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Success 200 {object} BluetoothStatus
// @Failure 404 {object} ErrorResponse "Bluetooth not available"
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/status [get]
func (h *HALHandler) GetBluetoothStatus(w http.ResponseWriter, r *http.Request) {
	status := BluetoothStatus{
		Available: false,
	}

	// Check if bluetoothctl is available
	output, err := execWithTimeout(r.Context(), "bluetoothctl", "show")
	if err != nil {
		errorResponse(w, http.StatusNotFound, "Bluetooth not available")
		return
	}

	status.Available = true

	// Parse output
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "Controller ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				status.Address = parts[1]
			}
		}

		if strings.HasPrefix(line, "Name:") {
			status.Name = strings.TrimPrefix(line, "Name: ")
		}

		if strings.HasPrefix(line, "Alias:") {
			status.Alias = strings.TrimPrefix(line, "Alias: ")
		}

		if strings.HasPrefix(line, "Powered:") {
			status.Powered = strings.Contains(line, "yes")
		}

		if strings.HasPrefix(line, "Discoverable:") {
			status.Discoverable = strings.Contains(line, "yes")
		}

		if strings.HasPrefix(line, "Pairable:") {
			status.Pairable = strings.Contains(line, "yes")
		}
	}

	jsonResponse(w, http.StatusOK, status)
}

// PowerOnBluetooth powers on Bluetooth.
// @Summary Power on Bluetooth
// @Description Powers on the Bluetooth adapter
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/power/on [post]
func (h *HALHandler) PowerOnBluetooth(w http.ResponseWriter, r *http.Request) {
	_, err := execWithTimeout(r.Context(), "bluetoothctl", "power", "on")
	if err != nil {
		log.Printf("bluetooth power on failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("power on Bluetooth", err))
		return
	}

	successResponse(w, "Bluetooth powered on")
}

// PowerOffBluetooth powers off Bluetooth.
// @Summary Power off Bluetooth
// @Description Powers off the Bluetooth adapter
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/power/off [post]
func (h *HALHandler) PowerOffBluetooth(w http.ResponseWriter, r *http.Request) {
	_, err := execWithTimeout(r.Context(), "bluetoothctl", "power", "off")
	if err != nil {
		log.Printf("bluetooth power off failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("power off Bluetooth", err))
		return
	}

	successResponse(w, "Bluetooth powered off")
}

// ============================================================================
// Bluetooth Device Handlers
// ============================================================================

// GetBluetoothDevices lists Bluetooth devices.
// @Summary List Bluetooth devices
// @Description Returns list of paired and available Bluetooth devices
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Success 200 {object} BluetoothDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/devices [get]
func (h *HALHandler) GetBluetoothDevices(w http.ResponseWriter, r *http.Request) {
	response := BluetoothDevicesResponse{
		Paired:    h.getPairedBluetoothDevices(r.Context()),
		Available: []BluetoothDevice{},
	}

	jsonResponse(w, http.StatusOK, response)
}

// ScanBluetoothDevices scans for Bluetooth devices.
// @Summary Scan Bluetooth devices
// @Description Scans for available Bluetooth devices with bounded duration
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Param duration query int false "Scan duration in seconds (1-30)" default(10)
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/scan [post]
func (h *HALHandler) ScanBluetoothDevices(w http.ResponseWriter, r *http.Request) {
	// HF04-01: Fix process leak — use bounded timeout instead of fire-and-forget cmd.Start()
	duration := 10
	if d := r.URL.Query().Get("duration"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			duration = n
		}
	}
	if duration < 1 || duration > 30 {
		errorResponse(w, http.StatusBadRequest, "scan duration must be 1-30 seconds")
		return
	}

	// Use a dedicated context with the scan duration as timeout.
	// bluetoothctl scan on runs indefinitely — the context cancellation kills it cleanly.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(duration)*time.Second)
	defer cancel()

	// Run scan with bounded timeout — process is killed when context expires.
	// bluetoothctl --timeout N scan on exits after N seconds.
	_, err := execWithTimeout(ctx, "bluetoothctl", "--timeout", strconv.Itoa(duration), "scan", "on")
	// bluetoothctl scan exits non-zero when the timeout fires — that's expected
	if err != nil && ctx.Err() != context.DeadlineExceeded {
		log.Printf("bluetooth scan error: %v", err)
	}

	successResponse(w, fmt.Sprintf("Bluetooth scan completed (%ds)", duration))
}

// PairBluetoothDevice pairs with a Bluetooth device.
// @Summary Pair Bluetooth device
// @Description Initiates pairing with a Bluetooth device
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Param request body BluetoothConnectRequest true "Device address"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/pair [post]
func (h *HALHandler) PairBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	// HF04-08: Apply limitBody
	r = limitBody(r, 1<<20)

	var req BluetoothConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// HF04-06: Validate Bluetooth MAC address
	if err := validateMACAddress(req.Address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "bluetoothctl", "pair", req.Address)
	if err != nil {
		log.Printf("bluetooth pair %s failed: %v", req.Address, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("pairing", err))
		return
	}

	successResponse(w, fmt.Sprintf("pairing initiated with %s", req.Address))
}

// ConnectBluetoothDevice connects to a Bluetooth device.
// @Summary Connect Bluetooth device
// @Description Connects to a paired Bluetooth device
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Param address path string true "Device address" example(DC:A6:32:12:34:56)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/connect/{address} [post]
func (h *HALHandler) ConnectBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")

	// HF04-06: Validate Bluetooth MAC address
	if err := validateMACAddress(address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "bluetoothctl", "connect", address)
	if err != nil {
		log.Printf("bluetooth connect %s failed: %v", address, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("connect", err))
		return
	}

	successResponse(w, fmt.Sprintf("connected to %s", address))
}

// DisconnectBluetoothDevice disconnects from a Bluetooth device.
// @Summary Disconnect Bluetooth device
// @Description Disconnects from a connected Bluetooth device
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Param address path string true "Device address" example(DC:A6:32:12:34:56)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/disconnect/{address} [post]
func (h *HALHandler) DisconnectBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")

	// HF04-06: Validate Bluetooth MAC address
	if err := validateMACAddress(address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "bluetoothctl", "disconnect", address)
	if err != nil {
		log.Printf("bluetooth disconnect %s failed: %v", address, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("disconnect", err))
		return
	}

	successResponse(w, fmt.Sprintf("disconnected from %s", address))
}

// RemoveBluetoothDevice removes a paired Bluetooth device.
// @Summary Remove Bluetooth device
// @Description Removes a paired Bluetooth device
// @Tags Bluetooth
// @Accept json
// @Produce json
// @Param address path string true "Device address" example(DC:A6:32:12:34:56)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /bluetooth/remove/{address} [delete]
func (h *HALHandler) RemoveBluetoothDevice(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")

	// HF04-06: Validate Bluetooth MAC address
	if err := validateMACAddress(address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "bluetoothctl", "remove", address)
	if err != nil {
		log.Printf("bluetooth remove %s failed: %v", address, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("remove", err))
		return
	}

	successResponse(w, fmt.Sprintf("removed %s", address))
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) getPairedBluetoothDevices(ctx context.Context) []BluetoothDevice {
	var devices []BluetoothDevice

	output, err := execWithTimeout(ctx, "bluetoothctl", "devices", "Paired")
	if err != nil {
		return devices
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse: "Device DC:A6:32:12:34:56 My Phone"
		if strings.HasPrefix(line, "Device ") {
			parts := strings.SplitN(line[7:], " ", 2)
			if len(parts) >= 1 {
				device := BluetoothDevice{
					Address: parts[0],
					Paired:  true,
				}
				if len(parts) >= 2 {
					device.Name = parts[1]
				}

				// Check if connected
				if infoOutput, err := execWithTimeout(ctx, "bluetoothctl", "info", device.Address); err == nil {
					device.Connected = strings.Contains(infoOutput, "Connected: yes")
					device.Trusted = strings.Contains(infoOutput, "Trusted: yes")
				}

				devices = append(devices, device)
			}
		}
	}

	return devices
}
