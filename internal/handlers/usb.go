package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// ============================================================================
// USB Types
// ============================================================================

// USBDevice represents a USB device.
// @Description USB device information
type USBDevice struct {
	Bus       int    `json:"bus" example:"1"`
	Device    int    `json:"device" example:"5"`
	VendorID  string `json:"vendor_id" example:"1d6b"`
	ProductID string `json:"product_id" example:"0002"`
	Vendor    string `json:"vendor,omitempty" example:"Linux Foundation"`
	Product   string `json:"product,omitempty" example:"USB 2.0 Hub"`
	Serial    string `json:"serial,omitempty"`
	Class     string `json:"class,omitempty" example:"Hub"`
	Speed     string `json:"speed,omitempty" example:"480M"`
	Path      string `json:"path,omitempty" example:"/dev/bus/usb/001/005"`
}

// USBDevicesResponse represents USB devices list.
// @Description List of USB devices
type USBDevicesResponse struct {
	Count   int         `json:"count" example:"5"`
	Devices []USBDevice `json:"devices"`
}

// USBHub represents a USB hub.
// @Description USB hub and its connected devices
type USBHub struct {
	Bus     int         `json:"bus" example:"1"`
	Devices []USBDevice `json:"devices"`
}

// ============================================================================
// USB Device Handlers
// ============================================================================

// GetUSBDevices lists USB devices.
// @Summary List USB devices
// @Description Returns list of all connected USB devices
// @Tags USB
// @Accept json
// @Produce json
// @Success 200 {object} USBDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /usb/devices [get]
func (h *HALHandler) GetUSBDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.scanUSBDevices(r)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// GetUSBDevicesTree returns USB devices as a tree.
// @Summary Get USB device tree
// @Description Returns USB devices organized by hub/bus
// @Tags USB
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /usb/tree [get]
func (h *HALHandler) GetUSBDevicesTree(w http.ResponseWriter, r *http.Request) {
	// Use lsusb -t for tree output
	output, err := execWithTimeout(r.Context(), "lsusb", "-t")
	if err != nil {
		log.Printf("lsusb -t failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("get USB tree", err))
		return
	}

	// Also get detailed list
	devices := h.scanUSBDevices(r)

	// Group by bus
	hubs := make(map[int][]USBDevice)
	for _, device := range devices {
		hubs[device.Bus] = append(hubs[device.Bus], device)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"tree": output,
		"hubs": hubs,
	})
}

// GetUSBDevicesByClass returns USB devices filtered by class.
// @Summary Get USB devices by class
// @Description Returns USB devices filtered by device class
// @Tags USB
// @Accept json
// @Produce json
// @Param class query string true "Device class" Enums(hub, storage, audio, video, hid, serial, network)
// @Success 200 {object} USBDevicesResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /usb/class [get]
func (h *HALHandler) GetUSBDevicesByClass(w http.ResponseWriter, r *http.Request) {
	class := r.URL.Query().Get("class")
	if class == "" {
		errorResponse(w, http.StatusBadRequest, "class required")
		return
	}

	devices := h.scanUSBDevices(r)

	// Map class names to class codes
	classMap := map[string][]string{
		"hub":     {"Hub"},
		"storage": {"Mass Storage"},
		"audio":   {"Audio"},
		"video":   {"Video"},
		"hid":     {"Human Interface Device"},
		"serial":  {"CDC", "Communications"},
		"network": {"Wireless", "Ethernet"},
	}

	patterns := classMap[strings.ToLower(class)]
	if patterns == nil {
		errorResponse(w, http.StatusBadRequest, "invalid class: "+class)
		return
	}

	var filtered []USBDevice
	for _, device := range devices {
		for _, pattern := range patterns {
			if strings.Contains(device.Class, pattern) ||
				strings.Contains(device.Product, pattern) {
				filtered = append(filtered, device)
				break
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(filtered),
		"class":   class,
		"devices": filtered,
	})
}

// ResetUSBDevice resets a USB device.
// @Summary Reset USB device
// @Description Resets a USB device by bus and device number
// @Tags USB
// @Accept json
// @Produce json
// @Param bus query int true "USB bus number" example(1)
// @Param device query int true "USB device number" example(5)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /usb/reset [post]
func (h *HALHandler) ResetUSBDevice(w http.ResponseWriter, r *http.Request) {
	busStr := r.URL.Query().Get("bus")
	deviceStr := r.URL.Query().Get("device")

	bus, err := strconv.Atoi(busStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid bus number")
		return
	}

	device, err := strconv.Atoi(deviceStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid device number")
		return
	}

	// HF04-02: Validate bus/device range
	if err := validateUSBBusDevice(bus, device); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-02: Fix broken format string — was passing literal "%03d/%03d" to usbreset
	devPath := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, device)

	_, err = execWithTimeout(r.Context(), "usbreset", devPath)
	if err != nil {
		log.Printf("usbreset %s failed: %v", devPath, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("USB reset", err))
		return
	}

	successResponse(w, fmt.Sprintf("USB device %s reset", devPath))
}

// RescanUSB rescans USB buses.
// @Summary Rescan USB
// @Description Triggers a rescan of USB buses
// @Tags USB
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /usb/rescan [post]
func (h *HALHandler) RescanUSB(w http.ResponseWriter, r *http.Request) {
	// Trigger rescan via udevadm
	_, err := execWithTimeout(r.Context(), "udevadm", "trigger", "--subsystem-match=usb")
	if err != nil {
		log.Printf("USB rescan failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("USB rescan", err))
		return
	}

	successResponse(w, "USB rescan triggered")
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) scanUSBDevices(r *http.Request) []USBDevice {
	var devices []USBDevice

	// Use lsusb for device listing
	output, err := execWithTimeout(r.Context(), "lsusb")
	if err != nil {
		return devices
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}

		device := USBDevice{}

		// Parse: "Bus 001 Device 005: ID 1d6b:0002 Linux Foundation 2.0 root hub"
		// Extract bus and device numbers
		if idx := strings.Index(line, "Bus "); idx != -1 {
			busStr := line[idx+4 : idx+7]
			device.Bus, _ = strconv.Atoi(busStr)
		}

		if idx := strings.Index(line, "Device "); idx != -1 {
			devStr := line[idx+7 : idx+10]
			device.Device, _ = strconv.Atoi(strings.TrimRight(devStr, ":"))
		}

		// Extract IDs
		if idx := strings.Index(line, "ID "); idx != -1 {
			idPart := line[idx+3:]
			if colonIdx := strings.Index(idPart, ":"); colonIdx != -1 {
				device.VendorID = idPart[:colonIdx]
				// Product ID ends at space or end
				productPart := idPart[colonIdx+1:]
				if spaceIdx := strings.Index(productPart, " "); spaceIdx != -1 {
					device.ProductID = productPart[:spaceIdx]
					// Rest is product description
					description := strings.TrimSpace(productPart[spaceIdx:])
					// First word(s) are vendor, rest is product
					parts := strings.SplitN(description, " ", 2)
					if len(parts) >= 1 {
						device.Vendor = parts[0]
					}
					if len(parts) >= 2 {
						device.Product = parts[1]
					}
				} else {
					device.ProductID = productPart
				}
			}
		}

		// HF04-11: Fix path padding — strings.ReplaceAll doesn't pad, use fmt.Sprintf
		device.Path = fmt.Sprintf("/dev/bus/usb/%03d/%03d", device.Bus, device.Device)

		if device.VendorID != "" {
			devices = append(devices, device)
		}
	}

	// Try to get more details for each device
	for i := range devices {
		h.enrichUSBDevice(r, &devices[i])
	}

	return devices
}

func (h *HALHandler) enrichUSBDevice(r *http.Request, device *USBDevice) {
	// Use lsusb -v -s bus:device for more details
	selector := strconv.Itoa(device.Bus) + ":" + strconv.Itoa(device.Device)
	output, err := execWithTimeout(r.Context(), "lsusb", "-v", "-s", selector)
	if err != nil {
		return
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "iProduct") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) >= 3 {
				device.Product = parts[2]
			}
		}

		if strings.HasPrefix(line, "iManufacturer") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) >= 3 {
				device.Vendor = parts[2]
			}
		}

		if strings.HasPrefix(line, "iSerial") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) >= 3 {
				device.Serial = parts[2]
			}
		}

		if strings.HasPrefix(line, "bDeviceClass") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) >= 3 {
				device.Class = strings.TrimSpace(parts[2])
			}
		}

		if strings.HasPrefix(line, "bInterfaceClass") && device.Class == "" {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) >= 3 {
				device.Class = strings.TrimSpace(parts[2])
			}
		}
	}
}
