package handlers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// VPN Types
// ============================================================================

// VPNStatus represents overall VPN status.
// @Description Overall VPN status
type VPNStatus struct {
	WireGuard []VPNInterface `json:"wireguard"`
	OpenVPN   []VPNInterface `json:"openvpn"`
	Tor       TorStatus      `json:"tor"`
}

// VPNInterface represents a VPN interface.
// @Description VPN interface status
type VPNInterface struct {
	Name      string `json:"name" example:"wg0"`
	Active    bool   `json:"active" example:"true"`
	Type      string `json:"type" example:"wireguard"`
	Endpoint  string `json:"endpoint,omitempty" example:"vpn.example.com:51820"`
	LocalIP   string `json:"local_ip,omitempty" example:"10.0.0.2"`
	PublicKey string `json:"public_key,omitempty"`
}

// TorStatus represents Tor service status.
// @Description Tor service status
type TorStatus struct {
	Running      bool   `json:"running" example:"true"`
	Bootstrapped int    `json:"bootstrapped" example:"100"`
	CircuitReady bool   `json:"circuit_ready" example:"true"`
	SocksPort    int    `json:"socks_port" example:"9050"`
	ControlPort  int    `json:"control_port" example:"9051"`
	ExitIP       string `json:"exit_ip,omitempty" example:"185.220.101.1"`
}

// TorConfig represents Tor configuration.
// @Description Tor configuration settings
type TorConfig struct {
	SocksPort   int               `json:"socks_port" example:"9050"`
	ControlPort int               `json:"control_port" example:"9051"`
	DataDir     string            `json:"data_dir" example:"/var/lib/tor"`
	Settings    map[string]string `json:"settings"`
}

// ============================================================================
// VPN Status Handlers
// ============================================================================

// GetVPNStatus returns overall VPN status.
// @Summary Get VPN status
// @Description Returns status of all VPN connections (WireGuard, OpenVPN, Tor)
// @Tags VPN
// @Accept json
// @Produce json
// @Success 200 {object} VPNStatus
// @Failure 500 {object} ErrorResponse
// @Router /vpn/status [get]
func (h *HALHandler) GetVPNStatus(w http.ResponseWriter, r *http.Request) {
	status := VPNStatus{
		WireGuard: h.getWireGuardInterfaces(),
		OpenVPN:   h.getOpenVPNConnections(),
		Tor:       h.getTorStatus(),
	}

	jsonResponse(w, http.StatusOK, status)
}

// GetPublicIP returns the current public IP as seen from the host.
// @Summary Get public IP address
// @Description Returns the public IP address by querying external services from the host network.
// @Description When a VPN tunnel is active, this returns the VPN exit IP since HAL runs on host networking.
// @Tags VPN
// @Produce json
// @Success 200 {object} map[string]string "public_ip: current public IP"
// @Failure 500 {object} ErrorResponse
// @Router /vpn/public-ip [get]
func (h *HALHandler) GetPublicIP(w http.ResponseWriter, r *http.Request) {
	services := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
		"https://icanhazip.com",
		"https://checkip.amazonaws.com",
	}

	client := &http.Client{Timeout: 5 * time.Second}

	var lastErr error
	for _, svc := range services {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, svc, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "CubeOS/1.0")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: status %d", svc, resp.StatusCode)
			continue
		}

		ip := strings.TrimSpace(string(body))
		if ip != "" && net.ParseIP(ip) != nil {
			jsonResponse(w, http.StatusOK, map[string]string{"public_ip": ip})
			return
		}
	}

	log.Printf("GetPublicIP: all services failed: %v", lastErr)
	errorResponse(w, http.StatusInternalServerError, "failed to determine public IP")
}

// ============================================================================
// WireGuard Handlers
// ============================================================================

// WireGuardUp brings up a WireGuard interface.
// @Summary Bring up WireGuard interface
// @Description Activates a WireGuard VPN interface using its config file
// @Tags VPN
// @Accept json
// @Produce json
// @Param name path string true "Config name (sanitized, alphanumeric/hyphen/underscore)" example(wg0)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/wireguard/up/{name} [post]
func (h *HALHandler) WireGuardUp(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !isValidVPNName(name) {
		errorResponse(w, http.StatusBadRequest, "invalid VPN interface name")
		return
	}

	// Use full config path — configs are at /cubeos/config/vpn/wireguard/, not /etc/wireguard/
	// Run via nsenter to access host's wg-quick and kernel WireGuard module
	confPath := "/cubeos/config/vpn/wireguard/" + name + ".conf"
	out, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"wg-quick", "up", confPath)
	if err != nil {
		log.Printf("WireGuardUp(%s): %v: %s", name, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("WireGuard up", err))
		return
	}

	successResponse(w, fmt.Sprintf("WireGuard interface %s is up", name))
}

// WireGuardDown brings down a WireGuard interface.
// @Summary Bring down WireGuard interface
// @Description Deactivates a WireGuard VPN interface
// @Tags VPN
// @Accept json
// @Produce json
// @Param name path string true "Config name (sanitized)" example(wg0)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/wireguard/down/{name} [post]
func (h *HALHandler) WireGuardDown(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !isValidVPNName(name) {
		errorResponse(w, http.StatusBadRequest, "invalid VPN interface name")
		return
	}

	// Use full config path via nsenter (matches WireGuardUp)
	confPath := "/cubeos/config/vpn/wireguard/" + name + ".conf"
	out, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"wg-quick", "down", confPath)
	if err != nil {
		log.Printf("WireGuardDown(%s): %v: %s", name, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("WireGuard down", err))
		return
	}

	successResponse(w, fmt.Sprintf("WireGuard interface %s is down", name))
}

// ============================================================================
// OpenVPN Handlers
// ============================================================================

// OpenVPNUp brings up an OpenVPN connection.
// @Summary Bring up OpenVPN connection
// @Description Starts an OpenVPN connection using the config file directly
// @Tags VPN
// @Accept json
// @Produce json
// @Param name path string true "Connection name" example(client)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/openvpn/up/{name} [post]
func (h *HALHandler) OpenVPNUp(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !isValidVPNName(name) {
		errorResponse(w, http.StatusBadRequest, "invalid VPN connection name")
		return
	}

	confPath := "/cubeos/config/vpn/openvpn/" + name + ".ovpn"
	authPath := "/cubeos/config/vpn/openvpn/" + name + ".auth"
	pidFile := "/run/openvpn-" + name + ".pid"
	logFile := "/var/log/openvpn-" + name + ".log"

	// Build openvpn command arguments
	ovpnArgs := []string{
		"--config", confPath,
		"--daemon", "ovpn-" + name,
		"--writepid", pidFile,
		"--log-append", logFile,
	}

	// If auth file exists, inject it (overrides any auth-user-pass in .ovpn)
	if _, err := os.Stat(authPath); err == nil {
		ovpnArgs = append(ovpnArgs, "--auth-user-pass", authPath)
	}

	// Override up/down scripts with no-op — the .ovpn may reference scripts
	// that don't exist on this host (e.g. /etc/openvpn/update-resolv-conf).
	// script-security 2 is required to avoid fatal errors when scripts are configured.
	ovpnArgs = append(ovpnArgs, "--script-security", "2",
		"--up", "/bin/true", "--down", "/bin/true")

	// Build full nsenter command
	nsenterArgs := []string{"-t", "1", "-m", "-n", "--", "openvpn"}
	nsenterArgs = append(nsenterArgs, ovpnArgs...)

	out, err := execWithTimeout(r.Context(), "nsenter", nsenterArgs...)
	if err != nil {
		log.Printf("OpenVPNUp(%s): %v: %s", name, err, out)
		// Check log file for more details
		if logContent, readErr := os.ReadFile(logFile); readErr == nil {
			lines := strings.Split(string(logContent), "\n")
			// Get last 5 lines for error context
			start := len(lines) - 5
			if start < 0 {
				start = 0
			}
			log.Printf("OpenVPNUp(%s) log tail: %s", name, strings.Join(lines[start:], "\n"))
		}
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("OpenVPN start", err))
		return
	}

	successResponse(w, fmt.Sprintf("OpenVPN connection %s started", name))
}

// OpenVPNDown brings down an OpenVPN connection.
// @Summary Bring down OpenVPN connection
// @Description Stops an OpenVPN connection by sending SIGTERM to its process
// @Tags VPN
// @Accept json
// @Produce json
// @Param name path string true "Connection name" example(client)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/openvpn/down/{name} [post]
func (h *HALHandler) OpenVPNDown(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !isValidVPNName(name) {
		errorResponse(w, http.StatusBadRequest, "invalid VPN connection name")
		return
	}

	// Read PID file and kill the process
	pidFile := "/run/openvpn-" + name + ".pid"
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		// Try pkill as fallback
		out, killErr := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
			"pkill", "-f", "ovpn-"+name)
		if killErr != nil {
			log.Printf("OpenVPNDown(%s): no pidfile and pkill failed: %v: %s", name, killErr, out)
			errorResponse(w, http.StatusInternalServerError, "OpenVPN process not found")
			return
		}
		successResponse(w, fmt.Sprintf("OpenVPN connection %s stopped", name))
		return
	}

	pid := strings.TrimSpace(string(pidData))
	out, err := execWithTimeout(r.Context(), "nsenter", "-t", "1", "-m", "-n", "--",
		"kill", pid)
	if err != nil {
		log.Printf("OpenVPNDown(%s): kill %s failed: %v: %s", name, pid, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("OpenVPN stop", err))
		return
	}

	// Clean up pid file
	_ = os.Remove(pidFile)

	successResponse(w, fmt.Sprintf("OpenVPN connection %s stopped", name))
}

// ============================================================================
// Tor Handlers
// ============================================================================

// GetTorStatus returns Tor service status.
// @Summary Get Tor status
// @Description Returns Tor service status and circuit information
// @Tags VPN
// @Accept json
// @Produce json
// @Success 200 {object} TorStatus
// @Failure 500 {object} ErrorResponse
// @Router /vpn/tor/status [get]
func (h *HALHandler) GetTorStatus(w http.ResponseWriter, r *http.Request) {
	status := h.getTorStatus()
	jsonResponse(w, http.StatusOK, status)
}

// StartTor starts the Tor service.
// @Summary Start Tor
// @Description Starts the Tor SOCKS proxy service
// @Tags VPN
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/tor/start [post]
func (h *HALHandler) StartTor(w http.ResponseWriter, r *http.Request) {
	out, err := execWithTimeout(r.Context(), "systemctl", "start", "tor")
	if err != nil {
		log.Printf("StartTor: %v: %s", err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("Tor start", err))
		return
	}

	successResponse(w, "Tor service started")
}

// StopTor stops the Tor service.
// @Summary Stop Tor
// @Description Stops the Tor SOCKS proxy service
// @Tags VPN
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/tor/stop [post]
func (h *HALHandler) StopTor(w http.ResponseWriter, r *http.Request) {
	out, err := execWithTimeout(r.Context(), "systemctl", "stop", "tor")
	if err != nil {
		log.Printf("StopTor: %v: %s", err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("Tor stop", err))
		return
	}

	successResponse(w, "Tor service stopped")
}

// NewTorCircuit requests a new Tor circuit.
// @Summary Request new Tor circuit
// @Description Requests a new Tor circuit for a new exit IP
// @Tags VPN
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /vpn/tor/newcircuit [post]
func (h *HALHandler) NewTorCircuit(w http.ResponseWriter, r *http.Request) {
	controlPort := getTorControlPort()
	err := torControlCommand(r.Context(), controlPort, "SIGNAL NEWNYM")
	if err != nil {
		log.Printf("NewTorCircuit: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to request new Tor circuit")
		return
	}

	successResponse(w, "new Tor circuit requested")
}

// GetTorConfig returns Tor configuration.
// @Summary Get Tor configuration
// @Description Returns Tor service configuration settings
// @Tags VPN
// @Accept json
// @Produce json
// @Success 200 {object} TorConfig
// @Failure 500 {object} ErrorResponse
// @Router /vpn/tor/config [get]
func (h *HALHandler) GetTorConfig(w http.ResponseWriter, r *http.Request) {
	config := TorConfig{
		SocksPort:   9050,
		ControlPort: 9051,
		DataDir:     "/var/lib/tor",
		Settings:    make(map[string]string),
	}

	// Read torrc if it exists
	if data, err := os.ReadFile("/etc/tor/torrc"); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, " ", 2)
			if len(parts) == 2 {
				config.Settings[parts[0]] = parts[1]
			}
		}
	}

	jsonResponse(w, http.StatusOK, config)
}

// ============================================================================
// Tor Control Protocol (replaces bash -c + nc) — HF03-13
// ============================================================================

// getTorControlPort returns the Tor control port from env or default.
func getTorControlPort() string {
	if port := os.Getenv("HAL_TOR_CONTROL_PORT"); port != "" {
		return port
	}
	return "9051"
}

// torControlCommand connects to the Tor control port via net.Dial and sends a command.
func torControlCommand(ctx context.Context, port string, command string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return fmt.Errorf("connect to Tor control port: %w", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Send AUTHENTICATE
	fmt.Fprintf(conn, "AUTHENTICATE \"\"\r\n")
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if !strings.HasPrefix(line, "250") {
		return fmt.Errorf("auth failed: %s", strings.TrimSpace(line))
	}

	// Send command
	fmt.Fprintf(conn, "%s\r\n", command)
	line, err = reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read command response: %w", err)
	}
	if !strings.HasPrefix(line, "250") {
		return fmt.Errorf("command failed: %s", strings.TrimSpace(line))
	}

	// Send QUIT
	fmt.Fprintf(conn, "QUIT\r\n")
	return nil
}

// torControlQuery connects to the Tor control port and sends a GETINFO query.
func torControlQuery(ctx context.Context, port string, query string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return "", fmt.Errorf("connect to Tor control port: %w", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Authenticate
	fmt.Fprintf(conn, "AUTHENTICATE \"\"\r\n")
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read auth response: %w", err)
	}
	if !strings.HasPrefix(line, "250") {
		return "", fmt.Errorf("auth failed: %s", strings.TrimSpace(line))
	}

	// Send GETINFO
	fmt.Fprintf(conn, "GETINFO %s\r\n", query)
	var result strings.Builder
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			break
		}
		result.WriteString(line)
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}

	// Quit
	fmt.Fprintf(conn, "QUIT\r\n")
	return result.String(), nil
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) getWireGuardInterfaces() []VPNInterface {
	var interfaces []VPNInterface

	ctx := context.Background()
	out, err := execWithTimeout(ctx, "wg", "show", "interfaces")
	if err != nil {
		return interfaces
	}

	names := strings.Fields(out)
	for _, name := range names {
		iface := VPNInterface{
			Name:   name,
			Type:   "wireguard",
			Active: true,
		}

		// Get more details
		if details, err := execWithTimeout(ctx, "wg", "show", name); err == nil {
			lines := strings.Split(details, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "endpoint:") {
					iface.Endpoint = strings.TrimPrefix(line, "endpoint: ")
				}
				if strings.HasPrefix(line, "public key:") {
					iface.PublicKey = strings.TrimPrefix(line, "public key: ")
				}
			}
		}

		// Get local IP
		if ipOut, err := execWithTimeout(ctx, "ip", "-o", "addr", "show", name); err == nil {
			fields := strings.Fields(ipOut)
			for i, f := range fields {
				if f == "inet" && i+1 < len(fields) {
					iface.LocalIP = strings.Split(fields[i+1], "/")[0]
				}
			}
		}

		interfaces = append(interfaces, iface)
	}

	return interfaces
}

func (h *HALHandler) getOpenVPNConnections() []VPNInterface {
	var interfaces []VPNInterface

	ctx := context.Background()

	// Check for tun interfaces
	entries, _ := os.ReadDir("/sys/class/net")
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "tun") {
			iface := VPNInterface{
				Name:   name,
				Type:   "openvpn",
				Active: true,
			}

			// Get local IP
			if ipOut, err := execWithTimeout(ctx, "ip", "-o", "addr", "show", name); err == nil {
				fields := strings.Fields(ipOut)
				for i, f := range fields {
					if f == "inet" && i+1 < len(fields) {
						iface.LocalIP = strings.Split(fields[i+1], "/")[0]
					}
				}
			}

			interfaces = append(interfaces, iface)
		}
	}

	return interfaces
}

func (h *HALHandler) getTorStatus() TorStatus {
	status := TorStatus{
		SocksPort:   9050,
		ControlPort: 9051,
	}

	ctx := context.Background()

	// Check if Tor is running
	out, err := execWithTimeout(ctx, "systemctl", "is-active", "tor")
	if err == nil {
		status.Running = strings.TrimSpace(out) == "active"
	}

	if !status.Running {
		return status
	}

	// Get bootstrap status via Tor control protocol (replaces bash -c + nc)
	controlPort := getTorControlPort()
	if resp, err := torControlQuery(ctx, controlPort, "status/bootstrap-phase"); err == nil {
		if strings.Contains(resp, "PROGRESS=100") {
			status.Bootstrapped = 100
			status.CircuitReady = true
		} else if idx := strings.Index(resp, "PROGRESS="); idx != -1 {
			progressStr := resp[idx+9:]
			if spaceIdx := strings.Index(progressStr, " "); spaceIdx != -1 {
				progressStr = progressStr[:spaceIdx]
			}
			fmt.Sscanf(progressStr, "%d", &status.Bootstrapped)
		}
	}

	// Get exit IP through Tor
	if exitOut, err := execWithTimeout(ctx, "curl", "-s", "--max-time", "10",
		"--socks5-hostname", "localhost:9050", "https://check.torproject.org/api/ip"); err == nil {
		if idx := strings.Index(exitOut, `"IP":"`); idx != -1 {
			ipStart := idx + 6
			if ipEnd := strings.Index(exitOut[ipStart:], `"`); ipEnd != -1 {
				status.ExitIP = exitOut[ipStart : ipStart+ipEnd]
			}
		}
	}

	return status
}
