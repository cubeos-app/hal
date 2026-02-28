package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// DHCPCapabilityResponse represents the result of DHCP safety detection.
// @Description DHCP capability check result
type DHCPCapabilityResponse struct {
	SafeToEnable         bool   `json:"safe_to_enable" example:"false"`
	ExistingDHCPDetected bool   `json:"existing_dhcp_detected" example:"true"`
	ExistingDHCPServer   string `json:"existing_dhcp_server" example:"192.168.1.1"`
	Message              string `json:"message" example:"Existing DHCP server detected on network"`
}

// ProxyCapabilityResponse represents the result of local NPM health detection.
// @Description Local NPM proxy capability check result
type ProxyCapabilityResponse struct {
	Available  bool   `json:"available" example:"true"`
	NPMURL     string `json:"npm_url" example:"http://10.42.24.1:81"`
	NPMVersion string `json:"npm_version" example:"2.11.3"`
}

// GetDHCPCapability checks if Pi-hole DHCP can safely be enabled.
// @Summary Check DHCP capability
// @Description Checks if Pi-hole DHCP can safely be enabled by detecting existing DHCP servers on the network. Sends a DHCP DISCOVER and listens for responses within 3 seconds.
// @Tags Network
// @Produce json
// @Success 200 {object} DHCPCapabilityResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/capabilities/dhcp [get]
func (h *HALHandler) GetDHCPCapability(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	result := DHCPCapabilityResponse{
		SafeToEnable:         false,
		ExistingDHCPDetected: false,
	}

	// Try nmap DHCP discovery first
	dhcpServer, err := detectDHCPViaNmap(ctx)
	if err == nil && dhcpServer != "" {
		result.ExistingDHCPDetected = true
		result.ExistingDHCPServer = dhcpServer
		result.Message = "Existing DHCP server detected on network"
		jsonResponse(w, http.StatusOK, result)
		return
	}

	// Fallback: try raw UDP DHCP DISCOVER
	dhcpServer, err = detectDHCPViaUDP(ctx)
	if err == nil && dhcpServer != "" {
		result.ExistingDHCPDetected = true
		result.ExistingDHCPServer = dhcpServer
		result.Message = "Existing DHCP server detected on network"
		jsonResponse(w, http.StatusOK, result)
		return
	}

	if err != nil {
		// Cannot detect — report as unsafe
		result.Message = fmt.Sprintf("Cannot detect DHCP: %s", sanitizeExecError("dhcp-detect", err))
		jsonResponse(w, http.StatusOK, result)
		return
	}

	// No DHCP server found — safe to enable
	result.SafeToEnable = true
	result.Message = "No existing DHCP server detected"
	jsonResponse(w, http.StatusOK, result)
}

// getUplinkInterface returns the interface used for the default route, or "eth0" as fallback.
func getUplinkInterface(ctx context.Context) string {
	output, err := execWithTimeout(ctx, "ip", "route", "show", "default")
	if err == nil {
		// Format: "default via 192.168.1.1 dev eth0 ..."
		parts := strings.Fields(output)
		for i, p := range parts {
			if p == "dev" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}
	return "eth0"
}

// detectDHCPViaNmap uses nmap's DHCP discovery script to detect DHCP servers.
func detectDHCPViaNmap(ctx context.Context) (string, error) {
	iface := getUplinkInterface(ctx)
	output, err := execWithTimeout(ctx, "nmap", "--script", "broadcast-dhcp-discover", "-e", iface, "--unprivileged")
	if err != nil {
		return "", err
	}

	// Parse nmap output for "Server Identifier" line
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Server Identifier") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", nil
}

// detectDHCPViaUDP sends a minimal DHCP DISCOVER and listens for OFFER.
func detectDHCPViaUDP(ctx context.Context) (string, error) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:68")
	if err != nil {
		return "", fmt.Errorf("bind UDP 68: %w", err)
	}
	defer conn.Close()

	// Set deadline from context
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	conn.SetDeadline(deadline)

	// Build minimal DHCP DISCOVER packet
	discover := buildDHCPDiscover()

	// Send to broadcast
	dst := &net.UDPAddr{IP: net.IPv4(255, 255, 255, 255), Port: 67}
	if _, err := conn.WriteTo(discover, dst); err != nil {
		return "", fmt.Errorf("send discover: %w", err)
	}

	// Listen for response
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		// Timeout = no DHCP server found (good)
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return "", nil
		}
		return "", fmt.Errorf("read response: %w", err)
	}

	// Parse server IP from DHCP OFFER (offset 20 = siaddr)
	if n >= 24 {
		serverIP := net.IPv4(buf[20], buf[21], buf[22], buf[23])
		if !serverIP.Equal(net.IPv4zero) {
			return serverIP.String(), nil
		}
	}

	return "", nil
}

// buildDHCPDiscover creates a minimal DHCP DISCOVER packet.
func buildDHCPDiscover() []byte {
	pkt := make([]byte, 300)
	pkt[0] = 1    // op: BOOTREQUEST
	pkt[1] = 1    // htype: Ethernet
	pkt[2] = 6    // hlen: MAC length
	pkt[3] = 0    // hops
	pkt[4] = 0x39 // xid (random transaction ID)
	pkt[5] = 0x03
	pkt[6] = 0xF3
	pkt[7] = 0x26

	// Magic cookie at offset 236
	pkt[236] = 99
	pkt[237] = 130
	pkt[238] = 83
	pkt[239] = 99

	// DHCP options
	pkt[240] = 53 // Option 53: DHCP Message Type
	pkt[241] = 1  // Length
	pkt[242] = 1  // DHCP DISCOVER

	pkt[243] = 255 // End option

	return pkt[:244]
}

// GetProxyCapability checks if the local NPM instance is healthy.
// @Summary Check proxy capability
// @Description Checks if the local Nginx Proxy Manager is reachable and healthy.
// @Tags Network
// @Produce json
// @Success 200 {object} ProxyCapabilityResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/capabilities/proxy [get]
func (h *HALHandler) GetProxyCapability(w http.ResponseWriter, r *http.Request) {
	gatewayIP := os.Getenv("CUBEOS_GATEWAY_IP")
	if gatewayIP == "" {
		gatewayIP = "10.42.24.1"
	}
	npmPort := os.Getenv("CUBEOS_NPM_PORT")
	if npmPort == "" {
		npmPort = "81"
	}
	npmURL := "http://" + gatewayIP + ":" + npmPort

	result := ProxyCapabilityResponse{
		Available: false,
		NPMURL:    npmURL,
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost:" + npmPort + "/api/")
	if err != nil {
		// Try gateway IP fallback
		resp, err = client.Get(npmURL + "/api/")
		if err != nil {
			jsonResponse(w, http.StatusOK, result)
			return
		}
	}
	defer resp.Body.Close()

	// 200 or 401 means NPM is running
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		result.Available = true

		// Try to get version from API response
		if resp.StatusCode == http.StatusOK {
			var apiResp map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&apiResp); err == nil {
				if v, ok := apiResp["version"].(string); ok {
					result.NPMVersion = v
				}
			}
		}
	}

	jsonResponse(w, http.StatusOK, result)
}
