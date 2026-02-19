package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// Iridium Response Types
// ============================================================================

// IridiumStatusResponse represents the full modem status.
// @Description Iridium satellite modem status
type IridiumStatusResponse struct {
	Connected bool   `json:"connected" example:"true"`
	Port      string `json:"port" example:"/dev/ttyUSB1"`
	IMEI      string `json:"imei,omitempty" example:"300234010123456"`
	Model     string `json:"model,omitempty" example:"IRIDIUM 9600 Family SBD Transceiver"`
	Signal    int    `json:"signal" example:"3"`
	MOFlag    bool   `json:"mo_flag" example:"false"`
	MTFlag    bool   `json:"mt_flag" example:"false"`
	MTQueued  int    `json:"mt_queued" example:"0"`
	LastCheck string `json:"last_check" example:"2026-02-08T12:00:00Z"`
}

// IridiumSignalResponse represents signal quality info.
// @Description Iridium satellite signal strength
type IridiumSignalResponse struct {
	Strength    int    `json:"strength" example:"3"`
	Description string `json:"description" example:"Good (~-106 dBm)"`
}

// IridiumSendRequest represents an SBD message to send.
// @Description Iridium SBD message parameters
type IridiumSendRequest struct {
	Data   string `json:"data" example:"SGVsbG8gV29ybGQ="`           // Base64-encoded for binary
	Text   string `json:"text,omitempty" example:"Hello World"`      // Plain text (max 120 chars)
	Format string `json:"format" example:"text" enums:"text,binary"` // "text" or "binary"
}

// IridiumSendResponse represents the result of sending an SBD message.
// @Description SBD send result
type IridiumSendResponse struct {
	Status     string `json:"status" example:"sent"`
	MOStatus   int    `json:"mo_status" example:"0"`
	MOMSN      int    `json:"momsn" example:"42"`
	MTReceived bool   `json:"mt_received" example:"false"`
	MTQueued   int    `json:"mt_queued" example:"0"`
}

// IridiumMailboxResponse represents a mailbox check result.
// @Description Iridium mailbox check result
type IridiumMailboxResponse struct {
	MTReceived bool    `json:"mt_received" example:"true"`
	MTMessage  *string `json:"mt_message,omitempty"`
	MTQueued   int     `json:"mt_queued" example:"0"`
}

// IridiumReceiveResponse represents the MT buffer contents.
// @Description Iridium MT buffer contents
type IridiumReceiveResponse struct {
	Data   string `json:"data,omitempty" example:"SGVsbG8="`
	Length int    `json:"length" example:"5"`
	Format string `json:"format" example:"binary"`
}

// IridiumClearRequest specifies which buffer to clear.
// @Description Buffer clear request
type IridiumClearRequest struct {
	Buffer string `json:"buffer" example:"both" enums:"mo,mt,both"`
}

// ============================================================================
// Handlers
// ============================================================================

// GetIridiumDevices lists detected Iridium modems.
// @Summary List Iridium devices
// @Description Scans USB serial ports for Iridium satellite modems
// @Tags Iridium
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /iridium/devices [get]
func (h *HALHandler) GetIridiumDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.iridium.ScanDevices(r.Context())
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// GetIridiumStatus returns the full modem status.
// @Summary Get Iridium modem status
// @Description Returns connection state, IMEI, signal, and message queue status
// @Tags Iridium
// @Produce json
// @Param port query string false "Serial port (connects if not already connected)" default(/dev/ttyUSB1)
// @Success 200 {object} IridiumStatusResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/status [get]
func (h *HALHandler) GetIridiumStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Auto-connect if not connected
	if !h.iridium.IsConnected() {
		port := r.URL.Query().Get("port")
		if err := h.iridium.Connect(ctx, port); err != nil {
			jsonResponse(w, http.StatusOK, IridiumStatusResponse{
				Connected: false,
				LastCheck: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
	}

	resp := IridiumStatusResponse{
		Connected: true,
		Port:      h.iridium.Port(),
		IMEI:      h.iridium.IMEI(),
		Model:     h.iridium.Model(),
		LastCheck: time.Now().UTC().Format(time.RFC3339),
	}

	// Get signal
	sig, _, err := h.iridium.GetSignal(ctx)
	if err != nil {
		log.Printf("iridium: signal query failed: %v", err)
	} else {
		resp.Signal = sig
	}

	// Get SBD status
	sbdStatus, err := h.iridium.GetSBDStatus(ctx)
	if err != nil {
		log.Printf("iridium: SBD status query failed: %v", err)
	} else {
		resp.MOFlag = sbdStatus.MOFlag
		resp.MTFlag = sbdStatus.MTFlag
		resp.MTQueued = sbdStatus.MTWaiting
	}

	jsonResponse(w, http.StatusOK, resp)
}

// GetIridiumSignal returns signal strength.
// @Summary Get Iridium signal strength
// @Description Returns signal quality (0-5) with human description
// @Tags Iridium
// @Produce json
// @Param port query string false "Serial port" default(/dev/ttyUSB1)
// @Success 200 {object} IridiumSignalResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/signal [get]
func (h *HALHandler) GetIridiumSignal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Auto-connect if needed
	if !h.iridium.IsConnected() {
		port := r.URL.Query().Get("port")
		if err := h.iridium.Connect(ctx, port); err != nil {
			jsonResponse(w, http.StatusOK, IridiumSignalResponse{
				Strength:    0,
				Description: "No signal (modem not connected)",
			})
			return
		}
	}

	strength, desc, err := h.iridium.GetSignal(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("signal query", err))
		return
	}

	jsonResponse(w, http.StatusOK, IridiumSignalResponse{
		Strength:    strength,
		Description: desc,
	})
}

// SendIridiumMessage sends an SBD message (text or binary).
// @Summary Send Iridium SBD message
// @Description Writes message to MO buffer and initiates SBDIX session. Blocking (10-60s).
// @Tags Iridium
// @Accept json
// @Produce json
// @Param port query string false "Serial port" default(/dev/ttyUSB1)
// @Param request body IridiumSendRequest true "Message to send"
// @Success 200 {object} IridiumSendResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/send [post]
func (h *HALHandler) SendIridiumMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Body limit
	r = limitBody(r, 1<<20)

	var req IridiumSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate: must provide text or data
	if req.Text == "" && req.Data == "" {
		errorResponse(w, http.StatusBadRequest, "text or data is required")
		return
	}

	// Default format
	if req.Format == "" {
		if req.Text != "" {
			req.Format = "text"
		} else {
			req.Format = "binary"
		}
	}

	// Validate inputs BEFORE auto-connect (users deserve proper 400 errors)
	var textMsg string
	var binaryData []byte

	switch req.Format {
	case "text":
		textMsg = req.Text
		if textMsg == "" {
			// Fallback: decode data as text
			decoded, decErr := base64.StdEncoding.DecodeString(req.Data)
			if decErr != nil {
				errorResponse(w, http.StatusBadRequest, "data is not valid base64")
				return
			}
			textMsg = string(decoded)
		}

		// Validate text message
		if err := validateIridiumMessage(textMsg); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(textMsg) > 120 {
			errorResponse(w, http.StatusBadRequest, "text message too long (max 120 chars for AT+SBDWT, use binary for larger)")
			return
		}

	case "binary":
		var decErr error
		binaryData, decErr = base64.StdEncoding.DecodeString(req.Data)
		if decErr != nil {
			errorResponse(w, http.StatusBadRequest, "data is not valid base64")
			return
		}
		if len(binaryData) == 0 {
			errorResponse(w, http.StatusBadRequest, "data is empty")
			return
		}
		if len(binaryData) > 340 {
			errorResponse(w, http.StatusBadRequest, "binary data too large (max 340 bytes for SBD)")
			return
		}

	default:
		errorResponse(w, http.StatusBadRequest, "format must be 'text' or 'binary'")
		return
	}

	// Auto-connect if needed
	if !h.iridium.IsConnected() {
		port := r.URL.Query().Get("port")
		if err := h.iridium.Connect(ctx, port); err != nil {
			errorResponse(w, http.StatusInternalServerError, "modem not connected and auto-connect failed")
			return
		}
	}

	var result SBDIXResult
	var err error

	switch req.Format {
	case "text":
		result, err = h.iridium.SendText(ctx, textMsg)

	case "binary":
		result, err = h.iridium.SendBinary(ctx, binaryData)

	default:
		errorResponse(w, http.StatusBadRequest, "format must be 'text' or 'binary'")
		return
	}

	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("send failed: %s", sanitizeExecError("SBD send", err)))
		return
	}

	status := "sent"
	if !result.MOSuccess() {
		status = "failed"
	}

	jsonResponse(w, http.StatusOK, IridiumSendResponse{
		Status:     status,
		MOStatus:   result.MOStatus,
		MOMSN:      result.MOMSN,
		MTReceived: result.MTStatus == 1,
		MTQueued:   result.MTQueued,
	})
}

// CheckIridiumMailbox performs a mailbox check (SBDIX without MO).
// @Summary Check Iridium mailbox
// @Description Performs SBDIX without sending to check for incoming MT messages. Blocking (10-60s).
// @Tags Iridium
// @Produce json
// @Param port query string false "Serial port" default(/dev/ttyUSB1)
// @Success 200 {object} IridiumMailboxResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/mailbox_check [post]
func (h *HALHandler) CheckIridiumMailbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Auto-connect if needed
	if !h.iridium.IsConnected() {
		port := r.URL.Query().Get("port")
		if err := h.iridium.Connect(ctx, port); err != nil {
			errorResponse(w, http.StatusInternalServerError, "modem not connected")
			return
		}
	}

	result, err := h.iridium.MailboxCheck(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("mailbox check", err))
		return
	}

	resp := IridiumMailboxResponse{
		MTReceived: result.MTStatus == 1,
		MTQueued:   result.MTQueued,
	}

	// If MT received, read it
	if result.MTStatus == 1 && result.MTLength > 0 {
		data, err := h.iridium.ReadBinaryMT(ctx)
		if err != nil {
			log.Printf("iridium: MT read failed after mailbox check: %v", err)
		} else {
			encoded := base64.StdEncoding.EncodeToString(data)
			resp.MTMessage = &encoded
		}
	}

	jsonResponse(w, http.StatusOK, resp)
}

// ReceiveIridiumMessage reads the MT buffer.
// @Summary Read Iridium MT buffer
// @Description Reads the current Mobile Terminated message buffer
// @Tags Iridium
// @Produce json
// @Param port query string false "Serial port" default(/dev/ttyUSB1)
// @Param format query string false "Read format" default(binary) Enums(binary,text)
// @Success 200 {object} IridiumReceiveResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/receive [get]
func (h *HALHandler) ReceiveIridiumMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Auto-connect if needed
	if !h.iridium.IsConnected() {
		port := r.URL.Query().Get("port")
		if err := h.iridium.Connect(ctx, port); err != nil {
			jsonResponse(w, http.StatusOK, IridiumReceiveResponse{
				Length: 0,
				Format: "binary",
			})
			return
		}
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "binary"
	}

	switch format {
	case "binary":
		data, err := h.iridium.ReadBinaryMT(ctx)
		if err != nil {
			log.Printf("iridium: binary MT read: %v", err)
			jsonResponse(w, http.StatusOK, IridiumReceiveResponse{
				Length: 0,
				Format: "binary",
			})
			return
		}

		jsonResponse(w, http.StatusOK, IridiumReceiveResponse{
			Data:   base64.StdEncoding.EncodeToString(data),
			Length: len(data),
			Format: "binary",
		})

	case "text":
		text, err := h.iridium.ReadTextMT(ctx)
		if err != nil {
			log.Printf("iridium: text MT read: %v", err)
			jsonResponse(w, http.StatusOK, IridiumReceiveResponse{
				Length: 0,
				Format: "text",
			})
			return
		}

		jsonResponse(w, http.StatusOK, IridiumReceiveResponse{
			Data:   text,
			Length: len(text),
			Format: "text",
		})

	default:
		errorResponse(w, http.StatusBadRequest, "format must be 'binary' or 'text'")
	}
}

// ClearIridiumBuffers clears MO/MT/both buffers.
// @Summary Clear Iridium buffers
// @Description Clears the MO, MT, or both SBD message buffers
// @Tags Iridium
// @Accept json
// @Produce json
// @Param request body IridiumClearRequest true "Buffer to clear"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/clear [post]
func (h *HALHandler) ClearIridiumBuffers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r = limitBody(r, 1<<20)

	var req IridiumClearRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Buffer == "" {
		errorResponse(w, http.StatusBadRequest, "buffer field is required (mo, mt, or both)")
		return
	}

	// Validate buffer value before checking connection
	switch strings.ToLower(req.Buffer) {
	case "mo", "mt", "both":
		// valid
	default:
		errorResponse(w, http.StatusBadRequest, "invalid buffer (must be mo, mt, or both)")
		return
	}

	if !h.iridium.IsConnected() {
		errorResponse(w, http.StatusInternalServerError, "modem not connected")
		return
	}

	if err := h.iridium.ClearBuffers(ctx, req.Buffer); err != nil {
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("clear buffer", err))
		return
	}

	successResponse(w, fmt.Sprintf("%s buffer cleared", req.Buffer))
}

// StreamIridiumEvents serves an SSE stream of Iridium events.
// @Summary Stream Iridium events (SSE)
// @Description Server-Sent Events stream for ring alerts and modem status changes
// @Tags Iridium
// @Produce text/event-stream
// @Success 200 {string} string "SSE stream"
// @Router /iridium/events [get]
func (h *HALHandler) StreamIridiumEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := h.iridium.SubscribeEvents()
	defer unsubscribe()

	// Send initial event
	initialEvent := IridiumEvent{
		Type:    "connected_to_stream",
		Message: "SSE stream established",
		Time:    time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(initialEvent)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", initialEvent.Type, string(data))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(data))
			flusher.Flush()

		case <-ctx.Done():
			return
		}
	}
}

// ConnectIridium explicitly connects to a modem.
// @Summary Connect to Iridium modem
// @Description Opens serial connection and initializes the modem
// @Tags Iridium
// @Produce json
// @Param port query string false "Serial port (auto-detect if empty)"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /iridium/connect [post]
func (h *HALHandler) ConnectIridium(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")

	if port != "" {
		if err := validateSerialPort(port); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if err := h.iridium.Connect(r.Context(), port); err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("connection failed: %s", sanitizeExecError("connect", err)))
		return
	}

	successResponse(w, fmt.Sprintf("connected to %s (IMEI: %s)", h.iridium.Port(), h.iridium.IMEI()))
}

// DisconnectIridium closes the modem connection.
// @Summary Disconnect Iridium modem
// @Description Closes the serial connection
// @Tags Iridium
// @Produce json
// @Success 200 {object} SuccessResponse
// @Router /iridium/disconnect [post]
func (h *HALHandler) DisconnectIridium(w http.ResponseWriter, r *http.Request) {
	h.iridium.Disconnect()
	successResponse(w, "disconnected")
}

// GetIridiumMessages is a backward-compatibility alias for ReceiveIridiumMessage.
func (h *HALHandler) GetIridiumMessages(w http.ResponseWriter, r *http.Request) {
	h.ReceiveIridiumMessage(w, r)
}
