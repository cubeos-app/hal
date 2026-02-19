package handlers

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// =============================================================================
// Types
// =============================================================================

// ListeningPort represents a TCP or UDP port in LISTEN state on the host.
// @Description A port currently bound and listening on the host
type ListeningPort struct {
	Port     int    `json:"port" example:"22"`
	Protocol string `json:"protocol" example:"tcp"`
	Process  string `json:"process" example:"sshd"`
	PID      int    `json:"pid" example:"1234"`
}

// ListeningPortsResponse is the response from the listening ports endpoint.
// @Description All TCP/UDP ports currently in LISTEN state on the host
type ListeningPortsResponse struct {
	Ports []ListeningPort `json:"ports"`
}

// =============================================================================
// Handler
// =============================================================================

// ListeningPortsHandler returns all TCP/UDP ports in LISTEN state on the host.
// @Summary List listening ports
// @Description Returns all TCP and UDP ports currently in LISTEN state on the host by parsing /proc/net/tcp, /proc/net/tcp6, /proc/net/udp, and /proc/net/udp6. Process name and PID are best-effort.
// @Tags Network
// @Produce json
// @Success 200 {object} ListeningPortsResponse
// @Failure 500 {object} ErrorResponse
// @Router /network/ports/listening [get]
func (h *HALHandler) ListeningPortsHandler(w http.ResponseWriter, r *http.Request) {
	// Build inode→pid map for process resolution
	inodePID := buildInodePIDMap()

	var ports []ListeningPort

	// Parse TCP (state 0A = LISTEN)
	tcpPorts := parseProcNet("/proc/net/tcp", "tcp", "0A", inodePID)
	ports = append(ports, tcpPorts...)

	tcp6Ports := parseProcNet("/proc/net/tcp6", "tcp", "0A", inodePID)
	ports = append(ports, tcp6Ports...)

	// Parse UDP — UDP has no LISTEN state, but state 07 (CLOSE) represents
	// a bound-but-unconnected socket, which is the UDP equivalent of "listening".
	udpPorts := parseProcNet("/proc/net/udp", "udp", "07", inodePID)
	ports = append(ports, udpPorts...)

	udp6Ports := parseProcNet("/proc/net/udp6", "udp", "07", inodePID)
	ports = append(ports, udp6Ports...)

	// Deduplicate: a port appearing in both tcp and tcp6 (or udp and udp6)
	// with the same protocol is the same listener (dual-stack socket).
	ports = deduplicatePorts(ports)

	jsonResponse(w, http.StatusOK, ListeningPortsResponse{Ports: ports})
}

// =============================================================================
// /proc/net parsing
// =============================================================================

// parseProcNet reads a /proc/net file and extracts ports in the given state.
// Format of each line (after header):
//
//	sl  local_address rem_address   st tx_queue:rx_queue ... inode
//	0: 0100007F:0035 00000000:0000 0A 00000000:00000000 ...  12345
//
// local_address is hex IP:hex port. State is hex (0A = LISTEN for TCP).
func parseProcNet(path, protocol, stateFilter string, inodePID map[string]pidInfo) []ListeningPort {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var ports []ListeningPort
	scanner := bufio.NewScanner(f)

	// Skip header line
	if !scanner.Scan() {
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		// fields[1] = local_address (hex_ip:hex_port)
		// fields[3] = state (hex)
		state := strings.ToUpper(fields[3])
		if state != stateFilter {
			continue
		}

		localAddr := fields[1]
		colonIdx := strings.LastIndex(localAddr, ":")
		if colonIdx < 0 {
			continue
		}

		hexPort := localAddr[colonIdx+1:]
		port, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil || port == 0 {
			continue
		}

		// Resolve process via inode (field index 9)
		inode := fields[9]
		var process string
		var pid int
		if info, ok := inodePID[inode]; ok {
			process = info.comm
			pid = info.pid
		}

		ports = append(ports, ListeningPort{
			Port:     int(port),
			Protocol: protocol,
			Process:  process,
			PID:      pid,
		})
	}

	return ports
}

// =============================================================================
// Process resolution via /proc
// =============================================================================

type pidInfo struct {
	pid  int
	comm string
}

// buildInodePIDMap scans /proc/{pid}/fd to map socket inodes to PIDs and process names.
// This is best-effort — if we can't read a /proc entry (permissions, race), we skip it.
func buildInodePIDMap() map[string]pidInfo {
	result := make(map[string]pidInfo)

	procDir, err := os.Open("/proc")
	if err != nil {
		return result
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(-1)
	if err != nil {
		return result
	}

	for _, entry := range entries {
		// Only numeric entries are PIDs
		pid, err := strconv.Atoi(entry)
		if err != nil {
			continue
		}

		pidPath := filepath.Join("/proc", entry)

		// Read process name
		commBytes, err := os.ReadFile(filepath.Join(pidPath, "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commBytes))

		// Scan file descriptors for socket inodes
		fdPath := filepath.Join(pidPath, "fd")
		fdDir, err := os.Open(fdPath)
		if err != nil {
			continue
		}

		fdEntries, err := fdDir.Readdirnames(-1)
		fdDir.Close()
		if err != nil {
			continue
		}

		for _, fd := range fdEntries {
			link, err := os.Readlink(filepath.Join(fdPath, fd))
			if err != nil {
				continue
			}
			// Socket links look like: socket:[12345]
			if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
				inode := link[8 : len(link)-1]
				result[inode] = pidInfo{pid: pid, comm: comm}
			}
		}
	}

	return result
}

// =============================================================================
// Deduplication
// =============================================================================

// deduplicatePorts removes duplicate port+protocol entries, keeping the first
// occurrence (which typically has better process info from the IPv4 entry).
func deduplicatePorts(ports []ListeningPort) []ListeningPort {
	type key struct {
		port     int
		protocol string
	}
	seen := make(map[key]bool)
	var result []ListeningPort

	for _, p := range ports {
		k := key{port: p.Port, protocol: p.Protocol}
		if seen[k] {
			// If the existing entry has no process info but this one does, prefer this one
			for i := range result {
				if result[i].Port == p.Port && result[i].Protocol == p.Protocol {
					if result[i].Process == "" && p.Process != "" {
						result[i] = p
					}
					break
				}
			}
			continue
		}
		seen[k] = true
		result = append(result, p)
	}

	return result
}

// =============================================================================
// Utility — not needed but documents the hex-to-IP conversion if ever needed
// =============================================================================

// hexToIPv4 converts a /proc/net hex IP to dotted notation (unused but documented).
// e.g., "0100007F" → "127.0.0.1" (note: /proc/net stores in little-endian on little-endian hosts)
func hexToIPv4(hex string) string {
	if len(hex) != 8 {
		return hex
	}
	b := make([]byte, 4)
	for i := 0; i < 4; i++ {
		val, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return hex
		}
		b[i] = byte(val)
	}
	// /proc/net stores in host byte order (little-endian on ARM/x86)
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0])
}
