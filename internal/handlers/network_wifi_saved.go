package handlers

import (
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// SavedNetwork represents a saved WiFi network
type SavedNetwork struct {
	SSID     string `json:"ssid"`
	Security string `json:"security,omitempty"`
	AutoJoin bool   `json:"auto_join"`
}

// GetSavedWiFiNetworks returns list of saved WiFi networks
// @Summary List saved WiFi networks
// @Description Returns all saved WiFi network configurations
// @Tags Network
// @Produce json
// @Param interface query string false "WiFi interface name"
// @Success 200 {object} map[string]interface{} "networks array with count"
// @Failure 400 {object} ErrorResponse "Invalid interface name"
// @Failure 500 {object} ErrorResponse "Failed to list networks"
// @Router /network/wifi/saved [get]
func (h *HALHandler) GetSavedWiFiNetworks(w http.ResponseWriter, r *http.Request) {
	iface := r.URL.Query().Get("interface")
	if iface == "" {
		iface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(iface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	var networks []SavedNetwork

	// Try NetworkManager first (nmcli)
	output, err := execWithTimeout(r.Context(), "nmcli", "-t", "-f", "NAME,TYPE,AUTOCONNECT", "connection", "show")
	if err == nil {
		lines := strings.Split(strings.TrimSpace(output), "\n")
		for _, line := range lines {
			parts := strings.Split(line, ":")
			if len(parts) >= 3 && parts[1] == "802-11-wireless" {
				autoJoin := parts[2] == "yes"
				networks = append(networks, SavedNetwork{
					SSID:     parts[0],
					AutoJoin: autoJoin,
				})
			}
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"networks": networks,
			"count":    len(networks),
			"source":   "NetworkManager",
		})
		return
	}

	// Try wpa_cli (via nsenter for container compatibility)
	output, err = execWpaCli(r.Context(), "-i", iface, "list_networks")
	if err == nil {
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			// Skip header line (network id / ssid / bssid / flags)
			if len(fields) >= 2 && fields[0] != "network" {
				// Try to parse network ID - if it's not a number, skip
				if _, err := strconv.Atoi(fields[0]); err != nil {
					continue
				}
				networks = append(networks, SavedNetwork{
					SSID:     fields[1],
					AutoJoin: true, // wpa_supplicant networks auto-connect by default
				})
			}
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"networks": networks,
			"count":    len(networks),
			"source":   "wpa_supplicant",
		})
		return
	}

	// Return empty list if neither works
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"networks": networks,
		"count":    0,
		"source":   "none",
		"error":    "No network manager available",
	})
}

// ForgetWiFiNetwork removes a saved WiFi network
// @Summary Forget a saved WiFi network
// @Description Removes a WiFi network from saved configurations
// @Tags Network
// @Produce json
// @Param ssid path string true "Network SSID to forget"
// @Param interface query string false "WiFi interface name"
// @Success 200 {object} map[string]interface{} "success message"
// @Failure 400 {object} ErrorResponse "SSID required or invalid"
// @Failure 404 {object} ErrorResponse "Network not found"
// @Failure 500 {object} ErrorResponse "Failed to forget network"
// @Router /network/wifi/saved/{ssid} [delete]
func (h *HALHandler) ForgetWiFiNetwork(w http.ResponseWriter, r *http.Request) {
	ssid := chi.URLParam(r, "ssid")
	if ssid == "" {
		errorResponse(w, http.StatusBadRequest, "SSID required")
		return
	}

	// URL decode the SSID
	decodedSSID, err := url.PathUnescape(ssid)
	if err == nil {
		ssid = decodedSSID
	}

	// Validate SSID
	if err := validateSSID(ssid); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	iface := r.URL.Query().Get("interface")
	if iface == "" {
		iface = getDefaultWiFiInterface()
	}
	if err := validateInterfaceName(iface); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Try NetworkManager first (nmcli)
	_, err = execWithTimeout(r.Context(), "nmcli", "connection", "delete", ssid)
	if err == nil {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"ssid":    ssid,
			"message": "Network forgotten via NetworkManager",
		})
		return
	}

	// Try wpa_cli (via nsenter for container compatibility)
	output, err := execWpaCli(r.Context(), "-i", iface, "list_networks")
	if err != nil {
		log.Printf("ForgetWiFiNetwork: list_networks: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to list networks")
		return
	}

	// Find the network ID for this SSID
	lines := strings.Split(output, "\n")
	networkID := ""
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ssid {
			networkID = fields[0]
			break
		}
	}

	if networkID == "" {
		errorResponse(w, http.StatusNotFound, "network not found")
		return
	}

	// Remove the network
	_, err = execWpaCli(r.Context(), "-i", iface, "remove_network", networkID)
	if err != nil {
		log.Printf("ForgetWiFiNetwork: remove_network: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to remove network")
		return
	}

	// Save configuration (best effort)
	_, _ = execWpaCli(r.Context(), "-i", iface, "save_config")

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"ssid":    ssid,
		"message": "Network forgotten via wpa_supplicant",
	})
}
