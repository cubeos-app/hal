package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// WiFiStatusResponse contains comprehensive WiFi connection info
type WiFiStatusResponse struct {
	Connected      bool     `json:"connected"`
	SSID           string   `json:"ssid,omitempty"`
	BSSID          string   `json:"bssid,omitempty"`
	Frequency      int      `json:"frequency,omitempty"`
	Channel        int      `json:"channel,omitempty"`
	SignalDBM      int      `json:"signal_dbm,omitempty"`
	SignalPercent  int      `json:"signal_percent,omitempty"`
	Security       string   `json:"security,omitempty"`
	IPAddress      string   `json:"ip_address,omitempty"`
	Netmask        string   `json:"netmask,omitempty"`
	Gateway        string   `json:"gateway,omitempty"`
	DNS            []string `json:"dns,omitempty"`
	Interface      string   `json:"interface"`
	MACAddress     string   `json:"mac_address,omitempty"`
	WiFiGeneration string   `json:"wifi_generation,omitempty"`
	TxBitrate      string   `json:"tx_bitrate,omitempty"`
}

// GetWiFiStatus returns comprehensive WiFi connection status for an interface
// @Summary Get WiFi connection status
// @Description Returns detailed WiFi status including signal, IP, gateway, DNS, channel
// @Tags Network
// @Produce json
// @Param iface path string true "WiFi interface name"
// @Success 200 {object} WiFiStatusResponse
// @Failure 400 {object} ErrorResponse
// @Router /network/wifi/status/{iface} [get]
func (h *HALHandler) GetWiFiStatus(w http.ResponseWriter, r *http.Request) {
	iface := chi.URLParam(r, "iface")
	if iface == "" {
		errorResponse(w, http.StatusBadRequest, "interface is required")
		return
	}
	if err := validateInterfaceName(iface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	status := WiFiStatusResponse{
		Connected: false,
		Interface: iface,
	}

	// Get wpa_cli status (via nsenter for container compat)
	output, err := execWpaCli(r.Context(), "-i", iface, "status")
	if err != nil {
		// Not connected or wpa_supplicant not running
		jsonResponse(w, http.StatusOK, status)
		return
	}

	kv := parseKeyValue(output)

	// Check connection state
	if kv["wpa_state"] != "COMPLETED" {
		jsonResponse(w, http.StatusOK, status)
		return
	}

	status.Connected = true
	status.SSID = kv["ssid"]
	status.BSSID = kv["bssid"]
	status.MACAddress = kv["address"]
	status.Security = kv["key_mgmt"]
	status.WiFiGeneration = kv["wifi_generation"]

	// Parse frequency and derive channel
	if freqStr, ok := kv["freq"]; ok {
		if freq, err := strconv.Atoi(freqStr); err == nil {
			status.Frequency = freq
			status.Channel = freqToChannel(freq)
		}
	}

	// Get signal strength from iw (more reliable than iwconfig in containers)
	iwOutput, err := execWithTimeout(r.Context(), "iw", "dev", iface, "link")
	if err == nil {
		// Parse signal: "signal: -50 dBm"
		for _, line := range strings.Split(iwOutput, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "signal:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					if dbm, err := strconv.Atoi(parts[1]); err == nil {
						status.SignalDBM = dbm
						status.SignalPercent = dbmToPercent(dbm)
					}
				}
			}
			if strings.HasPrefix(line, "tx bitrate:") {
				status.TxBitrate = strings.TrimPrefix(line, "tx bitrate: ")
			}
		}
	}

	// Get IP address and netmask from ip addr
	ipOutput, err := execWithTimeout(r.Context(), "ip", "-4", "addr", "show", "dev", iface)
	if err == nil {
		for _, line := range strings.Split(ipOutput, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "inet ") {
				// "inet 192.168.181.112/24 brd ..."
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					cidr := parts[1] // "192.168.181.112/24"
					slashIdx := strings.Index(cidr, "/")
					if slashIdx > 0 {
						status.IPAddress = cidr[:slashIdx]
						if prefix, err := strconv.Atoi(cidr[slashIdx+1:]); err == nil {
							status.Netmask = prefixToNetmask(prefix)
						}
					} else {
						status.IPAddress = cidr
					}
				}
			}
		}
	}

	// Get default gateway for this interface
	routeOutput, err := execWithTimeout(r.Context(), "ip", "route", "show", "dev", iface, "default")
	if err == nil {
		// "default via 192.168.181.1 proto dhcp ..."
		for _, line := range strings.Split(routeOutput, "\n") {
			if strings.HasPrefix(line, "default via ") {
				parts := strings.Fields(line)
				if len(parts) >= 3 {
					status.Gateway = parts[2]
				}
				break
			}
		}
	}

	// Get DNS servers from resolv.conf
	dnsOutput, err := execWithTimeout(r.Context(), "cat", "/etc/resolv.conf")
	if err == nil {
		for _, line := range strings.Split(dnsOutput, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					status.DNS = append(status.DNS, parts[1])
				}
			}
		}
	}

	jsonResponse(w, http.StatusOK, status)
}

// dbmToPercent converts signal strength in dBm to a percentage (0-100)
// Using the common approximation: -30 dBm = 100%, -90 dBm = 0%
func dbmToPercent(dbm int) int {
	if dbm >= -30 {
		return 100
	}
	if dbm <= -90 {
		return 0
	}
	// Linear interpolation between -90 and -30
	pct := int(float64(dbm+90) * 100.0 / 60.0)
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

// prefixToNetmask converts a CIDR prefix length to a dotted-decimal netmask
func prefixToNetmask(prefix int) string {
	if prefix < 0 || prefix > 32 {
		return ""
	}
	mask := uint32(0xFFFFFFFF) << (32 - prefix)
	return fmt.Sprintf("%d.%d.%d.%d",
		(mask>>24)&0xFF,
		(mask>>16)&0xFF,
		(mask>>8)&0xFF,
		mask&0xFF,
	)
}
