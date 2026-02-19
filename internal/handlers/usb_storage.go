package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

// ============================================================================
// USB Storage Types
// ============================================================================

// USBStorageDevice represents a USB storage device.
// @Description USB storage device information
type USBStorageDevice struct {
	Path       string `json:"path" example:"/dev/sda1"`
	Name       string `json:"name" example:"sda1"`
	Size       int64  `json:"size"`
	SizeHuman  string `json:"size_human" example:"32 GB"`
	Vendor     string `json:"vendor,omitempty" example:"SanDisk"`
	Model      string `json:"model,omitempty" example:"Cruzer Blade"`
	Serial     string `json:"serial,omitempty"`
	Filesystem string `json:"filesystem,omitempty" example:"exfat"`
	Label      string `json:"label,omitempty" example:"USBDRIVE"`
	Mountpoint string `json:"mountpoint,omitempty" example:"/mnt/usb"`
	Mounted    bool   `json:"mounted" example:"false"`
	Removable  bool   `json:"removable" example:"true"`
}

// USBStorageResponse represents USB storage list.
// @Description List of USB storage devices
type USBStorageResponse struct {
	Count   int                `json:"count" example:"2"`
	Devices []USBStorageDevice `json:"devices"`
}

// USBMountRequest represents USB mount request.
// @Description USB mount parameters
type USBMountRequest struct {
	Device     string `json:"device" example:"/dev/sda1"`
	Mountpoint string `json:"mountpoint,omitempty" example:"/mnt/usb"`
	Options    string `json:"options,omitempty" example:"rw,noatime"`
}

// ============================================================================
// USB Storage Handlers
// ============================================================================

// GetUSBStorageDevices lists USB storage devices.
// @Summary List USB storage devices
// @Description Returns list of connected USB storage devices
// @Tags USB
// @Accept json
// @Produce json
// @Success 200 {object} USBStorageResponse
// @Failure 500 {object} ErrorResponse
// @Router /storage/usb [get]
func (h *HALHandler) GetUSBStorageDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.scanUSBStorage()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// MountUSBStorage mounts a USB device.
// @Summary Mount USB storage
// @Description Mounts a USB storage device to specified mountpoint
// @Tags USB
// @Accept json
// @Produce json
// @Param request body USBMountRequest true "Mount parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /storage/usb/mount [post]
func (h *HALHandler) MountUSBStorage(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB
	var req USBMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate device path
	if err := validateBlockDevice(req.Device); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verify device exists
	if _, err := os.Stat(req.Device); os.IsNotExist(err) {
		errorResponse(w, http.StatusNotFound, "device not found")
		return
	}

	// Verify device is removable via lsblk
	ctx := r.Context()
	checkOut, err := execWithTimeout(ctx, "lsblk", "-ndo", "RM", req.Device)
	if err != nil {
		log.Printf("MountUSBStorage: lsblk check failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to verify device")
		return
	}
	if strings.TrimSpace(checkOut) != "1" {
		errorResponse(w, http.StatusBadRequest, "device is not removable")
		return
	}

	mountpoint := req.Mountpoint
	if mountpoint == "" {
		// Auto-generate mountpoint based on device name
		devName := strings.TrimPrefix(req.Device, "/dev/")
		mountpoint = "/mnt/" + devName
	}

	// Validate mountpoint is under /mnt/ or /media/
	if err := validateMountpoint(mountpoint); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate mount options
	options := "rw,noatime"
	if req.Options != "" {
		if err := validateMountOptions(req.Options); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		options = req.Options
	}

	// Create mountpoint
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		log.Printf("MountUSBStorage: mkdir failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to create mountpoint")
		return
	}

	// Mount with timeout
	out, err := execWithTimeout(ctx, "mount", "-o", options, req.Device, mountpoint)
	if err != nil {
		log.Printf("MountUSBStorage: mount failed: %s: %v", out, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("mount", err))
		return
	}

	successResponse(w, fmt.Sprintf("mounted %s at %s", req.Device, mountpoint))
}

// UnmountUSBStorage unmounts a USB device.
// @Summary Unmount USB storage
// @Description Unmounts a USB storage device
// @Tags USB
// @Accept json
// @Produce json
// @Param request body USBMountRequest true "Unmount parameters (mountpoint or device required)"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /storage/usb/unmount [post]
func (h *HALHandler) UnmountUSBStorage(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB
	var req USBMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	target := req.Mountpoint
	if target == "" {
		target = req.Device
	}
	if target == "" {
		errorResponse(w, http.StatusBadRequest, "mountpoint or device required")
		return
	}

	// If it looks like a mountpoint path, validate it's under /mnt/ or /media/
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "/dev/") {
		if err := validateMountpoint(target); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		// It's a device path — validate it
		if err := validateBlockDevice(target); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	ctx := r.Context()

	// Sync before unmount
	if _, err := execWithTimeout(ctx, "sync"); err != nil {
		log.Printf("UnmountUSBStorage: sync warning: %v", err)
	}

	out, err := execWithTimeout(ctx, "umount", target)
	if err != nil {
		log.Printf("UnmountUSBStorage: umount failed: %s: %v", out, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("unmount", err))
		return
	}

	successResponse(w, fmt.Sprintf("unmounted %s", target))
}

// EjectUSBStorage safely ejects a USB device.
// @Summary Eject USB storage
// @Description Safely ejects a USB storage device (sync + power off)
// @Tags USB
// @Accept json
// @Produce json
// @Param request body USBMountRequest true "Eject parameters (device required)"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /storage/usb/eject [post]
func (h *HALHandler) EjectUSBStorage(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB
	var req USBMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Device == "" {
		errorResponse(w, http.StatusBadRequest, "device required")
		return
	}

	// Validate device name — strip /dev/ prefix if present, validate the base name
	devName := strings.TrimPrefix(req.Device, "/dev/")
	// Get base device (e.g., "sda" from "sda1")
	baseDev := strings.TrimRight(devName, "0123456789")
	if err := validateDeviceName(baseDev); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// Sync first
	if _, err := execWithTimeout(ctx, "sync"); err != nil {
		log.Printf("EjectUSBStorage: sync warning: %v", err)
	}

	// Find all partitions for this device via lsblk (replaces bash -c glob)
	lsblkOut, err := execWithTimeout(ctx, "lsblk", "-nlo", "NAME,MOUNTPOINT", "/dev/"+baseDev)
	if err != nil {
		log.Printf("EjectUSBStorage: lsblk failed: %v", err)
		// Continue anyway — device might not have partitions
	} else {
		// Unmount each mounted partition individually
		for _, line := range strings.Split(strings.TrimSpace(lsblkOut), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] != "" {
				mountpoint := fields[1]
				out, err := execWithTimeout(ctx, "umount", mountpoint)
				if err != nil {
					log.Printf("EjectUSBStorage: umount %s: %s: %v", mountpoint, out, err)
					// Continue — try to unmount others
				}
			}
		}
	}

	// Eject
	out, err := execWithTimeout(ctx, "eject", "/dev/"+baseDev)
	if err != nil {
		log.Printf("EjectUSBStorage: eject failed: %s: %v", out, err)
		// Try udisks2 as fallback
		out2, err2 := execWithTimeout(ctx, "udisksctl", "power-off", "-b", "/dev/"+baseDev)
		if err2 != nil {
			log.Printf("EjectUSBStorage: udisksctl fallback failed: %s: %v", out2, err2)
			errorResponse(w, http.StatusInternalServerError, sanitizeExecError("eject", err))
			return
		}
	}

	successResponse(w, fmt.Sprintf("ejected %s", baseDev))
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) scanUSBStorage() []USBStorageDevice {
	var devices []USBStorageDevice

	// Use lsblk to get block devices with detailed info
	ctx, cancel := context.WithTimeout(context.Background(), defaultExecTimeout)
	defer cancel()
	output, err := execWithTimeout(ctx, "lsblk", "-J", "-b", "-o", "NAME,SIZE,TYPE,VENDOR,MODEL,SERIAL,FSTYPE,LABEL,MOUNTPOINT,RM,TRAN,PATH")
	if err != nil {
		return devices
	}

	var result struct {
		Blockdevices []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			Type       string `json:"type"`
			Vendor     string `json:"vendor"`
			Model      string `json:"model"`
			Serial     string `json:"serial"`
			Fstype     string `json:"fstype"`
			Label      string `json:"label"`
			Mountpoint string `json:"mountpoint"`
			Rm         bool   `json:"rm"`
			Tran       string `json:"tran"`
			Path       string `json:"path"`
			Children   []struct {
				Name       string `json:"name"`
				Size       int64  `json:"size"`
				Type       string `json:"type"`
				Fstype     string `json:"fstype"`
				Label      string `json:"label"`
				Mountpoint string `json:"mountpoint"`
				Path       string `json:"path"`
			} `json:"children,omitempty"`
		} `json:"blockdevices"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return devices
	}

	for _, dev := range result.Blockdevices {
		// Only USB devices
		if dev.Tran != "usb" {
			continue
		}

		// Process partitions
		if len(dev.Children) > 0 {
			for _, part := range dev.Children {
				if part.Type != "part" {
					continue
				}
				devices = append(devices, USBStorageDevice{
					Path:       part.Path,
					Name:       part.Name,
					Size:       part.Size,
					SizeHuman:  formatBytes(part.Size),
					Vendor:     strings.TrimSpace(dev.Vendor),
					Model:      strings.TrimSpace(dev.Model),
					Serial:     strings.TrimSpace(dev.Serial),
					Filesystem: part.Fstype,
					Label:      part.Label,
					Mountpoint: part.Mountpoint,
					Mounted:    part.Mountpoint != "",
					Removable:  dev.Rm,
				})
			}
		} else if dev.Type == "disk" {
			// Whole disk without partitions
			devices = append(devices, USBStorageDevice{
				Path:       dev.Path,
				Name:       dev.Name,
				Size:       dev.Size,
				SizeHuman:  formatBytes(dev.Size),
				Vendor:     strings.TrimSpace(dev.Vendor),
				Model:      strings.TrimSpace(dev.Model),
				Serial:     strings.TrimSpace(dev.Serial),
				Filesystem: dev.Fstype,
				Label:      dev.Label,
				Mountpoint: dev.Mountpoint,
				Mounted:    dev.Mountpoint != "",
				Removable:  dev.Rm,
			})
		}
	}

	return devices
}
