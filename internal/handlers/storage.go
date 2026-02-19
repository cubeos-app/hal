package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// Storage Types
// ============================================================================

// StorageDevice represents a storage device.
// @Description Storage device information
type StorageDevice struct {
	Name       string          `json:"name" example:"mmcblk0"`
	Path       string          `json:"path" example:"/dev/mmcblk0"`
	Size       int64           `json:"size" example:"64000000000"`
	SizeHuman  string          `json:"size_human" example:"64 GB"`
	Type       string          `json:"type" example:"disk"`
	Model      string          `json:"model,omitempty" example:"SD Card"`
	Serial     string          `json:"serial,omitempty"`
	Vendor     string          `json:"vendor,omitempty"`
	Removable  bool            `json:"removable" example:"false"`
	Partitions []StorageDevice `json:"partitions,omitempty"`
}

// StorageDevicesResponse represents storage devices list.
// @Description List of storage devices
type StorageDevicesResponse struct {
	Devices []StorageDevice `json:"devices"`
	Count   int             `json:"count" example:"2"`
}

// FilesystemUsage represents filesystem usage.
// @Description Filesystem usage information
type FilesystemUsage struct {
	Mountpoint string `json:"mountpoint" example:"/"`
	Filesystem string `json:"filesystem" example:"/dev/mmcblk0p2"`
	Size       int64  `json:"size"`
	Used       int64  `json:"used"`
	Available  int64  `json:"available"`
	UsePercent int    `json:"use_percent" example:"45"`
	SizeHuman  string `json:"size_human" example:"128 GB"`
	UsedHuman  string `json:"used_human" example:"57.6 GB"`
	AvailHuman string `json:"avail_human" example:"70.4 GB"`
}

// StorageUsageResponse represents filesystem usage list.
// @Description Filesystem usage information
type StorageUsageResponse struct {
	Filesystems []FilesystemUsage `json:"filesystems"`
}

// SMARTInfo represents SMART health data.
// @Description SMART health data for a storage device
type SMARTInfo struct {
	Device       string                 `json:"device" example:"sda"`
	Type         string                 `json:"type" example:"SSD"`
	Smart        string                 `json:"smart" example:"supported"`
	Health       string                 `json:"health" example:"PASSED"`
	Temperature  int                    `json:"temperature,omitempty" example:"35"`
	PowerOnHours int                    `json:"power_on_hours,omitempty" example:"1234"`
	Attributes   map[string]interface{} `json:"attributes,omitempty"`
}

// ============================================================================
// Storage Device Handlers
// ============================================================================

// GetStorageDevices lists storage devices.
// @Summary List storage devices
// @Description Returns list of all block storage devices
// @Tags Storage
// @Accept json
// @Produce json
// @Success 200 {object} StorageDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /storage/devices [get]
func (h *HALHandler) GetStorageDevices(w http.ResponseWriter, r *http.Request) {
	output, err := execWithTimeout(r.Context(), "lsblk", "-J", "-b", "-o", "NAME,SIZE,TYPE,MODEL,SERIAL,VENDOR,RM,PATH")
	if err != nil {
		log.Printf("GetStorageDevices: lsblk failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("list devices", err))
		return
	}

	var result struct {
		Blockdevices []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Type     string `json:"type"`
			Model    string `json:"model"`
			Serial   string `json:"serial"`
			Vendor   string `json:"vendor"`
			Rm       bool   `json:"rm"`
			Path     string `json:"path"`
			Children []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"children,omitempty"`
		} `json:"blockdevices"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to parse device list")
		return
	}

	var devices []StorageDevice
	for _, dev := range result.Blockdevices {
		if dev.Type != "disk" {
			continue
		}

		device := StorageDevice{
			Name:      dev.Name,
			Path:      dev.Path,
			Size:      dev.Size,
			SizeHuman: formatBytes(dev.Size),
			Type:      dev.Type,
			Model:     strings.TrimSpace(dev.Model),
			Serial:    strings.TrimSpace(dev.Serial),
			Vendor:    strings.TrimSpace(dev.Vendor),
			Removable: dev.Rm,
		}

		// Add partitions
		for _, child := range dev.Children {
			if child.Type == "part" {
				device.Partitions = append(device.Partitions, StorageDevice{
					Name:      child.Name,
					Path:      child.Path,
					Size:      child.Size,
					SizeHuman: formatBytes(child.Size),
					Type:      child.Type,
				})
			}
		}

		devices = append(devices, device)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"devices": devices,
		"count":   len(devices),
	})
}

// GetStorageDevice returns info about a specific device.
// @Summary Get storage device details
// @Description Returns detailed information about a specific storage device
// @Tags Storage
// @Accept json
// @Produce json
// @Param device path string true "Device name" example(mmcblk0)
// @Success 200 {object} StorageDevice
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /storage/device/{device} [get]
func (h *HALHandler) GetStorageDevice(w http.ResponseWriter, r *http.Request) {
	device := chi.URLParam(r, "device")
	if err := validateDeviceName(device); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	devPath := "/dev/" + device
	if _, err := os.Stat(devPath); os.IsNotExist(err) {
		errorResponse(w, http.StatusNotFound, "device not found")
		return
	}

	output, err := execWithTimeout(r.Context(), "lsblk", "-J", "-b", "-o", "NAME,SIZE,TYPE,MODEL,SERIAL,VENDOR,RM,PATH", devPath)
	if err != nil {
		log.Printf("GetStorageDevice: lsblk failed for %s: %v", device, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("get device info", err))
		return
	}

	var result struct {
		Blockdevices []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Type   string `json:"type"`
			Model  string `json:"model"`
			Serial string `json:"serial"`
			Vendor string `json:"vendor"`
			Rm     bool   `json:"rm"`
			Path   string `json:"path"`
		} `json:"blockdevices"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to parse device info")
		return
	}

	if len(result.Blockdevices) == 0 {
		errorResponse(w, http.StatusNotFound, "device not found")
		return
	}

	dev := result.Blockdevices[0]
	jsonResponse(w, http.StatusOK, StorageDevice{
		Name:      dev.Name,
		Path:      dev.Path,
		Size:      dev.Size,
		SizeHuman: formatBytes(dev.Size),
		Type:      dev.Type,
		Model:     strings.TrimSpace(dev.Model),
		Serial:    strings.TrimSpace(dev.Serial),
		Vendor:    strings.TrimSpace(dev.Vendor),
		Removable: dev.Rm,
	})
}

// GetSmartInfo returns SMART data for a device.
// @Summary Get SMART health data
// @Description Returns SMART health information for a storage device
// @Tags Storage
// @Accept json
// @Produce json
// @Param device path string true "Device name" example(sda)
// @Success 200 {object} SMARTInfo
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /storage/smart/{device} [get]
func (h *HALHandler) GetSmartInfo(w http.ResponseWriter, r *http.Request) {
	device := chi.URLParam(r, "device")
	if err := validateDeviceName(device); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	devPath := "/dev/" + device

	// For SD cards, SMART isn't available
	if strings.HasPrefix(device, "mmcblk") {
		jsonResponse(w, http.StatusOK, SMARTInfo{
			Device: device,
			Type:   "SD/eMMC",
			Smart:  "not supported",
			Health: "unknown",
		})
		return
	}

	// Check if device exists
	if _, err := os.Stat(devPath); os.IsNotExist(err) {
		errorResponse(w, http.StatusNotFound, "device not found")
		return
	}

	output, err := execWithTimeout(r.Context(), "smartctl", "-j", "-a", devPath)
	if err != nil {
		// smartctl returns non-zero for various reasons, try to parse anyway
		if output == "" {
			log.Printf("GetSmartInfo: smartctl failed for %s: %v", device, err)
			errorResponse(w, http.StatusInternalServerError, "SMART data unavailable")
			return
		}
	}

	var smartData map[string]interface{}
	if err := json.Unmarshal([]byte(output), &smartData); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to parse SMART data")
		return
	}

	// Build response
	info := SMARTInfo{
		Device:     device,
		Attributes: smartData,
	}

	// Extract key fields
	if smartStatus, ok := smartData["smart_status"].(map[string]interface{}); ok {
		if passed, ok := smartStatus["passed"].(bool); ok {
			if passed {
				info.Health = "PASSED"
			} else {
				info.Health = "FAILED"
			}
		}
	}

	if temp, ok := smartData["temperature"].(map[string]interface{}); ok {
		if current, ok := temp["current"].(float64); ok {
			info.Temperature = int(current)
		}
	}

	if powerOn, ok := smartData["power_on_time"].(map[string]interface{}); ok {
		if hours, ok := powerOn["hours"].(float64); ok {
			info.PowerOnHours = int(hours)
		}
	}

	if device, ok := smartData["device"].(map[string]interface{}); ok {
		if devType, ok := device["type"].(string); ok {
			info.Type = devType
		}
	}

	info.Smart = "supported"

	jsonResponse(w, http.StatusOK, info)
}

// GetStorageUsage returns filesystem usage.
// @Summary Get filesystem usage
// @Description Returns disk usage information for all mounted filesystems
// @Tags Storage
// @Accept json
// @Produce json
// @Success 200 {object} StorageUsageResponse
// @Failure 500 {object} ErrorResponse
// @Router /storage/usage [get]
func (h *HALHandler) GetStorageUsage(w http.ResponseWriter, r *http.Request) {
	output, err := execWithTimeout(r.Context(), "df", "-B1", "--output=target,source,size,used,avail,pcent")
	if err != nil {
		log.Printf("GetStorageUsage: df failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("get disk usage", err))
		return
	}

	showAll := r.URL.Query().Get("all") == "true"

	var filesystems []FilesystemUsage
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // Skip header
		}

		fields := strings.Fields(line)
		if len(fields) >= 6 {
			source := fields[1]
			mountpoint := fields[0]

			// Unless ?all=true, only return real block-device-backed filesystems
			// This filters out: overlay (Docker), tmpfs, devtmpfs, /dev/shm, etc.
			if !showAll {
				if !strings.HasPrefix(source, "/dev/") {
					continue
				}
				// Also skip Docker overlay mounts that happen to show /dev/ source
				if strings.Contains(mountpoint, "/docker/") {
					continue
				}
			}

			size, _ := strconv.ParseInt(fields[2], 10, 64)
			used, _ := strconv.ParseInt(fields[3], 10, 64)
			avail, _ := strconv.ParseInt(fields[4], 10, 64)
			pct := strings.TrimSuffix(fields[5], "%")
			usePct, _ := strconv.Atoi(pct)

			filesystems = append(filesystems, FilesystemUsage{
				Mountpoint: fields[0],
				Filesystem: source,
				Size:       size,
				Used:       used,
				Available:  avail,
				UsePercent: usePct,
				SizeHuman:  formatBytes(size),
				UsedHuman:  formatBytes(used),
				AvailHuman: formatBytes(avail),
			})
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"filesystems": filesystems,
	})
}
