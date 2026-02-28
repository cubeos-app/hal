package handlers

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DetectedInterface represents a network interface with hardware classification.
type DetectedInterface struct {
	Name      string `json:"name"`
	Type      string `json:"type"`       // "wifi" | "ethernet" | "other"
	Bus       string `json:"bus"`        // "sdio" | "pci" | "usb" | "platform" | "virtual" | "unknown"
	BuiltIn   bool   `json:"built_in"`   // true for SDIO/PCI/platform, false for USB
	IsUp      bool   `json:"is_up"`      // operstate == "up"
	MAC       string `json:"mac"`        // MAC address
	Driver    string `json:"driver"`     // Kernel driver name
	APCapable bool   `json:"ap_capable"` // WiFi: supports AP mode
	Role      string `json:"role"`       // "ap" | "uplink" | "unassigned"
	PhyName   string `json:"phy_name,omitempty"`
	VendorID  string `json:"vendor_id,omitempty"`  // USB idVendor (e.g. "0bda")
	ProductID string `json:"product_id,omitempty"` // USB idProduct (e.g. "8812")
}

// InterfaceDetectionResponse is the response for GET /hardware/interfaces.
type InterfaceDetectionResponse struct {
	Interfaces      []DetectedInterface `json:"interfaces"`
	AutoAssigned    bool                `json:"auto_assigned"`
	APInterface     string              `json:"ap_interface"`
	UplinkInterface string              `json:"uplink_interface"`
	Tier            string              `json:"tier"`
}

// DetectInterfaces scans physical network interfaces with bus type and role assignment.
// @Summary Detect network interfaces with hardware classification
// @Description Scans /sys/class/net/ to detect interface type, bus (sdio/pci/usb), AP capability, and auto-assigns roles
// @Tags Hardware
// @Produce json
// @Success 200 {object} InterfaceDetectionResponse
// @Failure 500 {object} ErrorResponse
// @Router /hardware/interfaces [get]
func (h *HALHandler) DetectInterfaces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "Failed to read /sys/class/net: "+err.Error())
		return
	}

	var ifaces []DetectedInterface
	var wifiIfaces []int // indices into ifaces for WiFi interfaces
	var ethIfaces []int  // indices into ifaces for Ethernet interfaces

	for _, entry := range entries {
		name := entry.Name()

		// Skip virtual/container interfaces
		if shouldSkipInterface(name) {
			continue
		}

		iface := DetectedInterface{
			Name: name,
			Role: "unassigned",
		}

		// Detect type: WiFi or Ethernet
		if isWiFiInterface(name) {
			iface.Type = "wifi"
			iface.PhyName = detectPhyName(name)
		} else if hasDeviceDir(name) {
			iface.Type = "ethernet"
		} else if looksLikeEthernet(name) {
			// LXC/container veth pairs appear as eth0 without /sys/class/net/eth0/device
			iface.Type = "ethernet"
		} else {
			iface.Type = "other"
		}

		// Detect bus type
		iface.Bus = detectBusType(name)
		iface.BuiltIn = iface.Bus == "sdio" || iface.Bus == "pci" || iface.Bus == "platform"

		// Read USB device IDs
		if iface.Bus == "usb" {
			iface.VendorID, iface.ProductID = readUSBDeviceID(name)
		}

		// AP capability check: USB WiFi uses two-stage test with whitelist/blacklist,
		// built-in WiFi uses single-stage iw phy check
		if iface.Type == "wifi" && iface.PhyName != "" {
			if iface.Bus == "usb" && iface.VendorID != "" {
				iface.APCapable = checkAndRecordAPCapability(ctx, iface)
			} else {
				iface.APCapable = checkAPSupport(ctx, iface.PhyName)
			}
		}

		// Read operstate
		iface.IsUp = readOperState(name)

		// Read MAC address
		iface.MAC = readSysfsString(name, "address")

		// Detect driver
		iface.Driver = detectDriver(name)

		idx := len(ifaces)
		ifaces = append(ifaces, iface)

		if iface.Type == "wifi" {
			wifiIfaces = append(wifiIfaces, idx)
		} else if iface.Type == "ethernet" {
			ethIfaces = append(ethIfaces, idx)
		}
	}

	// Auto-assign roles
	autoAssigned := assignRoles(ifaces, wifiIfaces, ethIfaces)

	// Build response
	resp := InterfaceDetectionResponse{
		Interfaces:   ifaces,
		AutoAssigned: autoAssigned,
		Tier:         h.tier,
	}

	for i := range ifaces {
		switch ifaces[i].Role {
		case "ap":
			resp.APInterface = ifaces[i].Name
		case "uplink":
			resp.UplinkInterface = ifaces[i].Name
		}
	}

	jsonResponse(w, http.StatusOK, resp)
}

// shouldSkipInterface returns true for virtual/container interfaces.
func shouldSkipInterface(name string) bool {
	switch {
	case name == "lo":
		return true
	case strings.HasPrefix(name, "docker"):
		return true
	case strings.HasPrefix(name, "veth"):
		return true
	case strings.HasPrefix(name, "br-"):
		return true
	case strings.HasPrefix(name, "virbr"):
		return true
	}
	return false
}

// isWiFiInterface checks if the interface is wireless via /sys/class/net/{name}/wireless.
func isWiFiInterface(name string) bool {
	info, err := os.Stat(filepath.Join("/sys/class/net", name, "wireless"))
	return err == nil && info.IsDir()
}

// hasDeviceDir checks if /sys/class/net/{name}/device exists (physical device).
func hasDeviceDir(name string) bool {
	info, err := os.Stat(filepath.Join("/sys/class/net", name, "device"))
	return err == nil && info.IsDir()
}

// looksLikeEthernet returns true if the interface name follows common ethernet naming.
// In LXC/containers, veth pairs appear as eth0 without a /sys/class/net/eth0/device dir.
func looksLikeEthernet(name string) bool {
	return strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "en")
}

// detectBusType reads /sys/class/net/{name}/device/subsystem symlink to determine bus.
func detectBusType(ifaceName string) string {
	subsystemPath := filepath.Join("/sys/class/net", ifaceName, "device", "subsystem")
	target, err := os.Readlink(subsystemPath)
	if err == nil {
		base := filepath.Base(target)
		switch {
		case base == "sdio" || base == "mmc":
			return "sdio"
		case base == "pci" || base == "pcie":
			return "pci"
		case base == "usb":
			return "usb"
		case base == "platform":
			return "platform"
		}
		return base
	}

	// Fallback: read uevent for SUBSYSTEM= line
	ueventPath := filepath.Join("/sys/class/net", ifaceName, "device", "uevent")
	if sub := readUeventKey(ueventPath, "SUBSYSTEM"); sub != "" {
		switch {
		case sub == "sdio" || sub == "mmc":
			return "sdio"
		case sub == "pci" || sub == "pcie":
			return "pci"
		case sub == "usb":
			return "usb"
		case sub == "platform":
			return "platform"
		}
		return sub
	}

	// No device directory means virtual
	if !hasDeviceDir(ifaceName) {
		return "virtual"
	}

	return "unknown"
}

// detectDriver reads the driver name from uevent or module symlink.
func detectDriver(ifaceName string) string {
	// Try uevent DRIVER= line
	ueventPath := filepath.Join("/sys/class/net", ifaceName, "device", "uevent")
	if driver := readUeventKey(ueventPath, "DRIVER"); driver != "" {
		return driver
	}

	// Fallback: readlink device/driver
	driverPath := filepath.Join("/sys/class/net", ifaceName, "device", "driver")
	target, err := os.Readlink(driverPath)
	if err == nil {
		return filepath.Base(target)
	}

	return ""
}

// detectPhyName finds the phy device name for a WiFi interface.
func detectPhyName(ifaceName string) string {
	// Try /sys/class/net/{name}/phy80211/name
	phyNamePath := filepath.Join("/sys/class/net", ifaceName, "phy80211", "name")
	data, err := os.ReadFile(phyNamePath)
	if err == nil {
		return strings.TrimSpace(string(data))
	}

	// Fallback: iterate /sys/class/ieee80211/phy*/device/net/ to find matching interface
	phyDirs, err := filepath.Glob("/sys/class/ieee80211/phy*")
	if err != nil {
		return ""
	}
	for _, phyDir := range phyDirs {
		netDir := filepath.Join(phyDir, "device", "net", ifaceName)
		if info, err := os.Stat(netDir); err == nil && info.IsDir() {
			return filepath.Base(phyDir)
		}
	}

	return ""
}

// checkAPSupport checks if a WiFi phy supports AP mode via iw.
func checkAPSupport(ctx context.Context, phyName string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nsenter", "-t", "1", "-m", "--", "iw", "phy", phyName, "info")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	inModes := false
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "Supported interface modes:") {
			inModes = true
			continue
		}
		if inModes {
			if line == "" || (!strings.HasPrefix(line, "*") && !strings.HasPrefix(line, " ")) {
				break
			}
			if strings.Contains(line, "AP") {
				return true
			}
		}
	}
	return false
}

// assignRoles applies role heuristics and returns whether roles were auto-assigned unambiguously.
func assignRoles(ifaces []DetectedInterface, wifiIdxs, ethIdxs []int) bool {
	apCapableWifi := []int{}
	builtInWifi := []int{}
	usbWifi := []int{}

	for _, idx := range wifiIdxs {
		if ifaces[idx].APCapable {
			apCapableWifi = append(apCapableWifi, idx)
		}
		if ifaces[idx].BuiltIn {
			builtInWifi = append(builtInWifi, idx)
		} else {
			usbWifi = append(usbWifi, idx)
		}
	}

	// No WiFi interfaces at all — only eth_client possible
	if len(wifiIdxs) == 0 {
		if len(ethIdxs) > 0 {
			ifaces[ethIdxs[0]].Role = "uplink"
			return true
		}
		return false
	}

	// Exactly 1 WiFi (AP-capable) + Ethernet → wifi=AP, eth=uplink
	if len(apCapableWifi) == 1 && len(ethIdxs) > 0 {
		ifaces[apCapableWifi[0]].Role = "ap"
		ifaces[ethIdxs[0]].Role = "uplink"
		return true
	}

	// Exactly 1 WiFi (AP-capable), no Ethernet → wifi=AP only (offline hotspot)
	if len(apCapableWifi) == 1 && len(ethIdxs) == 0 {
		ifaces[apCapableWifi[0]].Role = "ap"
		return true
	}

	// 2+ WiFi (built-in + USB) → prefer USB WiFi for AP (better performance)
	if len(wifiIdxs) >= 2 && len(builtInWifi) > 0 && len(usbWifi) > 0 {
		apAssigned := false
		// Prefer AP-capable USB WiFi adapter for AP role
		for _, idx := range usbWifi {
			if ifaces[idx].APCapable {
				ifaces[idx].Role = "ap"
				apAssigned = true
				break
			}
		}
		// Fallback: use built-in WiFi for AP if no USB adapter is capable
		if !apAssigned {
			for _, idx := range builtInWifi {
				if ifaces[idx].APCapable {
					ifaces[idx].Role = "ap"
					apAssigned = true
					break
				}
			}
		}
		// Assign eth as uplink if present
		if len(ethIdxs) > 0 {
			ifaces[ethIdxs[0]].Role = "uplink"
		}
		return apAssigned
	}

	// Ambiguous — mark the best AP candidate but leave unassigned
	return false
}

// readOperState reads the interface operstate.
func readOperState(ifaceName string) bool {
	return readSysfsString(ifaceName, "operstate") == "up"
}

// readSysfsString reads a single sysfs file for an interface.
func readSysfsString(ifaceName, attr string) string {
	data, err := os.ReadFile(filepath.Join("/sys/class/net", ifaceName, attr))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readUeventKey reads a specific key from a uevent file.
func readUeventKey(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	prefix := key + "="
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}
