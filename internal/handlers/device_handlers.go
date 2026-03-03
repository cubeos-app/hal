package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ============================================================================
// Device REST Handlers
// ============================================================================

// ListDevices returns all USB devices from the supervisor registry.
// @Summary List all USB devices
// @Description Returns all USB devices detected by the device supervisor with class, role, and state
// @Tags Devices
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /devices [get]
func (h *HALHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.supervisor.Registry().ListAll()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// ListSerialDevices returns only serial devices (Meshtastic, Iridium, GPS).
// @Summary List serial devices
// @Description Returns serial USB devices with their identification role and connection state
// @Tags Devices
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /devices/serial [get]
func (h *HALHandler) ListSerialDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.supervisor.Registry().FindByClass(ClassSerial)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// TriggerDeviceScan forces an immediate USB + serial rescan.
// @Summary Trigger device rescan
// @Description Forces an immediate USB inventory scan and serial device reconciliation
// @Tags Devices
// @Produce json
// @Success 200 {object} SuccessResponse
// @Router /devices/scan [post]
func (h *HALHandler) TriggerDeviceScan(w http.ResponseWriter, r *http.Request) {
	h.supervisor.TriggerScan()
	successResponse(w, "device scan triggered")
}

// StreamDeviceEvents streams device state change events via SSE.
// @Summary Stream device events
// @Description Server-Sent Events stream for device add/remove/connect/disconnect events
// @Tags Devices
// @Produce text/event-stream
// @Success 200 {string} string "SSE stream"
// @Router /devices/events [get]
func (h *HALHandler) StreamDeviceEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Disable WriteTimeout for this long-lived SSE stream
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := h.supervisor.SubscribeEvents()
	defer unsubscribe()

	// Send initial event
	initial := DeviceEvent{
		Type: "connected_to_stream",
		Time: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(initial)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", initial.Type, string(data))
	flusher.Flush()

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
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
