package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// ============================================================================
// Meshtastic REST Handlers
// ============================================================================

// GetMeshtasticDevices scans for Meshtastic devices on serial ports and BLE.
// @Summary List Meshtastic devices
// @Description Scans USB serial ports (VID:PID matching) and Bluetooth Low Energy (Meshtastic service UUID) for Meshtastic-compatible LoRa radios. Independent of active connection.
// @Tags Meshtastic
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /meshtastic/devices [get]
func (h *HALHandler) GetMeshtasticDevices(w http.ResponseWriter, r *http.Request) {
	devices, bleWarning := h.meshtastic.ScanDevices(r.Context())
	response := map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	}
	if bleWarning != nil {
		response["ble_warning"] = bleWarning.Message
	}
	jsonResponse(w, http.StatusOK, response)
}

// GetMeshtasticStatus returns Meshtastic connection and radio status.
// @Summary Get Meshtastic status
// @Description Returns connection state, transport type, local node info, and node count
// @Tags Meshtastic
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /meshtastic/status [get]
func (h *HALHandler) GetMeshtasticStatus(w http.ResponseWriter, r *http.Request) {
	connected := h.meshtastic.IsConnected()

	status := map[string]interface{}{
		"connected": connected,
		"transport": "",
		"address":   "",
		"node_id":   "",
		"node_name": "",
		"num_nodes": 0,
	}

	if connected {
		d := h.meshtastic
		d.mu.RLock()
		if d.transport != nil {
			status["transport"] = d.transport.TransportType()
			status["address"] = d.transport.DeviceAddress()
		}
		status["num_nodes"] = len(d.nodes)
		d.mu.RUnlock()

		if myNode := d.GetMyNode(); myNode != nil {
			status["node_id"] = nodeIDStr(myNode.Num)
			status["node_name"] = myNode.LongName
			status["hw_model"] = myNode.HWModelName
		} else if myNum := d.GetMyNodeNum(); myNum != 0 {
			status["node_id"] = nodeIDStr(myNum)
		}
	}

	jsonResponse(w, http.StatusOK, status)
}

// GetMeshtasticNodes returns all known mesh nodes.
// @Summary Get mesh nodes
// @Description Returns list of nodes in the Meshtastic mesh network (from NodeDB)
// @Tags Meshtastic
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /meshtastic/nodes [get]
func (h *HALHandler) GetMeshtasticNodes(w http.ResponseWriter, r *http.Request) {
	nodes := h.meshtastic.GetNodes()

	// Format node IDs as !hex strings for display
	type nodeResponse struct {
		*MeshNode
		NodeIDStr string `json:"node_id_str"`
	}
	out := make([]nodeResponse, len(nodes))
	for i, n := range nodes {
		out[i] = nodeResponse{MeshNode: n, NodeIDStr: nodeIDStr(n.Num)}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count": len(out),
		"nodes": out,
	})
}

// GetMeshtasticPosition returns the local node's GPS position.
// @Summary Get Meshtastic position
// @Description Returns the local Meshtastic node's GPS position from the NodeDB
// @Tags Meshtastic
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /meshtastic/position [get]
func (h *HALHandler) GetMeshtasticPosition(w http.ResponseWriter, r *http.Request) {
	myNode := h.meshtastic.GetMyNode()

	if myNode == nil || (myNode.Latitude == 0 && myNode.Longitude == 0) {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"valid":     false,
			"latitude":  0,
			"longitude": 0,
			"altitude":  0,
			"sats":      0,
			"timestamp": "",
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"valid":     true,
		"latitude":  myNode.Latitude,
		"longitude": myNode.Longitude,
		"altitude":  myNode.Altitude,
		"sats":      myNode.Sats,
		"timestamp": myNode.LastHeardStr,
	})
}

// ConnectMeshtastic establishes a connection to a Meshtastic device.
// @Summary Connect to Meshtastic device
// @Description Connects to a Meshtastic device via USB serial or BLE and downloads the NodeDB. Transport is selected automatically or can be specified explicitly.
// @Tags Meshtastic
// @Accept json
// @Produce json
// @Param request body object false "Connection parameters: port (serial path), address (BLE MAC), transport (auto/serial/ble)"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /meshtastic/connect [post]
func (h *HALHandler) ConnectMeshtastic(w http.ResponseWriter, r *http.Request) {
	// Parse optional connection parameters
	var req struct {
		Port      string `json:"port"`      // Serial port path (e.g., "/dev/ttyACM0")
		Address   string `json:"address"`   // BLE MAC address (e.g., "AA:BB:CC:DD:EE:FF")
		Transport string `json:"transport"` // "auto" (default), "serial", or "ble"
	}

	// Body is optional — empty body means auto-detect
	if r.ContentLength > 0 {
		r = limitBody(r, 1<<10) // 1KB max
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Default transport to auto
	if req.Transport == "" {
		req.Transport = "auto"
	}

	// Build the connect target string based on transport selection
	var connectTarget string
	switch req.Transport {
	case "serial":
		if req.Port != "" {
			if err := validateSerialPort(req.Port); err != nil {
				errorResponse(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		connectTarget = req.Port // Empty = serial auto-detect

	case "ble":
		if req.Address != "" {
			if err := validateBLEAddress(req.Address); err != nil {
				errorResponse(w, http.StatusBadRequest, err.Error())
				return
			}
			connectTarget = "ble://" + req.Address
		} else {
			connectTarget = "ble://" // BLE auto-scan
		}

	case "auto":
		// Backwards-compatible: if port is specified, use it as-is
		if req.Port != "" {
			if err := validateSerialPort(req.Port); err != nil {
				errorResponse(w, http.StatusBadRequest, err.Error())
				return
			}
			connectTarget = req.Port
		} else if req.Address != "" {
			if err := validateBLEAddress(req.Address); err != nil {
				errorResponse(w, http.StatusBadRequest, err.Error())
				return
			}
			connectTarget = "ble://" + req.Address
		}
		// Empty connectTarget = full auto-detect (serial → BLE)

	default:
		errorResponse(w, http.StatusBadRequest, "transport must be 'auto', 'serial', or 'ble'")
		return
	}

	// Connect with a generous timeout for config download
	ctx, cancel := getConnectContext(r.Context(), 30*time.Second)
	defer cancel()

	if err := h.meshtastic.Connect(ctx, connectTarget); err != nil {
		log.Printf("meshtastic: connect failed: %v", err)
		// BLE safety gate → 409 Conflict (not 500)
		var safetyErr *BLESafetyError
		if errors.As(err, &safetyErr) {
			errorResponse(w, http.StatusConflict, safetyErr.Message)
			return
		}
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("connection failed: %v", err))
		return
	}

	d := h.meshtastic
	response := map[string]interface{}{
		"status":    "connected",
		"num_nodes": len(d.GetNodes()),
	}
	if d.GetMyNodeNum() != 0 {
		response["node_id"] = nodeIDStr(d.GetMyNodeNum())
	}
	if myNode := d.GetMyNode(); myNode != nil {
		response["node_name"] = myNode.LongName
	}
	// Report which transport was actually used
	d.mu.RLock()
	if d.transport != nil {
		response["transport"] = d.transport.TransportType()
		response["address"] = d.transport.DeviceAddress()
	}
	d.mu.RUnlock()

	jsonResponse(w, http.StatusOK, response)
}

// DisconnectMeshtastic closes the Meshtastic connection.
// @Summary Disconnect from Meshtastic device
// @Description Closes the connection to the Meshtastic device
// @Tags Meshtastic
// @Produce json
// @Success 200 {object} SuccessResponse
// @Router /meshtastic/disconnect [post]
func (h *HALHandler) DisconnectMeshtastic(w http.ResponseWriter, r *http.Request) {
	h.meshtastic.Disconnect()
	successResponse(w, "disconnected from Meshtastic device")
}

// SendMeshtasticMessage sends a text message via the Meshtastic mesh.
// @Summary Send Meshtastic message
// @Description Sends a text message via the Meshtastic mesh network. Auto-connects if not connected.
// @Tags Meshtastic
// @Accept json
// @Produce json
// @Param request body MeshtasticSendRequest true "Message parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /meshtastic/messages/send [post]
func (h *HALHandler) SendMeshtasticMessage(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<10) // 1KB max

	var req MeshtasticSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate input BEFORE checking connection (Phase 1 pattern: validate → connect → execute)
	if err := validateMeshtasticText(req.Text); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Parse destination
	var to uint32 = 0xFFFFFFFF // Default: broadcast
	if req.To != "" && req.To != "broadcast" {
		if err := validateMeshtasticNodeID(req.To); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		// Parse hex node ID (strip leading !)
		hexStr := req.To
		if len(hexStr) > 0 && hexStr[0] == '!' {
			hexStr = hexStr[1:]
		}
		parsed, err := strconv.ParseUint(hexStr, 16, 32)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid node ID format")
			return
		}
		to = uint32(parsed)
	}

	// Auto-connect if needed
	if !h.meshtastic.IsConnected() {
		ctx, cancel := getConnectContext(r.Context(), 30*time.Second)
		defer cancel()
		if err := h.meshtastic.Connect(ctx, ""); err != nil {
			var safetyErr *BLESafetyError
			if errors.As(err, &safetyErr) {
				errorResponse(w, http.StatusConflict, safetyErr.Message)
				return
			}
			errorResponse(w, http.StatusInternalServerError, "not connected and auto-connect failed")
			return
		}
	}

	if err := h.meshtastic.SendText(r.Context(), req.Text, to, uint32(req.Channel)); err != nil {
		log.Printf("meshtastic: send failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("send failed: %v", err))
		return
	}

	successResponse(w, "message sent via Meshtastic")
}

// SendMeshtasticRaw sends a raw payload with a specified portnum.
// @Summary Send raw Meshtastic packet
// @Description Sends a raw payload with arbitrary portnum to the mesh network
// @Tags Meshtastic
// @Accept json
// @Produce json
// @Param request body MeshtasticRawRequest true "Raw packet parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /meshtastic/messages/send_raw [post]
func (h *HALHandler) SendMeshtasticRaw(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<16) // 64KB max

	var req MeshtasticRawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate input before checking connection
	if req.Payload == "" {
		errorResponse(w, http.StatusBadRequest, "payload is required (base64-encoded)")
		return
	}

	payload, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid base64 payload")
		return
	}

	if len(payload) == 0 || len(payload) > 237 {
		errorResponse(w, http.StatusBadRequest, "payload must be 1-237 bytes")
		return
	}

	if req.PortNum <= 0 || req.PortNum > 65535 {
		errorResponse(w, http.StatusBadRequest, "portnum must be 1-65535")
		return
	}

	// Parse destination
	var to uint32 = 0xFFFFFFFF
	if req.To != "" && req.To != "broadcast" {
		hexStr := req.To
		if len(hexStr) > 0 && hexStr[0] == '!' {
			hexStr = hexStr[1:]
		}
		parsed, err := strconv.ParseUint(hexStr, 16, 32)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid destination node ID")
			return
		}
		to = uint32(parsed)
	}

	// Auto-connect if needed
	if !h.meshtastic.IsConnected() {
		ctx, cancel := getConnectContext(r.Context(), 30*time.Second)
		defer cancel()
		if err := h.meshtastic.Connect(ctx, ""); err != nil {
			var safetyErr *BLESafetyError
			if errors.As(err, &safetyErr) {
				errorResponse(w, http.StatusConflict, safetyErr.Message)
				return
			}
			errorResponse(w, http.StatusInternalServerError, "not connected and auto-connect failed")
			return
		}
	}

	if err := h.meshtastic.SendRaw(r.Context(), payload, req.PortNum, to, uint32(req.Channel), req.WantAck); err != nil {
		log.Printf("meshtastic: raw send failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("send failed: %v", err))
		return
	}

	successResponse(w, "raw packet sent via Meshtastic")
}

// GetMeshtasticMessages returns recent mesh messages from the ring buffer.
// @Summary Get recent mesh messages
// @Description Returns recent Meshtastic messages from the in-memory ring buffer
// @Tags Meshtastic
// @Produce json
// @Param limit query int false "Maximum messages to return" default(50)
// @Success 200 {object} map[string]interface{}
// @Router /meshtastic/messages [get]
func (h *HALHandler) GetMeshtasticMessages(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}

	messages := h.meshtastic.GetMessages(limit)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":    len(messages),
		"messages": messages,
	})
}

// StreamMeshtasticEvents streams real-time mesh events via Server-Sent Events.
// @Summary Stream Meshtastic events (SSE)
// @Description Real-time SSE stream of mesh messages, node updates, and connection events
// @Tags Meshtastic
// @Produce text/event-stream
// @Success 200 {string} string "SSE event stream"
// @Router /meshtastic/events [get]
func (h *HALHandler) StreamMeshtasticEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send initial connected event
	initialData, _ := json.Marshal(map[string]interface{}{
		"type":    "connected_to_stream",
		"message": "subscribed to Meshtastic event stream",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
	fmt.Fprintf(w, "event: connected_to_stream\ndata: %s\n\n", initialData)
	flusher.Flush()

	// Subscribe to events
	eventCh, unsubscribe := h.meshtastic.SubscribeEvents()
	defer unsubscribe()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-eventCh:
			if !ok {
				return // Channel closed
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			flusher.Flush()
		}
	}
}

// SetMeshtasticChannel configures a Meshtastic channel via AdminMessage.
// @Summary Set Meshtastic channel
// @Description Configures a Meshtastic channel by sending an AdminMessage to the connected radio. Supports primary (index 0) and secondary (1-7) channels with PSK, name, role, and uplink/downlink settings. Auto-connects if not already connected.
// @Tags Meshtastic
// @Accept json
// @Produce json
// @Param request body MeshtasticChannelRequest true "Channel configuration"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse "BLE safety gate conflict"
// @Failure 500 {object} ErrorResponse
// @Router /meshtastic/channel [post]
func (h *HALHandler) SetMeshtasticChannel(w http.ResponseWriter, r *http.Request) {
	r = limitBody(r, 1<<10) // 1KB max

	var req MeshtasticChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate index: 0-7
	if req.Index > 7 {
		errorResponse(w, http.StatusBadRequest, "channel index must be 0-7")
		return
	}

	// Validate role
	var roleVal uint32
	switch req.Role {
	case "PRIMARY":
		roleVal = 1
	case "SECONDARY":
		roleVal = 2
	case "DISABLED":
		roleVal = 0
	default:
		errorResponse(w, http.StatusBadRequest, "role must be PRIMARY, SECONDARY, or DISABLED")
		return
	}

	// Enforce role/index consistency
	if req.Index == 0 && roleVal == 2 {
		errorResponse(w, http.StatusBadRequest, "channel 0 cannot be SECONDARY (must be PRIMARY or DISABLED)")
		return
	}
	if req.Index > 0 && roleVal == 1 {
		errorResponse(w, http.StatusBadRequest, "only channel 0 can be PRIMARY")
		return
	}

	// Validate and decode PSK
	var pskBytes []byte
	if req.PSK != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.PSK)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "psk must be valid base64")
			return
		}
		pskLen := len(decoded)
		if pskLen != 0 && pskLen != 1 && pskLen != 16 && pskLen != 32 {
			errorResponse(w, http.StatusBadRequest, fmt.Sprintf("psk must be 0, 1, 16, or 32 bytes (got %d)", pskLen))
			return
		}
		pskBytes = decoded
	}

	// Validate channel name length (Meshtastic limit: 11 bytes for name)
	if len(req.Name) > 11 {
		errorResponse(w, http.StatusBadRequest, "channel name must be 11 characters or fewer")
		return
	}

	// Auto-connect if needed (same pattern as SendMeshtasticText)
	if !h.meshtastic.IsConnected() {
		ctx, cancel := getConnectContext(r.Context(), 30*time.Second)
		defer cancel()
		if err := h.meshtastic.Connect(ctx, ""); err != nil {
			var safetyErr *BLESafetyError
			if errors.As(err, &safetyErr) {
				errorResponse(w, http.StatusConflict, safetyErr.Message)
				return
			}
			errorResponse(w, http.StatusInternalServerError, "not connected and auto-connect failed")
			return
		}
	}

	ch := ChannelConfig{
		Index:           uint32(req.Index),
		Name:            req.Name,
		PSK:             pskBytes,
		Role:            roleVal,
		UplinkEnabled:   req.UplinkEnabled,
		DownlinkEnabled: req.DownlinkEnabled,
	}

	if err := h.meshtastic.SetChannel(r.Context(), ch); err != nil {
		log.Printf("meshtastic: set channel failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("set channel failed: %v", err))
		return
	}

	successResponse(w, fmt.Sprintf("channel %d configured as %s", req.Index, req.Role))
}

// GetMeshtasticConfig returns the current radio config.
// @Summary Get Meshtastic config
// @Description Returns the Meshtastic radio configuration received during the want_config_id handshake. Returns available:false if not connected or no config received yet.
// @Tags Meshtastic
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /meshtastic/config [get]
func (h *HALHandler) GetMeshtasticConfig(w http.ResponseWriter, r *http.Request) {
	configResult := h.meshtastic.GetConfig()
	if configResult == nil {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"available": false,
		})
		return
	}

	configResult["available"] = true
	jsonResponse(w, http.StatusOK, configResult)
}

// ============================================================================
// Request/Response Types
// ============================================================================

// MeshtasticSendRequest represents a text message send request.
// @Description Meshtastic text message parameters
type MeshtasticSendRequest struct {
	Text    string `json:"text" example:"Hello mesh!"`
	To      string `json:"to,omitempty" example:"!a1b2c3d4"`
	Channel int    `json:"channel,omitempty" example:"0"`
}

// MeshtasticChannelRequest represents a channel configuration request.
// @Description Meshtastic channel configuration parameters
type MeshtasticChannelRequest struct {
	Index           int    `json:"index" example:"0"`
	Name            string `json:"name" example:"CubeOS"`
	PSK             string `json:"psk,omitempty" example:"AQ=="`
	Role            string `json:"role" example:"PRIMARY"`
	UplinkEnabled   bool   `json:"uplink_enabled,omitempty" example:"false"`
	DownlinkEnabled bool   `json:"downlink_enabled,omitempty" example:"false"`
}

// MeshtasticRawRequest represents a raw packet send request.
// @Description Meshtastic raw packet parameters
type MeshtasticRawRequest struct {
	To      string `json:"to,omitempty" example:"!a1b2c3d4"`
	PortNum int    `json:"portnum" example:"256"`
	Payload string `json:"payload" example:"SGVsbG8="`
	Channel int    `json:"channel,omitempty" example:"0"`
	WantAck bool   `json:"want_ack,omitempty" example:"false"`
}

// ============================================================================
// Helpers
// ============================================================================

// getConnectContext creates a context with a timeout for connection operations.
// If the parent context already has a deadline shorter than maxTimeout, it's used as-is.
func getConnectContext(parent context.Context, maxTimeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok {
		if time.Until(deadline) < maxTimeout {
			return context.WithCancel(parent) // Parent deadline is shorter
		}
	}
	return context.WithTimeout(parent, maxTimeout)
}
