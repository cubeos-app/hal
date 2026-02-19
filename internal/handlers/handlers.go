// Package handlers provides HTTP handlers for the CubeOS HAL API.
package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// HALHandler handles all HAL API endpoints.
type HALHandler struct {
	powerMonitor *PowerMonitor
	iridium      *IridiumDriver
	meshtastic   *MeshtasticDriver

	// Stream process tracking (camera)
	streamMu     sync.Mutex
	streamCmd    *exec.Cmd
	streamCancel func()
}

// NewHALHandler creates a new HAL handler instance.
func NewHALHandler() *HALHandler {
	return &HALHandler{
		powerMonitor: NewPowerMonitor(),
		iridium:      NewIridiumDriver(),
		meshtastic:   NewMeshtasticDriver(),
	}
}

// Close cleans up HAL resources (stream processes, etc.).
func (h *HALHandler) Close() {
	h.stopStreamProcess()
}

// PowerMonitorRef returns a reference to the power monitor for shutdown wiring.
func (h *HALHandler) PowerMonitorRef() *PowerMonitor {
	return h.powerMonitor
}

// ErrorResponse represents an error response.
type ErrorResponse struct {
	Error string `json:"error" example:"resource not found"`
	Code  int    `json:"code" example:"404"`
}

// SuccessResponse represents a success response.
type SuccessResponse struct {
	Status  string `json:"status" example:"ok"`
	Message string `json:"message" example:"operation completed"`
}

// jsonResponse writes a JSON response with the given status code.
func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("jsonResponse: failed to encode response: %v", err)
	}
}

// errorResponse writes a JSON error response.
func errorResponse(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]interface{}{
		"error": message,
		"code":  status,
	})
}

// successResponse writes a JSON success response.
func successResponse(w http.ResponseWriter, message string) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"message": message,
	})
}

// formatBytes converts bytes to human-readable format.
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// formatBytesUint64 converts uint64 bytes to human-readable format.
func formatBytesUint64(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// readFileString reads a file and returns its contents as a trimmed string.
func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
