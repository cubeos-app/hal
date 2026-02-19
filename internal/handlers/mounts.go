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
// Mount Types
// ============================================================================

// NetworkMount represents a network mount.
// @Description Network mount (SMB/NFS)
type NetworkMount struct {
	Type       string `json:"type" example:"smb"`
	Source     string `json:"source" example:"//192.168.1.100/share"`
	Mountpoint string `json:"mountpoint" example:"/mnt/nas"`
	Options    string `json:"options,omitempty" example:"username=user,password=***"`
	Mounted    bool   `json:"mounted" example:"true"`
	Available  int64  `json:"available,omitempty"`
	Used       int64  `json:"used,omitempty"`
	Total      int64  `json:"total,omitempty"`
}

// NetworkMountsResponse represents network mounts list.
// @Description List of network mounts
type NetworkMountsResponse struct {
	Count  int            `json:"count" example:"2"`
	Mounts []NetworkMount `json:"mounts"`
}

// SMBMountRequest represents SMB mount request.
// @Description SMB mount parameters
type SMBMountRequest struct {
	Server     string `json:"server" example:"192.168.1.100"`
	Share      string `json:"share" example:"share"`
	Mountpoint string `json:"mountpoint" example:"/mnt/nas"`
	Username   string `json:"username,omitempty" example:"user"`
	Password   string `json:"password,omitempty" example:"secret"`
	Domain     string `json:"domain,omitempty" example:"WORKGROUP"`
	Version    string `json:"version,omitempty" example:"3.0"`
}

// NFSMountRequest represents NFS mount request.
// @Description NFS mount parameters
type NFSMountRequest struct {
	Server     string `json:"server" example:"192.168.1.100"`
	Export     string `json:"export" example:"/export/share"`
	Mountpoint string `json:"mountpoint" example:"/mnt/nfs"`
	Options    string `json:"options,omitempty" example:"ro,noatime"`
	Version    string `json:"version,omitempty" example:"4"`
}

// UnmountRequest represents unmount request.
// @Description Unmount parameters
type UnmountRequest struct {
	Mountpoint string `json:"mountpoint" example:"/mnt/nas"`
	Force      bool   `json:"force,omitempty" example:"false"`
	Lazy       bool   `json:"lazy,omitempty" example:"false"`
}

// CheckSMBRequest represents SMB server check request.
// @Description SMB server check parameters (password in body, not query string)
type CheckSMBRequest struct {
	Server   string `json:"server" example:"192.168.1.100"`
	Username string `json:"username,omitempty" example:"user"`
	Password string `json:"password,omitempty" example:"secret"`
}

// ============================================================================
// Network Mount Handlers
// ============================================================================

// GetNetworkMounts lists network mounts.
// @Summary List network mounts
// @Description Returns list of active network mounts (SMB/NFS)
// @Tags Mounts
// @Accept json
// @Produce json
// @Success 200 {object} NetworkMountsResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts [get]
func (h *HALHandler) GetNetworkMounts(w http.ResponseWriter, r *http.Request) {
	mounts := h.scanNetworkMounts()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":  len(mounts),
		"mounts": mounts,
	})
}

// MountSMB mounts an SMB share.
// @Summary Mount SMB share
// @Description Mounts a Windows/Samba network share
// @Tags Mounts
// @Accept json
// @Produce json
// @Param request body SMBMountRequest true "SMB mount parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts/smb [post]
func (h *HALHandler) MountSMB(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)
	var req SMBMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate server
	if err := validateHostnameOrIP(req.Server); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate share name
	if err := validateShareName(req.Share); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate mountpoint (restricts to /mnt/ or /media/) — HF03-05/08
	if err := validateMountpoint(req.Mountpoint); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate SMB version
	if err := validateSMBVersion(req.Version); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate credential fields against comma injection — HF03-03
	if req.Username != "" {
		if err := validateSMBCredentialField(req.Username, "username"); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Password != "" {
		if err := validateSMBCredentialField(req.Password, "password"); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Domain != "" {
		if err := validateSMBCredentialField(req.Domain, "domain"); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Create mountpoint (already validated to be under /mnt/ or /media/)
	if err := os.MkdirAll(req.Mountpoint, 0755); err != nil {
		log.Printf("MountSMB MkdirAll(%s): %v", req.Mountpoint, err)
		errorResponse(w, http.StatusInternalServerError, "failed to create mountpoint")
		return
	}

	// Build mount options
	source := fmt.Sprintf("//%s/%s", req.Server, req.Share)
	options := []string{}

	if req.Username != "" {
		options = append(options, "username="+req.Username)
		if req.Password != "" {
			options = append(options, "password="+req.Password)
		}
	} else {
		options = append(options, "guest")
	}

	if req.Domain != "" {
		options = append(options, "domain="+req.Domain)
	}

	if req.Version != "" {
		options = append(options, "vers="+req.Version)
	}

	// Add common options
	options = append(options, "iocharset=utf8")

	optString := strings.Join(options, ",")

	// Mount
	out, err := execWithTimeout(r.Context(), "mount", "-t", "cifs", "-o", optString, source, req.Mountpoint)
	if err != nil {
		log.Printf("MountSMB mount(%s -> %s): %v: %s", source, req.Mountpoint, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("SMB mount", err))
		return
	}

	successResponse(w, fmt.Sprintf("mounted %s at %s", source, req.Mountpoint))
}

// MountNFS mounts an NFS share.
// @Summary Mount NFS share
// @Description Mounts an NFS network share
// @Tags Mounts
// @Accept json
// @Produce json
// @Param request body NFSMountRequest true "NFS mount parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts/nfs [post]
func (h *HALHandler) MountNFS(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)
	var req NFSMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate server
	if err := validateHostnameOrIP(req.Server); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate export path
	if err := validateExportPath(req.Export); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate mountpoint (restricts to /mnt/ or /media/) — HF03-05/08
	if err := validateMountpoint(req.Mountpoint); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate NFS options against allowlist — HF03-04
	if err := validateNFSOptions(req.Options); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate NFS version
	if err := validateNFSVersion(req.Version); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Create mountpoint (already validated to be under /mnt/ or /media/)
	if err := os.MkdirAll(req.Mountpoint, 0755); err != nil {
		log.Printf("MountNFS MkdirAll(%s): %v", req.Mountpoint, err)
		errorResponse(w, http.StatusInternalServerError, "failed to create mountpoint")
		return
	}

	// Build source
	source := fmt.Sprintf("%s:%s", req.Server, req.Export)

	// Build options
	options := []string{}
	if req.Options != "" {
		options = append(options, req.Options)
	}
	if req.Version != "" {
		options = append(options, "nfsvers="+req.Version)
	}

	args := []string{"-t", "nfs"}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, source, req.Mountpoint)

	// Mount
	out, err := execWithTimeout(r.Context(), "mount", args...)
	if err != nil {
		log.Printf("MountNFS mount(%s -> %s): %v: %s", source, req.Mountpoint, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("NFS mount", err))
		return
	}

	successResponse(w, fmt.Sprintf("mounted %s at %s", source, req.Mountpoint))
}

// UnmountNetwork unmounts a network share.
// @Summary Unmount network share
// @Description Unmounts a network share (SMB/NFS)
// @Tags Mounts
// @Accept json
// @Produce json
// @Param request body UnmountRequest true "Unmount parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts/unmount [post]
func (h *HALHandler) UnmountNetwork(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)
	var req UnmountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate mountpoint — restrict to /mnt/ and /media/ only (HF03-05)
	if err := validateMountpoint(req.Mountpoint); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Sync first
	execWithTimeout(r.Context(), "sync")

	// Build umount command
	args := []string{}
	if req.Force {
		args = append(args, "-f")
	}
	if req.Lazy {
		args = append(args, "-l")
	}
	args = append(args, req.Mountpoint)

	out, err := execWithTimeout(r.Context(), "umount", args...)
	if err != nil {
		log.Printf("UnmountNetwork(%s): %v: %s", req.Mountpoint, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("unmount", err))
		return
	}

	successResponse(w, fmt.Sprintf("unmounted %s", req.Mountpoint))
}

// CheckSMBServer checks if an SMB server is available.
// @Summary Check SMB server
// @Description Checks if an SMB server is reachable and lists shares
// @Tags Mounts
// @Accept json
// @Produce json
// @Param request body CheckSMBRequest true "SMB check parameters"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts/smb/check [post]
func (h *HALHandler) CheckSMBServer(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)
	var req CheckSMBRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate server
	if err := validateHostnameOrIP(req.Server); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Use smbclient to list shares
	args := []string{"-L", req.Server, "-N"} // -N for no password
	if req.Username != "" {
		if err := validateSMBCredentialField(req.Username, "username"); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Password != "" {
			if err := validateSMBCredentialField(req.Password, "password"); err != nil {
				errorResponse(w, http.StatusBadRequest, err.Error())
				return
			}
			args = []string{"-L", req.Server, "-U", req.Username + "%" + req.Password}
		} else {
			args = []string{"-L", req.Server, "-U", req.Username}
		}
	}

	out, err := execWithTimeout(r.Context(), "smbclient", args...)
	if err != nil {
		log.Printf("CheckSMBServer(%s): %v: %s", req.Server, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("SMB check", err))
		return
	}

	// Parse shares from output
	var shares []string
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Disk") || strings.Contains(line, "IPC") || strings.Contains(line, "Printer") {
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				shares = append(shares, parts[0])
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"server":    req.Server,
		"available": true,
		"shares":    shares,
	})
}

// CheckNFSServer checks if an NFS server is available.
// @Summary Check NFS server
// @Description Checks if an NFS server is reachable and lists exports
// @Tags Mounts
// @Accept json
// @Produce json
// @Param server query string true "Server address" example(192.168.1.100)
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts/nfs/check [get]
func (h *HALHandler) CheckNFSServer(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	if err := validateHostnameOrIP(server); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Use showmount to list exports
	out, err := execWithTimeout(r.Context(), "showmount", "-e", server)
	if err != nil {
		log.Printf("CheckNFSServer(%s): %v: %s", server, err, out)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("NFS check", err))
		return
	}

	// Parse exports from output
	var exports []string
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if i == 0 { // Skip header
			continue
		}
		line = strings.TrimSpace(line)
		if line != "" {
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				exports = append(exports, parts[0])
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"server":    server,
		"available": true,
		"exports":   exports,
	})
}

// TestMountConnection tests connectivity to a remote share
// @Summary Test mount connection
// @Description Tests if a remote SMB/NFS share is reachable without actually mounting it
// @Tags Mounts
// @Accept json
// @Produce json
// @Param request body object true "Mount test parameters" example({"type":"smb","remote_path":"//server/share","username":"user","password":"secret"})
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /mounts/test [post]
func (h *HALHandler) TestMountConnection(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<20)

	var req struct {
		Type       string `json:"type"`
		RemotePath string `json:"remote_path"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Type == "" || req.RemotePath == "" {
		errorResponse(w, http.StatusBadRequest, "type and remote_path are required")
		return
	}

	switch req.Type {
	case "smb":
		// Extract server from //server/share format
		parts := strings.SplitN(strings.TrimPrefix(req.RemotePath, "//"), "/", 2)
		if len(parts) < 1 || parts[0] == "" {
			errorResponse(w, http.StatusBadRequest, "invalid SMB path format (expected //server/share)")
			return
		}
		server := parts[0]
		if err := validateHostnameOrIP(server); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		// Use smbclient to test connectivity
		args := []string{"-L", server, "-N"}
		if req.Username != "" {
			if err := validateSMBCredentialField(req.Username, "username"); err != nil {
				errorResponse(w, http.StatusBadRequest, err.Error())
				return
			}
			if req.Password != "" {
				if err := validateSMBCredentialField(req.Password, "password"); err != nil {
					errorResponse(w, http.StatusBadRequest, err.Error())
					return
				}
				args = []string{"-L", server, "-U", req.Username + "%" + req.Password}
			} else {
				args = []string{"-L", server, "-U", req.Username}
			}
		}

		_, err := execWithTimeout(r.Context(), "smbclient", args...)
		if err != nil {
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"reachable":   false,
				"type":        req.Type,
				"remote_path": req.RemotePath,
				"error":       sanitizeExecError("SMB test", err),
			})
			return
		}

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"reachable":   true,
			"type":        req.Type,
			"remote_path": req.RemotePath,
		})

	case "nfs":
		// Extract server from server:/export format
		parts := strings.SplitN(req.RemotePath, ":", 2)
		if len(parts) < 1 || parts[0] == "" {
			errorResponse(w, http.StatusBadRequest, "invalid NFS path format (expected server:/export)")
			return
		}
		server := parts[0]
		if err := validateHostnameOrIP(server); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		_, err := execWithTimeout(r.Context(), "showmount", "-e", server)
		if err != nil {
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"reachable":   false,
				"type":        req.Type,
				"remote_path": req.RemotePath,
				"error":       sanitizeExecError("NFS test", err),
			})
			return
		}

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"reachable":   true,
			"type":        req.Type,
			"remote_path": req.RemotePath,
		})

	default:
		errorResponse(w, http.StatusBadRequest, "type must be 'smb' or 'nfs'")
	}
}

// CheckMounted checks if a path is currently mounted
// @Summary Check if path is mounted
// @Description Checks if the specified path is an active mount point
// @Tags Mounts
// @Produce json
// @Param path query string true "Path to check" example(/mnt/nas)
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Router /mounts/check [get]
func (h *HALHandler) CheckMounted(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		errorResponse(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validateMountpoint(path); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check /proc/mounts for the path
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		log.Printf("CheckMounted: read /proc/mounts: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to read mount table")
		return
	}

	mounted := false
	var fsType, source string
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == path {
			mounted = true
			source = fields[0]
			fsType = fields[2]
			break
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"mounted": mounted,
		"path":    path,
		"source":  source,
		"fstype":  fsType,
	})
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) scanNetworkMounts() []NetworkMount {
	var mounts []NetworkMount

	// Read /proc/mounts
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return mounts
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		source := fields[0]
		mountpoint := fields[1]
		fstype := fields[2]

		// Check if it's a network mount
		if fstype == "cifs" || fstype == "nfs" || fstype == "nfs4" {
			mount := NetworkMount{
				Source:     source,
				Mountpoint: mountpoint,
				Mounted:    true,
			}

			if fstype == "cifs" {
				mount.Type = "smb"
			} else {
				mount.Type = "nfs"
			}

			// Get usage stats
			if usage := h.getMountUsage(mountpoint); usage != nil {
				mount.Total = usage["total"]
				mount.Used = usage["used"]
				mount.Available = usage["available"]
			}

			mounts = append(mounts, mount)
		}
	}

	return mounts
}

func (h *HALHandler) getMountUsage(mountpoint string) map[string]int64 {
	ctx := context.Background()
	out, err := execWithTimeout(ctx, "df", "-B1", mountpoint)
	if err != nil {
		return nil
	}

	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return nil
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return nil
	}

	total, _ := parseInt64(fields[1])
	used, _ := parseInt64(fields[2])
	available, _ := parseInt64(fields[3])

	return map[string]int64{
		"total":     total,
		"used":      used,
		"available": available,
	}
}

func parseInt64(s string) (int64, error) {
	var val int64
	_, err := fmt.Sscanf(s, "%d", &val)
	return val, err
}
