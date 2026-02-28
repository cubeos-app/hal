package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WiFiAPTestResult records the result of an AP capability test for a USB WiFi adapter.
type WiFiAPTestResult struct {
	VendorID  string `json:"vendor_id"`
	ProductID string `json:"product_id"`
	Driver    string `json:"driver,omitempty"`
	Interface string `json:"interface,omitempty"`
	TestedAt  string `json:"tested_at"`
	Result    string `json:"result"`               // "pass" or "fail"
	FailStage string `json:"fail_stage,omitempty"` // "iw_phy" or "hostapd" (only on fail)
}

const (
	whitelistPath = "/cubeos/config/wifi-ap-whitelist.json"
	blacklistPath = "/cubeos/config/wifi-ap-blacklist.json"
)

var apListMu sync.Mutex

// readAPList reads a whitelist or blacklist JSON file.
func readAPList(path string) ([]WiFiAPTestResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []WiFiAPTestResult
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// writeAPList writes a whitelist or blacklist JSON file.
func writeAPList(path string, list []WiFiAPTestResult) error {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// lookupAPList checks if a vendor:product pair is in the given list.
func lookupAPList(list []WiFiAPTestResult, vendorID, productID string) *WiFiAPTestResult {
	for i := range list {
		if list[i].VendorID == vendorID && list[i].ProductID == productID {
			return &list[i]
		}
	}
	return nil
}

// removeFromAPList removes a vendor:product pair from the list, returning the updated list.
func removeFromAPList(list []WiFiAPTestResult, vendorID, productID string) []WiFiAPTestResult {
	result := make([]WiFiAPTestResult, 0, len(list))
	for _, entry := range list {
		if entry.VendorID == vendorID && entry.ProductID == productID {
			continue
		}
		result = append(result, entry)
	}
	return result
}

// readUSBDeviceID reads idVendor and idProduct from sysfs for a network interface.
// Returns empty strings if the interface is not a USB device.
func readUSBDeviceID(ifaceName string) (vendorID, productID string) {
	devicePath := filepath.Join("/sys/class/net", ifaceName, "device")

	// Walk up from the device path to find the USB device directory with idVendor
	// The path is typically: /sys/class/net/wlan1/device -> /sys/devices/.../usb1/1-1/1-1.1:1.0
	// idVendor is at the parent USB device level: /sys/devices/.../usb1/1-1/1-1.1/idVendor
	realPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", ""
	}

	// Walk up directories looking for idVendor file
	path := realPath
	for i := 0; i < 5; i++ {
		vendorPath := filepath.Join(path, "idVendor")
		productPath := filepath.Join(path, "idProduct")
		vData, vErr := os.ReadFile(vendorPath)
		pData, pErr := os.ReadFile(productPath)
		if vErr == nil && pErr == nil {
			return strings.TrimSpace(string(vData)), strings.TrimSpace(string(pData))
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return "", ""
}

// testAPCapabilityStage2 runs a brief hostapd start/stop test to confirm AP works in practice.
// Returns nil on success, error on failure.
func testAPCapabilityStage2(ctx context.Context, ifaceName string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Create a minimal temporary hostapd config
	tmpConf, err := os.CreateTemp("", "hostapd-test-*.conf")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmpConf.Name()
	defer os.Remove(tmpPath)

	conf := fmt.Sprintf(`interface=%s
driver=nl80211
ssid=cubeos-ap-test
hw_mode=g
channel=1
`, ifaceName)
	if _, err := tmpConf.WriteString(conf); err != nil {
		tmpConf.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	tmpConf.Close()

	// Start hostapd in foreground mode (-d for debug) with a short timeout
	// We use nsenter to run in the host namespace
	cmd := exec.CommandContext(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"hostapd", "-dd", tmpPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start hostapd: %w", err)
	}

	// Wait briefly for hostapd to either fail or start successfully
	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case err := <-doneCh:
		// hostapd exited quickly — this usually means failure
		if err != nil {
			return fmt.Errorf("hostapd failed: %w", err)
		}
		// Clean exit within 2s is unusual but acceptable
		return nil
	case <-time.After(3 * time.Second):
		// hostapd stayed alive for 3s — AP mode works
		_ = cmd.Process.Kill()
		<-doneCh // wait for goroutine to finish
		return nil
	}
}

// checkAndRecordAPCapability runs the two-stage AP test and records the result.
// Returns true if the adapter is AP-capable.
func checkAndRecordAPCapability(ctx context.Context, iface DetectedInterface) bool {
	vendorID, productID := readUSBDeviceID(iface.Name)
	if vendorID == "" || productID == "" {
		// Not a USB device or can't read IDs — fall back to stage 1 only
		return checkAPSupport(ctx, iface.PhyName)
	}

	apListMu.Lock()
	defer apListMu.Unlock()

	// Check whitelist
	whitelist, _ := readAPList(whitelistPath)
	if entry := lookupAPList(whitelist, vendorID, productID); entry != nil {
		log.Printf("[DEBUG] USB WiFi %s:%s found in whitelist — skipping test", vendorID, productID)
		return true
	}

	// Check blacklist
	blacklist, _ := readAPList(blacklistPath)
	if entry := lookupAPList(blacklist, vendorID, productID); entry != nil {
		log.Printf("[DEBUG] USB WiFi %s:%s found in blacklist — skipping test", vendorID, productID)
		return false
	}

	// Unknown adapter — run two-stage test
	log.Printf("[INFO] Testing USB WiFi %s:%s (%s) AP capability", vendorID, productID, iface.Name)

	result := WiFiAPTestResult{
		VendorID:  vendorID,
		ProductID: productID,
		Driver:    iface.Driver,
		Interface: iface.Name,
		TestedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	// Stage 1: iw phy AP mode check
	if !checkAPSupport(ctx, iface.PhyName) {
		result.Result = "fail"
		result.FailStage = "iw_phy"
		blacklist = append(blacklist, result)
		if err := writeAPList(blacklistPath, blacklist); err != nil {
			log.Printf("[WARN] Failed to write AP blacklist: %v", err)
		}
		log.Printf("[INFO] USB WiFi %s:%s failed stage 1 (iw phy) — blacklisted", vendorID, productID)
		return false
	}

	// Stage 2: brief hostapd start/stop
	if err := testAPCapabilityStage2(ctx, iface.Name); err != nil {
		result.Result = "fail"
		result.FailStage = "hostapd"
		blacklist = append(blacklist, result)
		if err := writeAPList(blacklistPath, blacklist); err != nil {
			log.Printf("[WARN] Failed to write AP blacklist: %v", err)
		}
		log.Printf("[INFO] USB WiFi %s:%s failed stage 2 (hostapd): %v — blacklisted", vendorID, productID, err)
		return false
	}

	// Both stages passed — whitelist
	result.Result = "pass"
	whitelist = append(whitelist, result)
	if err := writeAPList(whitelistPath, whitelist); err != nil {
		log.Printf("[WARN] Failed to write AP whitelist: %v", err)
	}
	log.Printf("[INFO] USB WiFi %s:%s passed both AP stages — whitelisted", vendorID, productID)
	return true
}

// GetWiFiAPWhitelist returns the current AP whitelist.
// @Summary Get WiFi AP whitelist
// @Description Returns the list of USB WiFi adapters that have passed AP capability testing
// @Tags Hardware
// @Produce json
// @Success 200 {array} WiFiAPTestResult
// @Router /hardware/wifi-ap/whitelist [get]
func (h *HALHandler) GetWiFiAPWhitelist(w http.ResponseWriter, r *http.Request) {
	apListMu.Lock()
	whitelist, err := readAPList(whitelistPath)
	apListMu.Unlock()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "Failed to read whitelist: "+err.Error())
		return
	}
	if whitelist == nil {
		whitelist = []WiFiAPTestResult{}
	}
	jsonResponse(w, http.StatusOK, whitelist)
}

// GetWiFiAPBlacklist returns the current AP blacklist.
// @Summary Get WiFi AP blacklist
// @Description Returns the list of USB WiFi adapters that have failed AP capability testing
// @Tags Hardware
// @Produce json
// @Success 200 {array} WiFiAPTestResult
// @Router /hardware/wifi-ap/blacklist [get]
func (h *HALHandler) GetWiFiAPBlacklist(w http.ResponseWriter, r *http.Request) {
	apListMu.Lock()
	blacklist, err := readAPList(blacklistPath)
	apListMu.Unlock()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "Failed to read blacklist: "+err.Error())
		return
	}
	if blacklist == nil {
		blacklist = []WiFiAPTestResult{}
	}
	jsonResponse(w, http.StatusOK, blacklist)
}

// RetestWiFiAdapter removes an adapter from the blacklist and re-runs the AP capability test.
// @Summary Re-test a blacklisted WiFi adapter
// @Description Removes the adapter from the blacklist and runs the two-stage AP capability test again
// @Tags Hardware
// @Produce json
// @Param device_id path string true "Vendor:Product ID (e.g. 0bda:8812)"
// @Success 200 {object} WiFiAPTestResult
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /hardware/wifi-ap/retest/{device_id} [post]
func (h *HALHandler) RetestWiFiAdapter(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/hardware/wifi-ap/retest/")
	parts := strings.SplitN(deviceID, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		errorResponse(w, http.StatusBadRequest, "Invalid device_id format, expected vendor:product (e.g. 0bda:8812)")
		return
	}
	vendorID, productID := parts[0], parts[1]

	// Remove from blacklist
	apListMu.Lock()
	blacklist, _ := readAPList(blacklistPath)
	blacklist = removeFromAPList(blacklist, vendorID, productID)
	_ = writeAPList(blacklistPath, blacklist)
	// Also remove from whitelist in case it's there
	whitelist, _ := readAPList(whitelistPath)
	whitelist = removeFromAPList(whitelist, vendorID, productID)
	_ = writeAPList(whitelistPath, whitelist)
	apListMu.Unlock()

	// Find the matching interface
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "Failed to scan interfaces: "+err.Error())
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !isWiFiInterface(name) {
			continue
		}
		vid, pid := readUSBDeviceID(name)
		if vid == vendorID && pid == productID {
			// Found the interface — run two-stage test
			iface := DetectedInterface{
				Name:    name,
				PhyName: detectPhyName(name),
				Driver:  detectDriver(name),
			}

			capable := checkAndRecordAPCapability(r.Context(), iface)

			result := WiFiAPTestResult{
				VendorID:  vendorID,
				ProductID: productID,
				Driver:    iface.Driver,
				Interface: name,
				TestedAt:  time.Now().UTC().Format(time.RFC3339),
			}
			if capable {
				result.Result = "pass"
			} else {
				result.Result = "fail"
			}
			jsonResponse(w, http.StatusOK, result)
			return
		}
	}

	errorResponse(w, http.StatusNotFound, fmt.Sprintf("No connected WiFi adapter with ID %s:%s found", vendorID, productID))
}
