package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// Cellular Types
// ============================================================================

// CellularModem represents a cellular modem.
// @Description Cellular modem information
type CellularModem struct {
	Index         int    `json:"index" example:"0"`
	Path          string `json:"path" example:"/org/freedesktop/ModemManager1/Modem/0"`
	Manufacturer  string `json:"manufacturer" example:"Quectel"`
	Model         string `json:"model" example:"EC25"`
	Revision      string `json:"revision,omitempty"`
	IMEI          string `json:"imei,omitempty" example:"123456789012345"`
	State         string `json:"state" example:"connected"`
	PowerState    string `json:"power_state" example:"on"`
	SignalQuality int    `json:"signal_quality" example:"75"`
	AccessTech    string `json:"access_tech" example:"lte"`
	Operator      string `json:"operator,omitempty" example:"T-Mobile"`
	EquipmentID   string `json:"equipment_id,omitempty"`
}

// CellularStatus represents cellular status.
// @Description Overall cellular status
type CellularStatus struct {
	Available   bool            `json:"available" example:"true"`
	Connected   bool            `json:"connected" example:"true"`
	Modems      []CellularModem `json:"modems"`
	ModemCount  int             `json:"modem_count" example:"1"`
	ActiveModem string          `json:"active_modem,omitempty" example:"/org/freedesktop/ModemManager1/Modem/0"`
}

// CellularSignal represents cellular signal info.
// @Description Cellular signal strength and quality
type CellularSignal struct {
	Quality   int     `json:"quality" example:"75"`
	RSSI      float64 `json:"rssi,omitempty" example:"-65.0"`
	RSRP      float64 `json:"rsrp,omitempty" example:"-95.0"`
	RSRQ      float64 `json:"rsrq,omitempty" example:"-10.0"`
	SNR       float64 `json:"snr,omitempty" example:"15.0"`
	Bars      int     `json:"bars" example:"4"`
	Tech      string  `json:"tech" example:"lte"`
	Band      string  `json:"band,omitempty" example:"Band 7"`
	Frequency int     `json:"frequency,omitempty" example:"2600"`
}

// CellularConnectRequest represents cellular connection request.
// @Description Cellular connection parameters
type CellularConnectRequest struct {
	ModemIndex int    `json:"modem_index" example:"0"`
	APN        string `json:"apn" example:"internet"`
	User       string `json:"user,omitempty"`
	Password   string `json:"password,omitempty"`
	PIN        string `json:"pin,omitempty"`
}

// AndroidTetheringStatus represents Android USB tethering status.
// @Description Android USB tethering status
type AndroidTetheringStatus struct {
	Available bool   `json:"available" example:"true"`
	Connected bool   `json:"connected" example:"true"`
	Interface string `json:"interface,omitempty" example:"usb0"`
	IPAddress string `json:"ip_address,omitempty" example:"192.168.42.129"`
	Gateway   string `json:"gateway,omitempty" example:"192.168.42.129"`
}

// ============================================================================
// Cellular Handlers
// ============================================================================

// GetCellularStatus returns cellular status.
// @Summary Get cellular status
// @Description Returns overall cellular modem status via ModemManager
// @Tags Cellular
// @Accept json
// @Produce json
// @Success 200 {object} CellularStatus
// @Failure 500 {object} ErrorResponse
// @Router /cellular/status [get]
func (h *HALHandler) GetCellularStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := CellularStatus{
		Available: false,
		Modems:    []CellularModem{},
	}

	// HF05-11: Check if ModemManager is running with timeout
	if _, err := execWithTimeout(ctx, "systemctl", "is-active", "ModemManager"); err != nil {
		jsonResponse(w, http.StatusOK, status)
		return
	}

	// List modems
	modems := h.listModems(ctx)
	status.Modems = modems
	status.ModemCount = len(modems)
	status.Available = len(modems) > 0

	// Check if any modem is connected
	for _, m := range modems {
		if m.State == "connected" {
			status.Connected = true
			status.ActiveModem = m.Path
			break
		}
	}

	jsonResponse(w, http.StatusOK, status)
}

// GetCellularModems lists cellular modems.
// @Summary List cellular modems
// @Description Returns list of detected cellular modems
// @Tags Cellular
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /cellular/modems [get]
func (h *HALHandler) GetCellularModems(w http.ResponseWriter, r *http.Request) {
	modems := h.listModems(r.Context())
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":  len(modems),
		"modems": modems,
	})
}

// GetCellularSignal returns cellular signal info.
// @Summary Get cellular signal
// @Description Returns cellular signal strength and quality
// @Tags Cellular
// @Accept json
// @Produce json
// @Param modem query int false "Modem index (0-15)" default(0)
// @Success 200 {object} CellularSignal
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /cellular/signal [get]
func (h *HALHandler) GetCellularSignal(w http.ResponseWriter, r *http.Request) {
	modemParam := r.URL.Query().Get("modem")
	modemIdx := 0
	if modemParam != "" {
		idx, err := strconv.Atoi(modemParam)
		if err != nil || idx < 0 || idx > 15 {
			errorResponse(w, http.StatusBadRequest, "invalid modem index (0-15)")
			return
		}
		modemIdx = idx
	}

	signal := h.getModemSignal(r.Context(), modemIdx)
	jsonResponse(w, http.StatusOK, signal)
}

// ConnectCellular connects a cellular modem.
// @Summary Connect cellular
// @Description Connects a cellular modem with specified APN
// @Tags Cellular
// @Accept json
// @Produce json
// @Param request body CellularConnectRequest true "Connection parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /cellular/connect [post]
func (h *HALHandler) ConnectCellular(w http.ResponseWriter, r *http.Request) {
	// HF05-09: Apply body limit
	r = limitBody(r, 1<<20)

	var req CellularConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// HF05-07: Validate APN
	if err := validateAPN(req.APN); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF05-07: Validate credentials — reject injection characters
	if strings.ContainsAny(req.User, ",\n\r\x00") || len(req.User) > 128 {
		errorResponse(w, http.StatusBadRequest, "invalid username")
		return
	}
	if strings.ContainsAny(req.Password, ",\n\r\x00") || len(req.Password) > 128 {
		errorResponse(w, http.StatusBadRequest, "invalid password")
		return
	}
	if req.PIN != "" && (len(req.PIN) < 4 || len(req.PIN) > 8 || !isDigitsOnly(req.PIN)) {
		errorResponse(w, http.StatusBadRequest, "invalid PIN (must be 4-8 digits)")
		return
	}

	// Validate modem index range
	if req.ModemIndex < 0 || req.ModemIndex > 15 {
		errorResponse(w, http.StatusBadRequest, "invalid modem index (0-15)")
		return
	}

	modemPath := fmt.Sprintf("/org/freedesktop/ModemManager1/Modem/%d", req.ModemIndex)

	// Build mmcli command
	args := []string{"-m", modemPath, "--simple-connect", fmt.Sprintf("apn=%s", req.APN)}
	if req.User != "" {
		args = append(args, fmt.Sprintf("user=%s", req.User))
	}
	if req.Password != "" {
		args = append(args, fmt.Sprintf("password=%s", req.Password))
	}

	// HF05-11: Use execWithTimeout with 60s for modem connection
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := execWithTimeout(ctx, "mmcli", args...); err != nil {
		log.Printf("cellular: connect failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("cellular connect", err))
		return
	}

	successResponse(w, "cellular connected")
}

// DisconnectCellular disconnects a cellular modem.
// @Summary Disconnect cellular
// @Description Disconnects a cellular modem
// @Tags Cellular
// @Accept json
// @Produce json
// @Param modem path int true "Modem index" example(0)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /cellular/disconnect/{modem} [post]
func (h *HALHandler) DisconnectCellular(w http.ResponseWriter, r *http.Request) {
	modemParam := chi.URLParam(r, "modem")
	modemIdx, err := strconv.Atoi(modemParam)
	if err != nil || modemIdx < 0 || modemIdx > 15 {
		errorResponse(w, http.StatusBadRequest, "invalid modem index (0-15)")
		return
	}

	modemPath := fmt.Sprintf("/org/freedesktop/ModemManager1/Modem/%d", modemIdx)

	// HF05-11: Use execWithTimeout
	if _, err := execWithTimeout(r.Context(), "mmcli", "-m", modemPath, "--simple-disconnect"); err != nil {
		log.Printf("cellular: disconnect failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("cellular disconnect", err))
		return
	}

	successResponse(w, "cellular disconnected")
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) listModems(ctx context.Context) []CellularModem {
	var modems []CellularModem

	// HF05-11: Get modem list from mmcli with timeout
	output, err := execWithTimeout(ctx, "mmcli", "-L")
	if err != nil {
		return modems
	}

	// Parse modem paths
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "/org/freedesktop/ModemManager1/Modem/") {
			// Extract modem path
			start := strings.Index(line, "/org/")
			if start == -1 {
				continue
			}

			pathEnd := strings.Index(line[start:], " ")
			var path string
			if pathEnd == -1 {
				path = strings.TrimSpace(line[start:])
			} else {
				path = line[start : start+pathEnd]
			}

			// Get modem details
			modem := h.getModemInfo(ctx, path)
			modems = append(modems, modem)
		}
	}

	return modems
}

func (h *HALHandler) getModemInfo(ctx context.Context, path string) CellularModem {
	modem := CellularModem{
		Path: path,
	}

	// Extract index from path
	parts := strings.Split(path, "/")
	if len(parts) > 0 {
		if idx, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			modem.Index = idx
		}
	}

	// HF05-11: Get detailed info with timeout
	output, err := execWithTimeout(ctx, "mmcli", "-m", path)
	if err != nil {
		return modem
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "manufacturer:") {
			modem.Manufacturer = strings.TrimSpace(strings.TrimPrefix(line, "manufacturer:"))
		}
		if strings.HasPrefix(line, "model:") {
			modem.Model = strings.TrimSpace(strings.TrimPrefix(line, "model:"))
		}
		if strings.HasPrefix(line, "revision:") {
			modem.Revision = strings.TrimSpace(strings.TrimPrefix(line, "revision:"))
		}
		if strings.HasPrefix(line, "equipment id:") {
			modem.EquipmentID = strings.TrimSpace(strings.TrimPrefix(line, "equipment id:"))
			modem.IMEI = modem.EquipmentID
		}
		if strings.HasPrefix(line, "state:") {
			modem.State = strings.TrimSpace(strings.TrimPrefix(line, "state:"))
		}
		if strings.HasPrefix(line, "power state:") {
			modem.PowerState = strings.TrimSpace(strings.TrimPrefix(line, "power state:"))
		}
		if strings.HasPrefix(line, "signal quality:") {
			sigStr := strings.TrimSpace(strings.TrimPrefix(line, "signal quality:"))
			sigStr = strings.TrimSuffix(sigStr, "%")
			sigStr = strings.Split(sigStr, " ")[0]
			modem.SignalQuality, _ = strconv.Atoi(sigStr)
		}
		if strings.HasPrefix(line, "access tech:") {
			modem.AccessTech = strings.TrimSpace(strings.TrimPrefix(line, "access tech:"))
		}
		if strings.HasPrefix(line, "operator name:") {
			modem.Operator = strings.TrimSpace(strings.TrimPrefix(line, "operator name:"))
		}
	}

	return modem
}

func (h *HALHandler) getModemSignal(ctx context.Context, modemIdx int) CellularSignal {
	signal := CellularSignal{}

	modemPath := fmt.Sprintf("/org/freedesktop/ModemManager1/Modem/%d", modemIdx)

	// HF05-11: Get signal info with timeout
	output, err := execWithTimeout(ctx, "mmcli", "-m", modemPath, "--signal-get")
	if err != nil {
		// Fallback to basic signal quality
		modem := h.getModemInfo(ctx, modemPath)
		signal.Quality = modem.SignalQuality
		signal.Bars = signal.Quality / 25
		if signal.Bars > 4 {
			signal.Bars = 4
		}
		signal.Tech = modem.AccessTech
		return signal
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "rssi:") {
			valStr := strings.TrimSpace(strings.TrimPrefix(line, "rssi:"))
			valStr = strings.TrimSuffix(valStr, " dBm")
			signal.RSSI, _ = strconv.ParseFloat(valStr, 64)
		}
		if strings.HasPrefix(line, "rsrp:") {
			valStr := strings.TrimSpace(strings.TrimPrefix(line, "rsrp:"))
			valStr = strings.TrimSuffix(valStr, " dBm")
			signal.RSRP, _ = strconv.ParseFloat(valStr, 64)
		}
		if strings.HasPrefix(line, "rsrq:") {
			valStr := strings.TrimSpace(strings.TrimPrefix(line, "rsrq:"))
			valStr = strings.TrimSuffix(valStr, " dB")
			signal.RSRQ, _ = strconv.ParseFloat(valStr, 64)
		}
		if strings.HasPrefix(line, "snr:") {
			valStr := strings.TrimSpace(strings.TrimPrefix(line, "snr:"))
			valStr = strings.TrimSuffix(valStr, " dB")
			signal.SNR, _ = strconv.ParseFloat(valStr, 64)
		}
	}

	// Calculate quality from RSSI if available
	if signal.RSSI != 0 {
		// Map RSSI to 0-100 quality
		// -50 dBm = excellent (100%), -100 dBm = no signal (0%)
		quality := int((signal.RSSI + 100) * 2)
		if quality > 100 {
			quality = 100
		}
		if quality < 0 {
			quality = 0
		}
		signal.Quality = quality
	}

	signal.Bars = signal.Quality / 25
	if signal.Bars > 4 {
		signal.Bars = 4
	}

	return signal
}

// GetAndroidTetheringStatus returns Android USB tethering status.
// @Router /cellular/android/status [get]
func (h *HALHandler) GetAndroidTetheringStatus(w http.ResponseWriter, r *http.Request) {
	status := h.checkAndroidTethering(r.Context())
	jsonResponse(w, http.StatusOK, status)
}

// EnableAndroidTethering enables Android USB tethering.
// @Router /cellular/android/enable [post]
func (h *HALHandler) EnableAndroidTethering(w http.ResponseWriter, r *http.Request) {
	status := h.checkAndroidTethering(r.Context())
	if !status.Connected {
		errorResponse(w, http.StatusBadRequest, "no Android device detected")
		return
	}
	// HF05-11: Use execWithTimeout
	if _, err := execWithTimeout(r.Context(), "dhclient", status.Interface); err != nil {
		log.Printf("cellular: dhclient failed on %s: %v", status.Interface, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("DHCP", err))
		return
	}
	successResponse(w, "Android tethering enabled on "+status.Interface)
}

// DisableAndroidTethering disables Android USB tethering.
// @Router /cellular/android/disable [post]
func (h *HALHandler) DisableAndroidTethering(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	for _, iface := range []string{"usb0", "rndis0"} {
		execWithTimeout(ctx, "dhclient", "-r", iface)
		execWithTimeout(ctx, "ip", "link", "set", iface, "down")
	}
	successResponse(w, "Android tethering disabled")
}

// checkAndroidTethering checks for Android USB tethering interface
func (h *HALHandler) checkAndroidTethering(ctx context.Context) AndroidTetheringStatus {
	status := AndroidTetheringStatus{}

	interfaces := []string{"usb0", "rndis0", "enp0s20f0u1"}
	for _, iface := range interfaces {
		if output, err := execWithTimeout(ctx, "ip", "addr", "show", iface); err == nil {
			status.Interface = iface
			status.Connected = true

			lines := strings.Split(output, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "inet ") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						status.IPAddress = strings.Split(fields[1], "/")[0]
					}
				}
			}
			break
		}
	}
	return status
}

// isDigitsOnly checks if a string contains only digit characters.
func isDigitsOnly(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
