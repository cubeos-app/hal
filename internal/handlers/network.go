package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// hostapdCLILoggedOnce suppresses repeated hostapd_cli failure logs.
var hostapdCLILoggedOnce atomic.Bool

// getDefaultInternetCheckIP returns the IP used for internet connectivity checks.
func getDefaultInternetCheckIP() string {
	if ip := os.Getenv("HAL_INTERNET_CHECK_IP"); ip != "" {
		return ip
	}
	return "8.8.8.8"
}

// getDefaultWiFiInterface returns the default WiFi interface name.
func getDefaultWiFiInterface() string {
	if iface := os.Getenv("HAL_DEFAULT_WIFI_INTERFACE"); iface != "" {
		return iface
	}
	return "wlan0"
}

// execWpaCli runs wpa_cli via nsenter to work around the reply-socket problem.
// When HAL runs in a container, wpa_cli creates its reply socket in the container's
// /tmp, but wpa_supplicant (on the host) tries to reply to the host's /tmp — causing
// a timeout. nsenter -t 1 -m executes wpa_cli in the host's mount namespace so both
// sockets share the same /tmp.
func execWpaCli(ctx context.Context, args ...string) (string, error) {
	nsenterArgs := []string{"-t", "1", "-m", "--", "wpa_cli"}
	nsenterArgs = append(nsenterArgs, args...)
	return execWithTimeout(ctx, "nsenter", nsenterArgs...)
}

// ListInterfaces returns all network interfaces
// @Summary List network interfaces
// @Description Returns all network interfaces with their status
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /network/interfaces [get]
func (h *HALHandler) ListInterfaces(w http.ResponseWriter, r *http.Request) {
	output, err := execWithTimeout(r.Context(), "ip", "-j", "addr")
	if err != nil {
		log.Printf("ListInterfaces: ip -j addr failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to get interfaces")
		return
	}

	// Parse raw ip -j addr output
	var rawInterfaces []struct {
		IfName   string   `json:"ifname"`
		Flags    []string `json:"flags"`
		MTU      int      `json:"mtu"`
		Address  string   `json:"address"`
		LinkType string   `json:"link_type"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}

	if err := json.Unmarshal([]byte(output), &rawInterfaces); err != nil {
		log.Printf("ListInterfaces: parse error: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to parse interfaces")
		return
	}

	// Transform to structured format
	type structuredInterface struct {
		Name          string   `json:"name"`
		IsUp          bool     `json:"is_up"`
		MACAddress    string   `json:"mac_address"`
		IPv4Addresses []string `json:"ipv4_addresses"`
		IPv6Addresses []string `json:"ipv6_addresses"`
		MTU           int      `json:"mtu"`
		IsWireless    bool     `json:"is_wireless"`
	}

	var interfaces []structuredInterface
	for _, raw := range rawInterfaces {
		iface := structuredInterface{
			Name:       raw.IfName,
			MACAddress: raw.Address,
			MTU:        raw.MTU,
		}

		// Check if UP from flags
		for _, flag := range raw.Flags {
			if flag == "UP" {
				iface.IsUp = true
				break
			}
		}

		// Extract IPv4 and IPv6 addresses from addr_info
		for _, addr := range raw.AddrInfo {
			addrStr := fmt.Sprintf("%s/%d", addr.Local, addr.PrefixLen)
			switch addr.Family {
			case "inet":
				iface.IPv4Addresses = append(iface.IPv4Addresses, addrStr)
			case "inet6":
				iface.IPv6Addresses = append(iface.IPv6Addresses, addrStr)
			}
		}

		// Ensure non-nil slices for JSON
		if iface.IPv4Addresses == nil {
			iface.IPv4Addresses = []string{}
		}
		if iface.IPv6Addresses == nil {
			iface.IPv6Addresses = []string{}
		}

		// Detect wireless interfaces
		if strings.HasPrefix(raw.IfName, "wlan") || strings.HasPrefix(raw.IfName, "wlx") || strings.HasPrefix(raw.IfName, "wlp") {
			iface.IsWireless = true
		}

		interfaces = append(interfaces, iface)
	}

	if interfaces == nil {
		interfaces = []structuredInterface{}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"interfaces": interfaces,
	})
}

// GetInterface returns a specific network interface
// @Summary Get network interface details
// @Description Returns details for a specific network interface
// @Tags Network
// @Produce json
// @Param name path string true "Interface name"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/interface/{name} [get]
func (h *HALHandler) GetInterface(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateInterfaceName(name); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	output, err := execWithTimeout(r.Context(), "ip", "-j", "addr", "show", name)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "interface not found")
		return
	}

	var iface []interface{}
	if err := json.Unmarshal([]byte(output), &iface); err != nil {
		log.Printf("GetInterface(%s): parse error: %v", name, err)
		errorResponse(w, http.StatusInternalServerError, "failed to parse interface")
		return
	}

	if len(iface) > 0 {
		jsonResponse(w, http.StatusOK, iface[0])
	} else {
		jsonResponse(w, http.StatusOK, map[string]interface{}{})
	}
}

// GetInterfaceTraffic returns traffic statistics for a specific interface
// @Summary Get interface traffic statistics
// @Description Returns TX/RX bytes for a specific interface from sysfs
// @Tags Network
// @Produce json
// @Param name path string true "Interface name"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /network/interface/{name}/traffic [get]
func (h *HALHandler) GetInterfaceTraffic(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateInterfaceName(name); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// validateInterfaceName() blocks path traversal characters (no / or ..)
	basePath := fmt.Sprintf("/sys/class/net/%s/statistics", name)

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		errorResponse(w, http.StatusNotFound, "interface not found")
		return
	}

	rxBytes, _ := readFileString(basePath + "/rx_bytes")
	txBytes, _ := readFileString(basePath + "/tx_bytes")
	rxPackets, _ := readFileString(basePath + "/rx_packets")
	txPackets, _ := readFileString(basePath + "/tx_packets")
	rxErrors, _ := readFileString(basePath + "/rx_errors")
	txErrors, _ := readFileString(basePath + "/tx_errors")
	rxDropped, _ := readFileString(basePath + "/rx_dropped")
	txDropped, _ := readFileString(basePath + "/tx_dropped")

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"interface":  name,
		"rx_bytes":   rxBytes,
		"tx_bytes":   txBytes,
		"rx_packets": rxPackets,
		"tx_packets": txPackets,
		"rx_errors":  rxErrors,
		"tx_errors":  txErrors,
		"rx_dropped": rxDropped,
		"tx_dropped": txDropped,
	})
}

// GetTrafficStats returns network traffic statistics for all interfaces
// @Summary Get network traffic statistics
// @Description Returns TX/RX bytes for all interfaces from /proc/net/dev
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /network/traffic [get]
func (h *HALHandler) GetTrafficStats(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		log.Printf("GetTrafficStats: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to read network statistics")
		return
	}

	interfaces := make(map[string]map[string]int64)
	lines := strings.Split(string(data), "\n")

	for _, line := range lines[2:] { // Skip header lines
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		iface := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])

		if len(fields) >= 12 {
			rxBytes, _ := strconv.ParseInt(fields[0], 10, 64)
			rxPackets, _ := strconv.ParseInt(fields[1], 10, 64)
			rxErrors, _ := strconv.ParseInt(fields[2], 10, 64)
			rxDropped, _ := strconv.ParseInt(fields[3], 10, 64)
			txBytes, _ := strconv.ParseInt(fields[8], 10, 64)
			txPackets, _ := strconv.ParseInt(fields[9], 10, 64)
			txErrors, _ := strconv.ParseInt(fields[10], 10, 64)
			txDropped, _ := strconv.ParseInt(fields[11], 10, 64)

			interfaces[iface] = map[string]int64{
				"rx_bytes":   rxBytes,
				"rx_packets": rxPackets,
				"rx_errors":  rxErrors,
				"rx_dropped": rxDropped,
				"tx_bytes":   txBytes,
				"tx_packets": txPackets,
				"tx_errors":  txErrors,
				"tx_dropped": txDropped,
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"interfaces": interfaces,
		"source":     "/proc/net/dev",
	})
}

// BringInterfaceUp brings an interface up
// @Summary Bring interface up
// @Description Brings a network interface up using ip link set
// @Tags Network
// @Produce json
// @Param name path string true "Interface name"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/interface/{name}/up [post]
func (h *HALHandler) BringInterfaceUp(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateInterfaceName(name); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "ip", "link", "set", name, "up")
	if err != nil {
		log.Printf("BringInterfaceUp(%s): %v", name, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("bring interface up", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"interface": name,
		"state":     "up",
	})
}

// BringInterfaceDown brings an interface down
// @Summary Bring interface down
// @Description Brings a network interface down using ip link set
// @Tags Network
// @Produce json
// @Param name path string true "Interface name"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/interface/{name}/down [post]
func (h *HALHandler) BringInterfaceDown(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validateInterfaceName(name); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "ip", "link", "set", name, "down")
	if err != nil {
		log.Printf("BringInterfaceDown(%s): %v", name, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("bring interface down", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"interface": name,
		"state":     "down",
	})
}

// GetNetworkStatus returns overall network status
// @Summary Get network status
// @Description Returns overall network connectivity status including default route and internet check
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /network/status [get]
func (h *HALHandler) GetNetworkStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"connected": false,
		"mode":      "unknown",
	}

	// Check default route
	output, err := execWithTimeout(r.Context(), "ip", "route", "show", "default")
	if err == nil && len(output) > 0 {
		status["connected"] = true
		status["default_route"] = strings.TrimSpace(output)
	}

	// Check internet connectivity and measure RTT
	checkIP := getDefaultInternetCheckIP()
	pingOutput, err := execWithTimeout(r.Context(), "ping", "-c", "1", "-W", "2", checkIP)
	status["internet"] = err == nil
	status["check_target"] = checkIP

	// Parse RTT from ping output: "time=1.23 ms"
	if err == nil && len(pingOutput) > 0 {
		if idx := strings.Index(pingOutput, "time="); idx >= 0 {
			rttStr := pingOutput[idx+5:]
			if spaceIdx := strings.IndexByte(rttStr, ' '); spaceIdx >= 0 {
				rttStr = rttStr[:spaceIdx]
			}
			if rtt, parseErr := strconv.ParseFloat(rttStr, 64); parseErr == nil {
				status["rtt_ms"] = rtt
			}
		}
	}

	// Determine mode based on interfaces
	if _, err := os.Stat("/sys/class/net/eth0"); err == nil {
		carrier, _ := os.ReadFile("/sys/class/net/eth0/carrier")
		if strings.TrimSpace(string(carrier)) == "1" {
			status["mode"] = "ethernet"
		}
	}

	jsonResponse(w, http.StatusOK, status)
}

// NetworkModeResponse represents the current CubeOS network mode.
// @Description Current network operating mode
type NetworkModeResponse struct {
	Mode        string `json:"mode" example:"wifi_router"`
	Description string `json:"description" example:"Access point with internet via Ethernet"`
	APActive    bool   `json:"ap_active" example:"true"`
	EthUp       bool   `json:"eth_up" example:"true"`
	WifiClient  bool   `json:"wifi_client" example:"false"`
	Internet    bool   `json:"internet" example:"true"`
}

// GetNetworkMode returns the current CubeOS network operating mode.
// @Summary Get network mode
// @Description Returns current mode: offline_hotspot, wifi_router, or wifi_bridge
// @Tags Network
// @Produce json
// @Success 200 {object} NetworkModeResponse
// @Router /network/mode [get]
func (h *HALHandler) GetNetworkMode(w http.ResponseWriter, r *http.Request) {
	resp := NetworkModeResponse{
		Mode:        "offline_hotspot",
		Description: "Access point only, air-gapped",
	}

	// Check if AP is active (hostapd running)
	if svcOutput, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "--", "systemctl", "is-active", "hostapd"); err == nil {
		resp.APActive = strings.TrimSpace(svcOutput) == "active"
	}

	// Check Ethernet link
	if _, err := os.Stat("/sys/class/net/eth0"); err == nil {
		carrier, _ := os.ReadFile("/sys/class/net/eth0/carrier")
		resp.EthUp = strings.TrimSpace(string(carrier)) == "1"
	}

	// Check WiFi client connection (wlan1 or usb0 used as uplink)
	for _, iface := range []string{"wlan1", "usb0"} {
		if _, err := os.Stat("/sys/class/net/" + iface); err == nil {
			operstate, _ := os.ReadFile("/sys/class/net/" + iface + "/operstate")
			if strings.TrimSpace(string(operstate)) == "up" {
				resp.WifiClient = true
				break
			}
		}
	}

	// B84: Also check enx* interfaces (Android USB tethering with predictable names)
	if !resp.WifiClient {
		entries, _ := os.ReadDir("/sys/class/net")
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "enx") {
				operstate, _ := os.ReadFile("/sys/class/net/" + e.Name() + "/operstate")
				if strings.TrimSpace(string(operstate)) == "up" {
					resp.WifiClient = true
					break
				}
			}
		}
	}

	// Check internet connectivity
	checkIP := getDefaultInternetCheckIP()
	_, err := execWithTimeout(r.Context(), "ping", "-c", "1", "-W", "2", checkIP)
	resp.Internet = err == nil

	// Determine mode — AP state matters for distinguishing router vs client modes
	if resp.APActive && resp.EthUp && resp.Internet {
		resp.Mode = "wifi_router"
		resp.Description = "Access point with internet via Ethernet"
	} else if resp.APActive && resp.WifiClient && resp.Internet {
		resp.Mode = "wifi_bridge"
		resp.Description = "Access point with internet via WiFi station"
	} else if !resp.APActive && resp.EthUp && resp.Internet {
		resp.Mode = "eth_client"
		resp.Description = "Ethernet client, no access point"
	} else if !resp.APActive && resp.WifiClient && resp.Internet {
		resp.Mode = "wifi_client"
		resp.Description = "WiFi client, no access point"
	} else if resp.APActive {
		resp.Mode = "offline_hotspot"
		resp.Description = "Access point only, air-gapped"
	} else {
		resp.Mode = "offline_hotspot"
		resp.Description = "No network connectivity"
	}

	jsonResponse(w, http.StatusOK, resp)
}

// ScanWiFi scans for WiFi networks
// @Summary Scan WiFi networks
// @Description Scans for available WiFi networks on specified interface using iw
// @Tags Network
// @Produce json
// @Param iface path string true "WiFi interface name"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/wifi/scan/{iface} [get]
func (h *HALHandler) ScanWiFi(w http.ResponseWriter, r *http.Request) {
	iface := chi.URLParam(r, "iface")
	if iface == "" {
		iface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(iface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	output, err := execWithTimeout(r.Context(), "iw", iface, "scan")
	if err != nil {
		log.Printf("ScanWiFi(%s): %v", iface, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("WiFi scan", err))
		return
	}

	networks := parseWifiScan(output)

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"interface": iface,
		"networks":  networks,
		"count":     len(networks),
	})
}

// parseWifiScan parses iw scan output into structured data
func parseWifiScan(output string) []map[string]interface{} {
	var networks []map[string]interface{}
	var current map[string]interface{}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "BSS ") {
			if current != nil {
				networks = append(networks, current)
			}
			current = make(map[string]interface{})
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				bssid := strings.TrimSuffix(parts[1], "(on")
				current["bssid"] = bssid
			}
			current["security"] = "Open"
		} else if current != nil {
			if strings.HasPrefix(trimmed, "SSID:") {
				current["ssid"] = strings.TrimSpace(strings.TrimPrefix(trimmed, "SSID:"))
			} else if strings.HasPrefix(trimmed, "signal:") {
				sigStr := strings.TrimPrefix(trimmed, "signal: ")
				sigStr = strings.TrimSuffix(sigStr, " dBm")
				sigStr = strings.TrimSpace(sigStr)
				if v, err := strconv.ParseFloat(sigStr, 64); err == nil {
					current["signal"] = int(v)
				}
			} else if strings.HasPrefix(trimmed, "freq:") {
				freqStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "freq:"))
				if v, err := strconv.Atoi(freqStr); err == nil {
					current["frequency"] = v
					// Derive channel from frequency
					current["channel"] = freqToChannel(v)
				}
			} else if strings.Contains(trimmed, "WPA") || strings.Contains(trimmed, "RSN") {
				if strings.Contains(trimmed, "RSN") {
					current["security"] = "WPA2"
				} else if strings.Contains(trimmed, "WPA") {
					// Don't downgrade from WPA2
					if current["security"] != "WPA2" {
						current["security"] = "WPA"
					}
				}
			} else if strings.Contains(trimmed, "WEP") {
				if current["security"] == "Open" {
					current["security"] = "WEP"
				}
			}
		}
	}

	if current != nil {
		networks = append(networks, current)
	}

	return networks
}

// freqToChannel converts WiFi frequency in MHz to channel number
func freqToChannel(freq int) int {
	switch {
	case freq >= 2412 && freq <= 2484:
		if freq == 2484 {
			return 14
		}
		return (freq - 2407) / 5
	case freq >= 5170 && freq <= 5825:
		return (freq - 5000) / 5
	case freq >= 5955 && freq <= 7115:
		return (freq - 5950) / 5
	default:
		return 0
	}
}

// ensureWpaSupplicant starts wpa_supplicant on the given interface if it's not
// already running. Required before any wpa_cli commands can succeed.
// On Ubuntu 24.04 with networkd, wpa_supplicant is disabled at boot to avoid
// conflicting with hostapd on wlan0. It only starts automatically when networkd
// processes a netplan wifis: section. If ConnectWiFi is called before netplan is
// configured (e.g., from the simple /network/wifi/connect endpoint), wpa_cli would
// fail with "add_network" error. This function ensures wpa_supplicant is running
// on the specified interface before proceeding.
// Uses nsenter to run in the host's mount+network namespace.
func ensureWpaSupplicant(ctx context.Context, iface string) error {
	// Quick check: try wpa_cli status — if it works, wpa_supplicant is already running
	_, err := execWpaCli(ctx, "-i", iface, "status")
	if err == nil {
		return nil // Already running
	}

	log.Printf("ensureWpaSupplicant(%s): wpa_supplicant not running, starting it", iface)

	// Ensure a minimal wpa_supplicant config exists for this interface.
	// The config needs ctrl_interface for wpa_cli to connect, and update_config=1
	// so wpa_cli save_config can persist networks.
	confPath := fmt.Sprintf("/etc/wpa_supplicant/wpa_supplicant-%s.conf", iface)
	checkCmd := fmt.Sprintf(`test -f %s || cat > %s << 'EOF'
ctrl_interface=DIR=/var/run/wpa_supplicant GROUP=netdev
update_config=1
EOF`, confPath, confPath)

	_, err = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "--", "bash", "-c", checkCmd)
	if err != nil {
		log.Printf("ensureWpaSupplicant(%s): failed to create per-interface config: %v, trying generic", iface, err)
		// Fall back to generic config
		genericConf := "/etc/wpa_supplicant/wpa_supplicant.conf"
		checkGeneric := fmt.Sprintf(`test -f %s || cat > %s << 'EOF'
ctrl_interface=DIR=/var/run/wpa_supplicant GROUP=netdev
update_config=1
EOF`, genericConf, genericConf)
		_, _ = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "--", "bash", "-c", checkGeneric)
		confPath = genericConf
	}

	// Start wpa_supplicant in daemon mode on the host
	_, err = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"wpa_supplicant", "-B", "-D", "nl80211", "-i", iface, "-c", confPath)
	if err != nil {
		return fmt.Errorf("failed to start wpa_supplicant on %s: %w", iface, err)
	}

	// Give it a moment to create the control socket
	time.Sleep(500 * time.Millisecond)

	// Verify it's running
	_, err = execWpaCli(ctx, "-i", iface, "status")
	if err != nil {
		return fmt.Errorf("wpa_supplicant started but control socket not ready on %s", iface)
	}

	log.Printf("ensureWpaSupplicant(%s): started successfully", iface)
	return nil
}

// ConnectWiFi connects to a WiFi network
// @Summary Connect to WiFi
// @Description Connects to a WiFi network using wpa_supplicant
// @Tags Network
// @Accept json
// @Produce json
// @Param request body object true "WiFi connection request" example({"ssid":"MyNetwork","password":"secret","interface":"wlan0"})
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/wifi/connect [post]
func (h *HALHandler) ConnectWiFi(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB

	var req struct {
		SSID      string `json:"ssid"`
		Password  string `json:"password"`
		Interface string `json:"interface"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate SSID
	if err := validateSSID(req.SSID); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate password if provided
	if req.Password != "" {
		if err := validateWiFiPassword(req.Password); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if req.Interface == "" {
		req.Interface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(req.Interface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// B126: Ensure wpa_supplicant is running on this interface before any wpa_cli
	// commands. On Ubuntu 24.04, wpa_supplicant is disabled at boot to avoid
	// conflicting with hostapd on wlan0. For USB dongle interfaces (wlan1), it
	// must be started explicitly if netplan hasn't already triggered it.
	if err := ensureWpaSupplicant(r.Context(), req.Interface); err != nil {
		log.Printf("ConnectWiFi: %v", err)
		errorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("wpa_supplicant not available on %s — is the WiFi adapter ready?", req.Interface))
		return
	}

	// Check if this SSID already exists as a saved network
	existingID := ""
	listOutput, listErr := execWpaCli(r.Context(), "-i", req.Interface, "list_networks")
	if listErr == nil {
		for _, line := range strings.Split(listOutput, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] != "network" {
				if _, err := strconv.Atoi(fields[0]); err == nil && fields[1] == req.SSID {
					existingID = fields[0]
					break
				}
			}
		}
	}

	var networkID string

	if existingID != "" && req.Password == "" {
		// Saved network, no new password → just reconnect using stored credentials
		networkID = existingID
		log.Printf("ConnectWiFi: reusing saved network %s (id=%s)", req.SSID, networkID)
	} else {
		// New network or password update: remove only the old entry for this SSID (not all),
		// then create a fresh one with the provided credentials
		if existingID != "" {
			_, _ = execWpaCli(r.Context(), "-i", req.Interface, "remove_network", existingID)
		}

		output, err := execWpaCli(r.Context(), "-i", req.Interface, "add_network")
		if err != nil {
			log.Printf("ConnectWiFi: add_network on %s failed: %v (output: %s)", req.Interface, err, output)
			errorResponse(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to add network on %s — wpa_supplicant may not be running", req.Interface))
			return
		}
		networkID = strings.TrimSpace(output)

		// Set SSID
		_, err = execWpaCli(r.Context(), "-i", req.Interface, "set_network", networkID, "ssid", fmt.Sprintf("\"%s\"", req.SSID))
		if err != nil {
			log.Printf("ConnectWiFi: set_network ssid: %v", err)
			errorResponse(w, http.StatusInternalServerError, "failed to set SSID")
			return
		}

		// Set password or open network
		if req.Password != "" {
			_, err = execWpaCli(r.Context(), "-i", req.Interface, "set_network", networkID, "psk", fmt.Sprintf("\"%s\"", req.Password))
			if err != nil {
				log.Printf("ConnectWiFi: set_network psk: %v", err)
				errorResponse(w, http.StatusInternalServerError, "failed to set password")
				return
			}
		} else {
			_, err = execWpaCli(r.Context(), "-i", req.Interface, "set_network", networkID, "key_mgmt", "NONE")
			if err != nil {
				log.Printf("ConnectWiFi: set_network key_mgmt: %v", err)
				errorResponse(w, http.StatusInternalServerError, "failed to set key management")
				return
			}
		}
	}

	// Select network — forces connection to THIS network, disabling all others
	_, err := execWpaCli(r.Context(), "-i", req.Interface, "select_network", networkID)
	if err != nil {
		log.Printf("ConnectWiFi: select_network: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to select network")
		return
	}

	// Save config (best effort)
	_, _ = execWpaCli(r.Context(), "-i", req.Interface, "save_config")

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":    true,
		"ssid":       req.SSID,
		"interface":  req.Interface,
		"network_id": networkID,
	})
}

// DisconnectWiFi disconnects from current WiFi
// @Summary Disconnect WiFi
// @Description Disconnects from current WiFi network using wpa_cli
// @Tags Network
// @Produce json
// @Param iface path string true "WiFi interface name"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/wifi/disconnect/{iface} [post]
func (h *HALHandler) DisconnectWiFi(w http.ResponseWriter, r *http.Request) {
	iface := chi.URLParam(r, "iface")
	if iface == "" {
		iface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(iface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWpaCli(r.Context(), "-i", iface, "disconnect")
	if err != nil {
		log.Printf("DisconnectWiFi(%s): %v", iface, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("WiFi disconnect", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"interface": iface,
	})
}

// GetAPStatus returns access point status
// @Summary Get AP status
// @Description Returns structured access point status from hostapd_cli with hostapd.conf fallback
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/status [get]
func (h *HALHandler) GetAPStatus(w http.ResponseWriter, r *http.Request) {
	result := map[string]interface{}{
		"active":    false,
		"ssid":      "",
		"channel":   0,
		"interface": "wlan0",
		"frequency": 0,
		"bssid":     "",
		"clients":   0,
	}

	// Try hostapd_cli status first
	output, err := hostapdCLI(r.Context(), "status")
	if err == nil {
		raw := parseKeyValue(output)
		// hostapd_cli uses keys like ssid[0], bssid[0] — normalize them
		if v, ok := raw["state"]; ok {
			result["active"] = (v == "ENABLED" || v == "COUNTRY_UPDATE" || v == "HT_SCAN" || v == "DFS")
			result["state"] = v
		}
		if v, ok := raw["ssid[0]"]; ok {
			result["ssid"] = v
		} else if v, ok := raw["ssid"]; ok {
			result["ssid"] = v
		}
		if v, ok := raw["channel"]; ok {
			if ch, err := strconv.Atoi(v); err == nil {
				result["channel"] = ch
			}
		}
		if v, ok := raw["freq"]; ok {
			if freq, err := strconv.Atoi(v); err == nil {
				result["frequency"] = freq
			}
		}
		if v, ok := raw["bssid[0]"]; ok {
			result["bssid"] = v
		} else if v, ok := raw["bssid"]; ok {
			result["bssid"] = v
		}
		if v, ok := raw["num_sta[0]"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				result["clients"] = n
			}
		}
		// Detect interface from "Selected interface" line
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "Selected interface") {
				// "Selected interface 'wlan0'"
				parts := strings.SplitN(line, "'", 3)
				if len(parts) >= 2 {
					result["interface"] = parts[1]
				}
			}
		}
	} else {
		// B7 fix: Log once, then silently fall back to config file
		if !hostapdCLILoggedOnce.Swap(true) {
			log.Printf("GetAPStatus: hostapd_cli failed: %v, falling back to config file (suppressing future logs)", err)
		}
	}

	// If ssid is still empty, fall back to hostapd.conf
	if result["ssid"] == "" {
		result["ssid"], result["channel"], result["interface"] = parseHostapdConf()
	}

	// Check if hostapd service is active (use nsenter since Alpine has no systemctl)
	if !result["active"].(bool) {
		svcOutput, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "--", "systemctl", "is-active", "hostapd")
		if err == nil && strings.TrimSpace(svcOutput) == "active" {
			result["active"] = true
		}
	}

	jsonResponse(w, http.StatusOK, result)
}

// parseKeyValue parses key=value output (used by hostapd_cli, wpa_cli, etc.)
func parseKeyValue(output string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return m
}

// parseHostapdConf reads SSID, channel, and interface from hostapd.conf as fallback
func parseHostapdConf() (ssid string, channel int, iface string) {
	ssid = "CubeOS"
	iface = "wlan0"

	confPaths := []string{
		"/etc/hostapd/hostapd.conf",
		"/etc/hostapd.conf",
	}

	for _, path := range confPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			switch key {
			case "ssid":
				ssid = val
			case "channel":
				if ch, err := strconv.Atoi(val); err == nil {
					channel = ch
				}
			case "interface":
				iface = val
			}
		}
		break // Use first found config
	}
	return
}

// hostapdCLI runs hostapd_cli via nsenter to use the host's binary.
// Alpine's hostapd_cli times out on the control socket (version mismatch with host hostapd),
// so we skip it entirely and go straight to nsenter which is instant.
// -i wlan0 ensures hostapd_cli finds the correct control socket.
func hostapdCLI(ctx context.Context, args ...string) (string, error) {
	cliArgs := append([]string{"-i", "wlan0"}, args...)
	nsArgs := append([]string{"-t", "1", "-m", "--", "hostapd_cli"}, cliArgs...)
	return execWithTimeout(ctx, "nsenter", nsArgs...)
}

// GetAPClients returns connected AP clients
// @Summary Get AP clients
// @Description Returns list of clients connected to the access point, enriched with DHCP lease data
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/clients [get]
func (h *HALHandler) GetAPClients(w http.ResponseWriter, r *http.Request) {
	// Load DHCP leases for IP/hostname enrichment
	leases := loadDHCPLeases()

	var clients []map[string]interface{}

	// Try hostapd_cli all_sta
	output, err := hostapdCLI(r.Context(), "all_sta")
	if err == nil {
		var currentMAC string
		var current map[string]interface{}

		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			// MAC address line starts a new client (17 chars, 5 colons)
			if len(line) == 17 && strings.Count(line, ":") == 5 {
				if current != nil {
					clients = append(clients, current)
				}
				currentMAC = strings.ToUpper(line)
				current = map[string]interface{}{
					"mac_address": currentMAC,
					"ip_address":  "",
					"hostname":    "",
				}
				// Enrich from DHCP leases
				if lease, ok := leases[strings.ToLower(line)]; ok {
					current["ip_address"] = lease.ip
					current["hostname"] = lease.hostname
				}
			} else if current != nil {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					val := strings.TrimSpace(parts[1])
					switch key {
					case "connected_time":
						if v, err := strconv.ParseInt(val, 10, 64); err == nil {
							current["connected_time"] = v
						}
					case "signal":
						// hostapd reports signal in dBm, may have trailing text
						sig := strings.Fields(val)
						if len(sig) > 0 {
							if v, err := strconv.Atoi(sig[0]); err == nil {
								current["signal"] = v
							}
						}
					case "rx_bytes":
						if v, err := strconv.ParseInt(val, 10, 64); err == nil {
							current["rx_bytes"] = v
						}
					case "tx_bytes":
						if v, err := strconv.ParseInt(val, 10, 64); err == nil {
							current["tx_bytes"] = v
						}
					}
				}
			}
		}
		if current != nil {
			clients = append(clients, current)
		}
	} else {
		// B7 fix: Log once, then silently fall back to DHCP leases
		if !hostapdCLILoggedOnce.Swap(true) {
			log.Printf("GetAPClients: hostapd_cli failed: %v, falling back to DHCP leases (suppressing future logs)", err)
		}

		// B119: Cross-reference DHCP leases with ARP table to exclude
		// disconnected clients. DHCP leases persist for hours, but ARP
		// entries for disconnected WiFi clients transition to FAILED within
		// seconds. Only include leases with REACHABLE/STALE/DELAY ARP state.
		reachableMACs := getReachableARPMacs(r.Context(), "wlan0")

		for _, lease := range leases {
			if strings.HasPrefix(lease.ip, "10.42.24.") && lease.ip != "10.42.24.1" {
				// Only include if ARP says this client is still reachable
				if _, reachable := reachableMACs[strings.ToLower(lease.mac)]; !reachable {
					continue
				}
				clients = append(clients, map[string]interface{}{
					"mac_address": strings.ToUpper(lease.mac),
					"ip_address":  lease.ip,
					"hostname":    lease.hostname,
					"source":      "dhcp_lease",
				})
			}
		}
	}

	if clients == nil {
		clients = []map[string]interface{}{}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"clients": clients,
		"count":   len(clients),
	})
}

// dhcpLease holds parsed DHCP lease data
type dhcpLease struct {
	mac      string
	ip       string
	hostname string
	expiry   string
}

// loadDHCPLeases reads DHCP lease files and returns a map keyed by lowercase MAC
func loadDHCPLeases() map[string]dhcpLease {
	leases := make(map[string]dhcpLease)

	leasePaths := []string{
		"/var/lib/misc/dnsmasq.leases",
		"/tmp/dnsmasq.leases",
		"/etc/pihole/dhcp.leases",
		"/var/lib/dhcp/dhcpd.leases",
		// Pi-hole in Docker paths
		"/cubeos/coreapps/pihole/appdata/etc-dnsmasq.d/dhcp.leases",
		"/cubeos/coreapps/pihole/appdata/etc-pihole/dhcp.leases",
	}

	for _, path := range leasePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// dnsmasq lease format: <expiry> <mac> <ip> <hostname> <client-id>
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				mac := strings.ToLower(fields[1])
				leases[mac] = dhcpLease{
					mac:      mac,
					ip:       fields[2],
					hostname: fields[3],
					expiry:   fields[0],
				}
			}
		}
		if len(leases) > 0 {
			break // Use first file that has data
		}
	}

	return leases
}

// getReachableARPMacs returns a set of lowercase MAC addresses that have
// REACHABLE, STALE, or DELAY ARP state on the given interface.
// Clients that have disconnected from WiFi transition to FAILED within seconds,
// so this effectively filters out stale DHCP leases from disconnected clients.
func getReachableARPMacs(ctx context.Context, iface string) map[string]bool {
	macs := make(map[string]bool)

	// "ip -4 neigh show dev wlan0" output format (IPv4 only):
	//   10.42.24.123 lladdr 28:a0:6b:9d:e9:28 REACHABLE
	//   10.42.24.124 FAILED
	// Note: -4 is critical — without it, IPv6 link-local entries (fe80::...)
	// may have STALE state with the client's MAC, causing disconnected clients
	// to appear reachable even when their IPv4 entry shows FAILED.
	output, err := execWithTimeout(ctx, "ip", "-4", "neigh", "show", "dev", iface)
	if err != nil {
		return macs
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Skip entries in FAILED or INCOMPLETE state — these are disconnected
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "FAILED") || strings.Contains(upper, "INCOMPLETE") {
			continue
		}

		// Extract MAC from "lladdr" field
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "lladdr" && i+1 < len(fields) {
				mac := strings.ToLower(fields[i+1])
				if len(mac) == 17 && strings.Count(mac, ":") == 5 {
					macs[mac] = true
				}
				break
			}
		}
	}

	return macs
}

// DisconnectAPClient disconnects a client from the AP
// @Summary Disconnect AP client
// @Description Disconnects a client from the access point by MAC address using hostapd_cli
// @Tags Network
// @Accept json
// @Produce json
// @Param request body object true "Disconnect request" example({"mac":"AA:BB:CC:DD:EE:FF"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/disconnect [post]
func (h *HALHandler) DisconnectAPClient(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB

	var req struct {
		MAC string `json:"mac"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := validateMACAddress(req.MAC); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := hostapdCLI(r.Context(), "disassociate", req.MAC)
	if err != nil {
		log.Printf("DisconnectAPClient(%s): %v", req.MAC, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("disconnect AP client", err))
		return
	}

	successResponse(w, fmt.Sprintf("client %s disconnected", req.MAC))
}

// BlockAPClient blocks a client from the AP
// @Summary Block AP client
// @Description Blocks a client from connecting to the access point using hostapd_cli deny_acl
// @Tags Network
// @Accept json
// @Produce json
// @Param request body object true "Block request" example({"mac":"AA:BB:CC:DD:EE:FF"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/block [post]
func (h *HALHandler) BlockAPClient(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB

	var req struct {
		MAC string `json:"mac"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := validateMACAddress(req.MAC); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// First disconnect, then deny
	_, _ = hostapdCLI(r.Context(), "disassociate", req.MAC)

	_, err := hostapdCLI(r.Context(), "deny_acl", "ADD_MAC", req.MAC)
	if err != nil {
		log.Printf("BlockAPClient(%s): %v", req.MAC, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("block AP client", err))
		return
	}

	successResponse(w, fmt.Sprintf("client %s blocked", req.MAC))
}

// UnblockAPClient removes a MAC address from the AP blocklist
// @Summary Unblock AP client
// @Description Removes a MAC address from the access point deny list using hostapd_cli
// @Tags Network
// @Produce json
// @Param mac path string true "MAC address to unblock"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/unblock/{mac} [post]
func (h *HALHandler) UnblockAPClient(w http.ResponseWriter, r *http.Request) {
	mac := chi.URLParam(r, "mac")
	if err := validateMACAddress(mac); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := hostapdCLI(r.Context(), "deny_acl", "DEL_MAC", mac)
	if err != nil {
		log.Printf("UnblockAPClient(%s): %v", mac, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("unblock AP client", err))
		return
	}

	successResponse(w, fmt.Sprintf("client %s unblocked", mac))
}

// APBlocklistResponse represents the list of blocked MAC addresses
type APBlocklistResponse struct {
	MACs []string `json:"macs"`
}

// GetAPBlocklist returns the list of blocked MAC addresses from hostapd deny ACL
// @Summary Get AP blocklist
// @Description Returns the list of MAC addresses blocked from the access point
// @Tags Network
// @Produce json
// @Success 200 {object} APBlocklistResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/blocklist [get]
func (h *HALHandler) GetAPBlocklist(w http.ResponseWriter, r *http.Request) {
	output, err := hostapdCLI(r.Context(), "deny_acl", "SHOW")
	if err != nil {
		// If hostapd isn't running, return empty list (not an error)
		jsonResponse(w, http.StatusOK, APBlocklistResponse{MACs: []string{}})
		return
	}

	macs := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		mac := strings.TrimSpace(line)
		if mac != "" && validateMACAddress(mac) == nil {
			macs = append(macs, strings.ToUpper(mac))
		}
	}

	jsonResponse(w, http.StatusOK, APBlocklistResponse{MACs: macs})
}

// RequestDHCP requests a DHCP lease on an interface
// @Summary Request DHCP lease
// @Description Requests a DHCP lease on the specified network interface using dhclient
// @Tags Network
// @Accept json
// @Produce json
// @Param request body object true "DHCP request" example({"interface":"eth0"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/dhcp/request [post]
func (h *HALHandler) RequestDHCP(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)

	var req struct {
		Interface string `json:"interface"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Interface == "" {
		errorResponse(w, http.StatusBadRequest, "interface is required")
		return
	}
	if err := validateInterfaceName(req.Interface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Release existing lease first (best effort) — via nsenter for host namespace
	_, _ = execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--", "dhclient", "-r", req.Interface)

	// Request new lease — try dhclient first, fall back to dhcpcd
	// Must run in host namespace to properly configure host interfaces
	_, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--", "dhclient", req.Interface)
	if err != nil {
		// dhclient not available, try dhcpcd
		log.Printf("RequestDHCP(%s): dhclient failed (%v), trying dhcpcd", req.Interface, err)
		_, err = execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--", "dhcpcd", req.Interface)
	}
	if err != nil {
		log.Printf("RequestDHCP(%s): %v", req.Interface, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("DHCP request", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"interface": req.Interface,
		"message":   "DHCP lease requested",
	})
}

// SetStaticIP sets a static IP address on an interface
// @Summary Set static IP
// @Description Sets a static IP address on the specified network interface using ip addr
// @Tags Network
// @Accept json
// @Produce json
// @Param request body object true "Static IP config" example({"interface":"eth0","ip":"10.42.24.100/24","gateway":"10.42.24.1"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/ip/static [post]
func (h *HALHandler) SetStaticIP(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)

	var req struct {
		Interface string `json:"interface"`
		IP        string `json:"ip"`
		Gateway   string `json:"gateway"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Interface == "" || req.IP == "" {
		errorResponse(w, http.StatusBadRequest, "interface and ip are required")
		return
	}
	if err := validateInterfaceName(req.Interface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCIDROrIP(req.IP); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid IP: "+err.Error())
		return
	}
	if req.Gateway != "" {
		if err := validateCIDROrIP(req.Gateway); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid gateway: "+err.Error())
			return
		}
	}

	// Flush existing addresses on interface
	_, _ = execWithTimeout(r.Context(), "ip", "addr", "flush", "dev", req.Interface)

	// Add static IP
	_, err := execWithTimeout(r.Context(), "ip", "addr", "add", req.IP, "dev", req.Interface)
	if err != nil {
		log.Printf("SetStaticIP(%s, %s): addr add: %v", req.Interface, req.IP, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("set static IP", err))
		return
	}

	// Set default gateway if provided
	if req.Gateway != "" {
		// Remove existing default route via this interface (best effort)
		_, _ = execWithTimeout(r.Context(), "ip", "route", "del", "default", "dev", req.Interface)

		_, err := execWithTimeout(r.Context(), "ip", "route", "add", "default", "via", req.Gateway, "dev", req.Interface)
		if err != nil {
			log.Printf("SetStaticIP(%s): route add default via %s: %v", req.Interface, req.Gateway, err)
			// Don't fail — IP is already set, just warn about gateway
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"success":   true,
				"interface": req.Interface,
				"ip":        req.IP,
				"warning":   "IP set but gateway route failed",
			})
			return
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"interface": req.Interface,
		"ip":        req.IP,
		"gateway":   req.Gateway,
		"message":   "static IP configured",
	})
}

// WriteNetplan writes netplan YAML to the host filesystem and applies it.
// Uses nsenter to access the host's /etc/netplan/ directory.
// Optionally reconfigures a specific interface after writing.
// @Summary Write netplan configuration
// @Description Writes netplan YAML to host and applies via netplan apply
// @Tags network
// @Accept json
// @Produce json
// @Param request body object true "yaml: netplan content, reconfigure_iface: optional interface to reconfigure"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /network/netplan [post]
func (h *HALHandler) WriteNetplan(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB max

	var req struct {
		YAML             string `json:"yaml"`
		ReconfigureIface string `json:"reconfigure_iface,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.YAML == "" {
		errorResponse(w, http.StatusBadRequest, "yaml content is required")
		return
	}

	// Validate YAML starts with expected header (basic sanity check)
	if !strings.Contains(req.YAML, "network:") || !strings.Contains(req.YAML, "version: 2") {
		errorResponse(w, http.StatusBadRequest, "invalid netplan YAML: must contain 'network:' and 'version: 2'")
		return
	}

	// Validate reconfigure_iface if provided
	if req.ReconfigureIface != "" {
		if err := validateInterfaceName(req.ReconfigureIface); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid interface name: "+err.Error())
			return
		}
	}

	// Write netplan YAML to host via nsenter
	// Using bash -c with heredoc to write multi-line content safely
	writeCmd := fmt.Sprintf("cat > /etc/netplan/01-cubeos.yaml << 'CUBEOS_NETPLAN_EOF'\n%sCUBEOS_NETPLAN_EOF", req.YAML)

	_, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "--",
		"bash", "-c", writeCmd)
	if err != nil {
		log.Printf("WriteNetplan: failed to write: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("write netplan", err))
		return
	}

	// Apply netplan: generates networkd configs, reloads networkd, AND manages
	// wpa_supplicant lifecycle for wifis: sections. Previously this used a 3-step
	// process (netplan generate → networkctl reload → networkctl reconfigure) which
	// updated networkd but never started wpa_supplicant, causing WiFi connections
	// to fail with NO-CARRIER on wlan1. netplan apply handles everything including
	// starting wpa_supplicant for WiFi interfaces. (B126)
	_, err = execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"netplan", "apply")
	if err != nil {
		log.Printf("WriteNetplan: netplan apply failed: %v", err)
		// Non-fatal — netplan is written, will take effect on reboot
	}

	// Optionally reconfigure a specific interface (e.g., after netplan apply,
	// give networkd an extra nudge for the target interface)
	if req.ReconfigureIface != "" {
		time.Sleep(time.Second) // Let netplan apply settle
		_, err = execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
			"networkctl", "reconfigure", req.ReconfigureIface)
		if err != nil {
			log.Printf("WriteNetplan: reconfigure %s failed: %v", req.ReconfigureIface, err)
			// Non-fatal — netplan is applied
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"message":      "netplan written and applied",
		"reloaded":     true,
		"reconfigured": req.ReconfigureIface,
	})
}

// ---------------------------------------------------------------------------
// Station mode endpoints (wifi_client) — Phase 6b
// ---------------------------------------------------------------------------

// StopHostapd stops the WiFi access point and releases wlan0 for client use.
// @Summary Stop WiFi access point
// @Description Stops hostapd, flushes wlan0 IP, and verifies the interface is released for station use
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /network/hostapd/stop [post]
func (h *HALHandler) StopHostapd(w http.ResponseWriter, r *http.Request) {
	iface := getDefaultWiFiInterface()

	// 1. Stop hostapd via systemctl
	_, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"systemctl", "stop", "hostapd")
	if err != nil {
		log.Printf("StopHostapd: systemctl stop failed: %v", err)
		// Non-fatal — hostapd may already be stopped
	}

	// 2. Wait for clean shutdown
	time.Sleep(time.Second)

	// 3. Flush IP from wlan0
	_, _ = execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"ip", "addr", "flush", "dev", iface)

	// 4. Verify hostapd is not running
	output, _ := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"systemctl", "is-active", "hostapd")
	active := strings.TrimSpace(output) == "active"
	if active {
		log.Printf("StopHostapd: hostapd still active after stop")
		errorResponse(w, http.StatusInternalServerError, "hostapd still running after stop attempt")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"stopped":   true,
		"interface": iface,
	})
}

// StationConnectRequest is the request body for ConnectStation.
type StationConnectRequest struct {
	SSID           string `json:"ssid"`
	Password       string `json:"password"`
	Interface      string `json:"interface"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// StationConnectResponse is the response from ConnectStation.
type StationConnectResponse struct {
	Connected bool   `json:"connected"`
	IP        string `json:"ip,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Interface string `json:"interface"`
}

// ConnectStation connects wlan0 as a WiFi client with timeout.
// @Summary Connect wlan0 as WiFi station
// @Description Ensures wpa_supplicant is running, connects to SSID, writes netplan, waits for IP within timeout
// @Tags Network
// @Accept json
// @Produce json
// @Param request body StationConnectRequest true "Station connection parameters"
// @Success 200 {object} StationConnectResponse
// @Failure 400 {object} ErrorResponse
// @Failure 408 {object} ErrorResponse "Connection timeout"
// @Failure 500 {object} ErrorResponse
// @Router /network/station/connect [post]
func (h *HALHandler) ConnectStation(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)

	var req StationConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate SSID
	if err := validateSSID(req.SSID); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate password if provided
	if req.Password != "" {
		if err := validateWiFiPassword(req.Password); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Default interface
	if req.Interface == "" {
		req.Interface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(req.Interface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Default and max timeout
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 30
	}
	if req.TimeoutSeconds > 120 {
		req.TimeoutSeconds = 120
	}

	ctx := r.Context()

	// 1. Ensure wpa_supplicant is running
	if err := ensureWpaSupplicant(ctx, req.Interface); err != nil {
		log.Printf("ConnectStation: wpa_supplicant setup failed: %v", err)
		errorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("wpa_supplicant not available on %s", req.Interface))
		return
	}

	// 2. Connect via wpa_cli — check if SSID already exists
	existingID := ""
	listOutput, listErr := execWpaCli(ctx, "-i", req.Interface, "list_networks")
	if listErr == nil {
		for _, line := range strings.Split(listOutput, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == req.SSID {
				existingID = fields[0]
				break
			}
		}
	}

	var networkID string
	if existingID != "" && req.Password == "" {
		networkID = existingID
	} else {
		if existingID != "" {
			_, _ = execWpaCli(ctx, "-i", req.Interface, "remove_network", existingID)
		}
		output, err := execWpaCli(ctx, "-i", req.Interface, "add_network")
		if err != nil {
			log.Printf("ConnectStation: add_network failed: %v", err)
			errorResponse(w, http.StatusInternalServerError, "failed to add network")
			return
		}
		networkID = strings.TrimSpace(output)

		// Set SSID
		_, err = execWpaCli(ctx, "-i", req.Interface, "set_network",
			networkID, "ssid", fmt.Sprintf("\"%s\"", req.SSID))
		if err != nil {
			log.Printf("ConnectStation: set ssid failed: %v", err)
			errorResponse(w, http.StatusInternalServerError, "failed to configure SSID")
			return
		}

		// Set password or open
		if req.Password != "" {
			_, err = execWpaCli(ctx, "-i", req.Interface, "set_network",
				networkID, "psk", fmt.Sprintf("\"%s\"", req.Password))
			if err != nil {
				log.Printf("ConnectStation: set psk failed: %v", err)
				errorResponse(w, http.StatusInternalServerError, "failed to configure password")
				return
			}
		} else {
			_, err = execWpaCli(ctx, "-i", req.Interface, "set_network",
				networkID, "key_mgmt", "NONE")
			if err != nil {
				log.Printf("ConnectStation: set key_mgmt failed: %v", err)
			}
		}
	}

	// Select network (forces connection)
	_, err := execWpaCli(ctx, "-i", req.Interface, "select_network", networkID)
	if err != nil {
		log.Printf("ConnectStation: select_network failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to select network")
		return
	}

	// Save config (best effort)
	_, _ = execWpaCli(ctx, "-i", req.Interface, "save_config")

	// 3. Write netplan with wifis: section so networkd handles DHCP
	netplanYAML := fmt.Sprintf("network:\n  version: 2\n  renderer: networkd\n  wifis:\n    %s:\n      dhcp4: true\n      optional: true\n      access-points:\n        \"%s\":\n          password: \"%s\"\n",
		req.Interface, req.SSID, req.Password)

	writeCmd := fmt.Sprintf("cat > /etc/netplan/01-cubeos.yaml << 'CUBEOS_NETPLAN_EOF'\n%sCUBEOS_NETPLAN_EOF", netplanYAML)
	_, writeErr := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "--",
		"bash", "-c", writeCmd)
	if writeErr != nil {
		log.Printf("ConnectStation: netplan write failed: %v", writeErr)
	}

	// Apply netplan
	_, _ = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"netplan", "apply")

	// 4. Poll for IP on interface every 2s until timeout
	deadline := time.Now().Add(time.Duration(req.TimeoutSeconds) * time.Second)
	var ip, gateway string
	for time.Now().Before(deadline) {
		ip, gateway = getInterfaceIPAndGateway(ctx, req.Interface)
		if ip != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}

	if ip == "" {
		log.Printf("ConnectStation: timeout waiting for IP on %s", req.Interface)
		errorResponse(w, http.StatusRequestTimeout,
			fmt.Sprintf("timeout waiting for IP on %s after %ds", req.Interface, req.TimeoutSeconds))
		return
	}

	jsonResponse(w, http.StatusOK, StationConnectResponse{
		Connected: true,
		IP:        ip,
		Gateway:   gateway,
		Interface: req.Interface,
	})
}

// getInterfaceIPAndGateway extracts IPv4 address and default gateway for an interface.
func getInterfaceIPAndGateway(ctx context.Context, iface string) (string, string) {
	// Get IP
	ipOutput, err := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"ip", "-4", "-o", "addr", "show", iface)
	if err != nil {
		return "", ""
	}
	var ip string
	for _, line := range strings.Split(ipOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		for i, p := range parts {
			if p == "inet" && i+1 < len(parts) {
				addr := parts[i+1]
				if idx := strings.Index(addr, "/"); idx >= 0 {
					addr = addr[:idx]
				}
				ip = addr
				break
			}
		}
		if ip != "" {
			break
		}
	}

	// Get gateway
	var gateway string
	gwOutput, gwErr := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"ip", "-4", "route", "show", "default")
	if gwErr == nil {
		for _, line := range strings.Split(gwOutput, "\n") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "via" && i+1 < len(fields) {
					gateway = fields[i+1]
					break
				}
			}
			if gateway != "" {
				break
			}
		}
	}

	return ip, gateway
}

// StationVerifyResponse is the response from VerifyStation.
type StationVerifyResponse struct {
	Connected bool   `json:"connected"`
	IP        string `json:"ip,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Internet  bool   `json:"internet"`
	Interface string `json:"interface"`
}

// VerifyStation checks if wlan0 station mode is working.
// @Summary Verify WiFi station connectivity
// @Description Checks if wlan0 has an IP address and can reach the gateway
// @Tags Network
// @Produce json
// @Param interface query string false "Interface name" default(wlan0)
// @Success 200 {object} StationVerifyResponse
// @Router /network/station/verify [get]
func (h *HALHandler) VerifyStation(w http.ResponseWriter, r *http.Request) {
	iface := r.URL.Query().Get("interface")
	if iface == "" {
		iface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(iface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// 1. Check if interface has an IPv4 address
	ip, gateway := getInterfaceIPAndGateway(ctx, iface)
	if ip == "" {
		jsonResponse(w, http.StatusOK, StationVerifyResponse{
			Connected: false,
			Internet:  false,
			Interface: iface,
		})
		return
	}

	// 2. Ping gateway (1 packet, 2s timeout)
	gatewayReachable := false
	if gateway != "" {
		_, err := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
			"ping", "-c", "1", "-W", "2", gateway)
		gatewayReachable = err == nil
	}

	// 3. Ping internet check IP
	internetReachable := false
	checkIP := getDefaultInternetCheckIP()
	_, err := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"ping", "-c", "1", "-W", "3", checkIP)
	internetReachable = err == nil

	jsonResponse(w, http.StatusOK, StationVerifyResponse{
		Connected: gatewayReachable || ip != "",
		IP:        ip,
		Gateway:   gateway,
		Internet:  internetReachable,
		Interface: iface,
	})
}

// RevertToAP reverts from station mode to access point mode.
// @Summary Revert to Access Point mode
// @Description Stops wpa_supplicant on wlan0, restores AP netplan, starts hostapd
// @Tags Network
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /network/ap/revert [post]
func (h *HALHandler) RevertToAP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	iface := getDefaultWiFiInterface()
	gatewayIP := os.Getenv("CUBEOS_GATEWAY_IP")
	if gatewayIP == "" {
		gatewayIP = "10.42.24.1"
	}

	// 1. Disconnect wpa_supplicant on wlan0
	_, _ = execWpaCli(ctx, "-i", iface, "disconnect")
	// Stop wpa_supplicant service
	_, _ = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"systemctl", "stop", "wpa_supplicant")

	// 2. Flush wlan0 IP
	_, _ = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"ip", "addr", "flush", "dev", iface)

	// 3. Restore AP netplan (offline_hotspot: wlan0 under ethernets with static IP)
	apNetplan := fmt.Sprintf("network:\n  version: 2\n  renderer: networkd\n  ethernets:\n    eth0:\n      dhcp4: true\n      optional: true\n    %s:\n      addresses:\n        - %s/24\n      optional: true\n",
		iface, gatewayIP)

	writeCmd := fmt.Sprintf("cat > /etc/netplan/01-cubeos.yaml << 'CUBEOS_NETPLAN_EOF'\n%sCUBEOS_NETPLAN_EOF", apNetplan)
	_, err := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "--",
		"bash", "-c", writeCmd)
	if err != nil {
		log.Printf("RevertToAP: netplan write failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("write AP netplan", err))
		return
	}

	// 4. Apply netplan
	_, err = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"netplan", "apply")
	if err != nil {
		log.Printf("RevertToAP: netplan apply failed: %v", err)
		// Non-fatal — continue to start hostapd
	}

	// 5. Start hostapd
	_, err = execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"systemctl", "start", "hostapd")
	if err != nil {
		log.Printf("RevertToAP: hostapd start failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("start hostapd", err))
		return
	}

	// 6. Wait and verify hostapd is active
	time.Sleep(2 * time.Second)
	output, _ := execWithTimeout(ctx, "nsenter", "-t", "1", "-m", "-n", "--",
		"systemctl", "is-active", "hostapd")
	apActive := strings.TrimSpace(output) == "active"

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"reverted":  true,
		"mode":      "offline_hotspot",
		"ap_active": apActive,
		"interface": iface,
	})
}
