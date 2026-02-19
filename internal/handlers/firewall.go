package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// getDefaultNATSource returns the default NAT source CIDR.
func getDefaultNATSource() string {
	if src := os.Getenv("HAL_NAT_SOURCE"); src != "" {
		return src
	}
	return "10.42.24.0/24"
}

// getDefaultNATInterface returns the default NAT output interface.
func getDefaultNATInterface() string {
	if iface := os.Getenv("HAL_NAT_INTERFACE"); iface != "" {
		return iface
	}
	return "eth0"
}

// GetFirewallRules returns iptables rules
// @Summary Get firewall rules
// @Description Returns all iptables rules from filter and nat tables
// @Tags Firewall
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /firewall/rules [get]
func (h *HALHandler) GetFirewallRules(w http.ResponseWriter, r *http.Request) {
	// Get filter table rules
	filterOutput, err := execWithTimeout(r.Context(), "iptables", "-L", "-n", "-v", "--line-numbers")
	if err != nil {
		log.Printf("GetFirewallRules: filter: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to get filter rules")
		return
	}

	// Get NAT table rules
	natOutput, _ := execWithTimeout(r.Context(), "iptables", "-t", "nat", "-L", "-n", "-v", "--line-numbers")

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"filter": parseIptablesOutput(filterOutput),
		"nat":    parseIptablesOutput(natOutput),
	})
}

// GetFirewallStatus returns overall firewall status
// @Summary Get firewall status
// @Description Returns overall firewall status including forwarding and NAT state
// @Tags Firewall
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /firewall/status [get]
func (h *HALHandler) GetFirewallStatus(w http.ResponseWriter, r *http.Request) {
	// Get forwarding status
	forwardData, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	forwardingEnabled := strings.TrimSpace(string(forwardData)) == "1"

	// Count rules in each chain
	filterOutput, _ := execWithTimeout(r.Context(), "iptables", "-L", "-n", "--line-numbers")
	natOutput, _ := execWithTimeout(r.Context(), "iptables", "-t", "nat", "-L", "-n", "--line-numbers")

	// Check if NAT is enabled by looking for MASQUERADE rules
	natEnabled := strings.Contains(natOutput, "MASQUERADE")

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"enabled":            true,
		"forwarding_enabled": forwardingEnabled,
		"nat_enabled":        natEnabled,
		"rules_count":        countIptablesRules(filterOutput),
		"nat_rules_count":    countIptablesRules(natOutput),
	})
}

// GetForwardingStatus returns IP forwarding status
// @Summary Get IP forwarding status
// @Description Returns whether IP forwarding is enabled by reading /proc/sys/net/ipv4/ip_forward
// @Tags Firewall
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /firewall/forwarding [get]
func (h *HALHandler) GetForwardingStatus(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		log.Printf("GetForwardingStatus: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to read forwarding status")
		return
	}

	value := strings.TrimSpace(string(data))
	enabled := value == "1"

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"enabled": enabled,
		"value":   value,
		"source":  "/proc/sys/net/ipv4/ip_forward",
	})
}

// GetNATStatus returns NAT status
// @Summary Get NAT status
// @Description Returns whether NAT/masquerading is enabled by checking POSTROUTING chain
// @Tags Firewall
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /firewall/nat/status [get]
func (h *HALHandler) GetNATStatus(w http.ResponseWriter, r *http.Request) {
	output, err := execWithTimeout(r.Context(), "iptables", "-t", "nat", "-L", "POSTROUTING", "-n", "-v")
	if err != nil {
		log.Printf("GetNATStatus: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to get NAT rules")
		return
	}

	enabled := strings.Contains(output, "MASQUERADE")

	var sourceNet, outInterface string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "MASQUERADE") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if strings.Contains(f, "/") && !strings.HasPrefix(f, "0.0.0.0") {
					sourceNet = f
				}
				if strings.HasPrefix(f, "eth") || strings.HasPrefix(f, "wlan") {
					outInterface = f
				}
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"enabled":       enabled,
		"source":        sourceNet,
		"out_interface": outInterface,
	})
}

// AddFirewallRule adds a firewall rule
// @Summary Add firewall rule
// @Description Adds a new iptables rule to the specified chain
// @Tags Firewall
// @Accept json
// @Produce json
// @Param request body object true "Firewall rule" example({"chain":"INPUT","protocol":"tcp","port":"80","action":"ACCEPT"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/rule [post]
func (h *HALHandler) AddFirewallRule(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB

	var req struct {
		Chain       string `json:"chain"`
		Protocol    string `json:"protocol"`
		Port        string `json:"port"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Action      string `json:"action"`
		Interface   string `json:"interface"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Apply defaults
	if req.Chain == "" {
		req.Chain = "INPUT"
	}
	if req.Action == "" {
		req.Action = "ACCEPT"
	}

	// Validate chain
	if err := validateFirewallChain(req.Chain); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate action
	if err := validateFirewallAction(req.Action); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate protocol if provided
	if req.Protocol != "" {
		if err := validateFirewallProtocol(req.Protocol); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Validate port if provided
	if req.Port != "" {
		if err := validatePort(req.Port); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		// Port requires protocol
		if req.Protocol == "" {
			errorResponse(w, http.StatusBadRequest, "protocol is required when port is specified")
			return
		}
	}

	// Validate source IP/CIDR if provided
	if req.Source != "" {
		if err := validateCIDROrIP(req.Source); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid source: "+err.Error())
			return
		}
	}

	// Validate destination IP/CIDR if provided
	if req.Destination != "" {
		if err := validateCIDROrIP(req.Destination); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid destination: "+err.Error())
			return
		}
	}

	// Validate interface if provided
	if req.Interface != "" {
		if err := validateInterfaceName(req.Interface); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid interface: "+err.Error())
			return
		}
	}

	// Build iptables command
	args := []string{"-A", req.Chain}

	if req.Interface != "" {
		args = append(args, "-i", req.Interface)
	}
	if req.Protocol != "" {
		args = append(args, "-p", req.Protocol)
	}
	if req.Port != "" {
		args = append(args, "--dport", req.Port)
	}
	if req.Source != "" {
		args = append(args, "-s", req.Source)
	}
	if req.Destination != "" {
		args = append(args, "-d", req.Destination)
	}
	args = append(args, "-j", req.Action)

	_, err := execWithTimeout(r.Context(), "iptables", args...)
	if err != nil {
		log.Printf("AddFirewallRule: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("add firewall rule", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"rule":    req,
	})
}

// DeleteFirewallRule deletes a firewall rule
// @Summary Delete firewall rule
// @Description Deletes an iptables rule by chain and rule number
// @Tags Firewall
// @Accept json
// @Produce json
// @Param request body object true "Rule to delete" example({"chain":"INPUT","rule_number":"1"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/rule [delete]
func (h *HALHandler) DeleteFirewallRule(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB

	var req struct {
		Chain      string `json:"chain"`
		RuleNumber string `json:"rule_number"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Chain == "" || req.RuleNumber == "" {
		errorResponse(w, http.StatusBadRequest, "chain and rule_number required")
		return
	}

	// Validate chain
	if err := validateFirewallChain(req.Chain); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate rule number (positive integer)
	if err := validateRuleNumber(req.RuleNumber); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err := execWithTimeout(r.Context(), "iptables", "-D", req.Chain, req.RuleNumber)
	if err != nil {
		log.Printf("DeleteFirewallRule: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("delete firewall rule", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"chain":   req.Chain,
		"deleted": req.RuleNumber,
	})
}

// EnableNAT enables NAT/masquerading
// @Summary Enable NAT
// @Description Enables NAT masquerading for the specified source network and output interface
// @Tags Firewall
// @Accept json
// @Produce json
// @Param request body object false "NAT config" example({"source":"10.42.24.0/24","out_interface":"eth0"})
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/nat/enable [post]
func (h *HALHandler) EnableNAT(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20) // 1MB

	var req struct {
		Source       string `json:"source"`
		OutInterface string `json:"out_interface"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Allow empty body with defaults
		req.Source = getDefaultNATSource()
		req.OutInterface = getDefaultNATInterface()
	}

	if req.Source == "" {
		req.Source = getDefaultNATSource()
	}
	if req.OutInterface == "" {
		req.OutInterface = getDefaultNATInterface()
	}

	// Validate source CIDR
	if err := validateCIDROrIP(req.Source); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid NAT source: "+err.Error())
		return
	}

	// Validate output interface
	if err := validateInterfaceName(req.OutInterface); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid output interface: "+err.Error())
		return
	}

	args := []string{"-t", "nat", "-A", "POSTROUTING", "-s", req.Source, "-o", req.OutInterface, "-j", "MASQUERADE"}
	_, err := execWithTimeout(r.Context(), "iptables", args...)
	if err != nil {
		log.Printf("EnableNAT: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("enable NAT", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"source":        req.Source,
		"out_interface": req.OutInterface,
	})
}

// DisableNAT disables NAT/masquerading
// @Summary Disable NAT
// @Description Disables NAT masquerading by flushing the POSTROUTING chain
// @Tags Firewall
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/nat/disable [post]
func (h *HALHandler) DisableNAT(w http.ResponseWriter, r *http.Request) {
	_, err := execWithTimeout(r.Context(), "iptables", "-t", "nat", "-F", "POSTROUTING")
	if err != nil {
		log.Printf("DisableNAT: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("disable NAT", err))
		return
	}

	successResponse(w, "NAT disabled")
}

// EnableIPForward enables IP forwarding
// @Summary Enable IP forwarding
// @Description Enables IP forwarding by writing 1 to /proc/sys/net/ipv4/ip_forward
// @Tags Firewall
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/forward/enable [post]
func (h *HALHandler) EnableIPForward(w http.ResponseWriter, r *http.Request) {
	err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	if err != nil {
		log.Printf("EnableIPForward: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to enable forwarding")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"enabled": true,
	})
}

// DisableIPForward disables IP forwarding
// @Summary Disable IP forwarding
// @Description Disables IP forwarding by writing 0 to /proc/sys/net/ipv4/ip_forward
// @Tags Firewall
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/forward/disable [post]
func (h *HALHandler) DisableIPForward(w http.ResponseWriter, r *http.Request) {
	err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("0"), 0644)
	if err != nil {
		log.Printf("DisableIPForward: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to disable forwarding")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"enabled": false,
	})
}

// getFirewallSavePath returns the path for persistent iptables rules.
func getFirewallSavePath() string {
	if p := os.Getenv("HAL_IPTABLES_SAVE_PATH"); p != "" {
		return p
	}
	return "/etc/iptables/rules.v4"
}

// SaveFirewallRules saves current iptables rules to persistent storage
// @Summary Save firewall rules
// @Description Saves current iptables rules to /etc/iptables/rules.v4 (or HAL_IPTABLES_SAVE_PATH)
// @Tags Firewall
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/save [post]
func (h *HALHandler) SaveFirewallRules(w http.ResponseWriter, r *http.Request) {
	savePath := getFirewallSavePath()

	output, err := execWithTimeout(r.Context(), "iptables-save")
	if err != nil {
		log.Printf("SaveFirewallRules: iptables-save: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("save firewall rules", err))
		return
	}

	// Ensure directory exists
	dir := savePath[:strings.LastIndex(savePath, "/")]
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("SaveFirewallRules: mkdir %s: %v", dir, err)
		errorResponse(w, http.StatusInternalServerError, "failed to create rules directory")
		return
	}

	if err := os.WriteFile(savePath, []byte(output), 0600); err != nil {
		log.Printf("SaveFirewallRules: write %s: %v", savePath, err)
		errorResponse(w, http.StatusInternalServerError, "failed to write firewall rules")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"path":    savePath,
		"message": "firewall rules saved",
	})
}

// RestoreFirewallRules restores iptables rules from persistent storage
// @Summary Restore firewall rules
// @Description Restores iptables rules from /etc/iptables/rules.v4 (or HAL_IPTABLES_SAVE_PATH)
// @Tags Firewall
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/restore [post]
func (h *HALHandler) RestoreFirewallRules(w http.ResponseWriter, r *http.Request) {
	savePath := getFirewallSavePath()

	data, err := os.ReadFile(savePath)
	if err != nil {
		if os.IsNotExist(err) {
			errorResponse(w, http.StatusNotFound, "no saved firewall rules found")
			return
		}
		log.Printf("RestoreFirewallRules: read %s: %v", savePath, err)
		errorResponse(w, http.StatusInternalServerError, "failed to read saved rules")
		return
	}

	// iptables-restore reads from stdin
	ctx := r.Context()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "iptables-restore")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("RestoreFirewallRules: iptables-restore: %v: %s", err, string(out))
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("restore firewall rules", err))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"path":    savePath,
		"message": "firewall rules restored",
	})
}

// ResetFirewall flushes all iptables rules and resets to default ACCEPT policy
// @Summary Reset firewall
// @Description Flushes all iptables rules in filter and nat tables, resets default policies to ACCEPT
// @Tags Firewall
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /firewall/reset [post]
func (h *HALHandler) ResetFirewall(w http.ResponseWriter, r *http.Request) {
	// Reset default policies to ACCEPT
	commands := []struct {
		args []string
		desc string
	}{
		{[]string{"-P", "INPUT", "ACCEPT"}, "reset INPUT policy"},
		{[]string{"-P", "FORWARD", "ACCEPT"}, "reset FORWARD policy"},
		{[]string{"-P", "OUTPUT", "ACCEPT"}, "reset OUTPUT policy"},
		{[]string{"-F"}, "flush filter rules"},
		{[]string{"-X"}, "delete custom chains"},
		{[]string{"-t", "nat", "-F"}, "flush NAT rules"},
		{[]string{"-t", "nat", "-X"}, "delete NAT custom chains"},
	}

	for _, cmd := range commands {
		if _, err := execWithTimeout(r.Context(), "iptables", cmd.args...); err != nil {
			log.Printf("ResetFirewall: %s: %v", cmd.desc, err)
			errorResponse(w, http.StatusInternalServerError, sanitizeExecError(cmd.desc, err))
			return
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "firewall reset to default ACCEPT policy",
	})
}

// Helper functions

func countIptablesRules(output string) int {
	count := 0
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 0 && line[0] >= '0' && line[0] <= '9' {
			count++
		}
	}
	return count
}

// mapProtocol converts numeric protocol IDs from iptables -n to human-readable names.
func mapProtocol(p string) string {
	switch p {
	case "0":
		return "all"
	case "6":
		return "tcp"
	case "17":
		return "udp"
	case "1":
		return "icmp"
	case "58":
		return "icmpv6"
	case "47":
		return "gre"
	case "50":
		return "esp"
	case "51":
		return "ah"
	case "132":
		return "sctp"
	default:
		return p
	}
}

func parseIptablesOutput(output string) []map[string]string {
	var rules []map[string]string
	lines := strings.Split(output, "\n")

	currentChain := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "Chain ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				currentChain = parts[1]
			}
			continue
		}

		if line == "" || strings.HasPrefix(line, "num") || strings.HasPrefix(line, "pkts") {
			continue
		}

		// Format with -L -n -v --line-numbers:
		// num  pkts bytes target  prot opt in  out  source  destination  [extra...]
		// [0]  [1]  [2]   [3]    [4] [5] [6] [7]   [8]      [9]        [10+]
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		rule := map[string]string{
			"chain":         currentChain,
			"target":        fields[3],
			"prot":          mapProtocol(fields[4]),
			"source":        fields[8],
			"destination":   fields[9],
			"in_interface":  fields[6],
			"out_interface": fields[7],
			"pkts":          fields[1],
			"bytes":         fields[2],
		}

		// Extract port info from extra fields (e.g. "tcp dpt:22", "udp spt:53 dpt:1024")
		extra := strings.Join(fields[10:], " ")
		if extra != "" {
			rule["options"] = extra
			// Extract destination port
			if idx := strings.Index(extra, "dpt:"); idx >= 0 {
				portStr := extra[idx+4:]
				if spaceIdx := strings.IndexByte(portStr, ' '); spaceIdx >= 0 {
					portStr = portStr[:spaceIdx]
				}
				rule["dport"] = portStr
			}
			// Extract source port
			if idx := strings.Index(extra, "spt:"); idx >= 0 {
				portStr := extra[idx+4:]
				if spaceIdx := strings.IndexByte(portStr, ' '); spaceIdx >= 0 {
					portStr = portStr[:spaceIdx]
				}
				rule["sport"] = portStr
			}
		}

		rules = append(rules, rule)
	}

	return rules
}
