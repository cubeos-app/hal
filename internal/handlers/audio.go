package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Audio Types
// ============================================================================

// AudioDevice represents an audio device.
// @Description Audio device information
type AudioDevice struct {
	Card        int    `json:"card" example:"0"`
	Device      int    `json:"device" example:"0"`
	Name        string `json:"name" example:"bcm2835 Headphones"`
	Type        string `json:"type" example:"playback"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty" example:"RUNNING"`
}

// AudioDevicesResponse represents audio devices list.
// @Description List of audio devices
type AudioDevicesResponse struct {
	Playback []AudioDevice `json:"playback"`
	Capture  []AudioDevice `json:"capture"`
}

// VolumeInfo represents volume information.
// @Description Audio volume information
type VolumeInfo struct {
	Control string `json:"control" example:"Master"`
	Volume  int    `json:"volume" example:"75"`
	Muted   bool   `json:"muted" example:"false"`
	Min     int    `json:"min" example:"0"`
	Max     int    `json:"max" example:"100"`
}

// VolumeRequest represents volume set request.
// @Description Volume set parameters
type VolumeRequest struct {
	Volume  int    `json:"volume" example:"75"`
	Control string `json:"control,omitempty" example:"Master"`
	Card    int    `json:"card,omitempty" example:"0"`
}

// MuteRequest represents mute request.
// @Description Mute parameters
type MuteRequest struct {
	Muted   bool   `json:"muted" example:"true"`
	Control string `json:"control,omitempty" example:"Master"`
	Card    int    `json:"card,omitempty" example:"0"`
}

// TestToneRequest represents test tone request.
// @Description Test tone parameters
type TestToneRequest struct {
	Card     int `json:"card,omitempty" example:"0"`
	Device   int `json:"device,omitempty" example:"0"`
	Duration int `json:"duration,omitempty" example:"2"`
}

// ============================================================================
// Audio Device Handlers
// ============================================================================

// GetAudioDevices lists audio devices.
// @Summary List audio devices
// @Description Returns list of ALSA audio devices
// @Tags Audio
// @Accept json
// @Produce json
// @Success 200 {object} AudioDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /audio/devices [get]
func (h *HALHandler) GetAudioDevices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	response := AudioDevicesResponse{
		Playback: h.getAudioDevices(ctx, "playback"),
		Capture:  h.getAudioDevices(ctx, "capture"),
	}

	jsonResponse(w, http.StatusOK, response)
}

// GetPlaybackDevices lists playback devices.
// @Summary List playback devices
// @Description Returns list of audio playback devices
// @Tags Audio
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /audio/playback [get]
func (h *HALHandler) GetPlaybackDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.getAudioDevices(r.Context(), "playback")
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// GetCaptureDevices lists capture devices.
// @Summary List capture devices
// @Description Returns list of audio capture (microphone) devices
// @Tags Audio
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /audio/capture [get]
func (h *HALHandler) GetCaptureDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.getAudioDevices(r.Context(), "capture")
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// ============================================================================
// Volume Control Handlers
// ============================================================================

// GetVolume returns current volume.
// @Summary Get volume
// @Description Returns current volume level
// @Tags Audio
// @Accept json
// @Produce json
// @Param control query string false "Mixer control" default(Master)
// @Param card query int false "Sound card" default(0)
// @Success 200 {object} VolumeInfo
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /audio/volume [get]
func (h *HALHandler) GetVolume(w http.ResponseWriter, r *http.Request) {
	control := r.URL.Query().Get("control")
	if control == "" {
		control = "Master"
	}

	// HF06-03: Validate mixer control
	if err := validateMixerControl(control); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	card := 0
	if cardParam := r.URL.Query().Get("card"); cardParam != "" {
		var err error
		card, err = strconv.Atoi(cardParam)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid card number")
			return
		}
	}

	// HF06-08: Validate audio card
	if err := validateAudioCard(card); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	info := h.getVolumeInfo(r.Context(), card, control)
	jsonResponse(w, http.StatusOK, info)
}

// SetVolume sets volume level.
// @Summary Set volume
// @Description Sets audio volume level
// @Tags Audio
// @Accept json
// @Produce json
// @Param request body VolumeRequest true "Volume parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /audio/volume [post]
func (h *HALHandler) SetVolume(w http.ResponseWriter, r *http.Request) {
	r.Body = limitBody(r, 1<<20).Body // HF06-04: limit body

	var req VolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Volume < 0 || req.Volume > 100 {
		errorResponse(w, http.StatusBadRequest, "volume must be 0-100")
		return
	}

	control := req.Control
	if control == "" {
		control = "Master"
	}

	// HF06-03: Validate mixer control
	if err := validateMixerControl(control); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF06-08: Validate audio card
	if err := validateAudioCard(req.Card); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF06-09: Use execWithTimeout
	cardArg := fmt.Sprintf("-c%d", req.Card)
	volumeArg := fmt.Sprintf("%d%%", req.Volume)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	_, err := execWithTimeout(ctx, "amixer", cardArg, "sset", control, volumeArg)
	if err != nil {
		// HF06-10: Sanitize error
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("set volume", err))
		return
	}

	successResponse(w, fmt.Sprintf("volume set to %d%%", req.Volume))
}

// SetMute sets mute state.
// @Summary Set mute
// @Description Sets audio mute state
// @Tags Audio
// @Accept json
// @Produce json
// @Param request body MuteRequest true "Mute parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /audio/mute [post]
func (h *HALHandler) SetMute(w http.ResponseWriter, r *http.Request) {
	r.Body = limitBody(r, 1<<20).Body // HF06-04: limit body

	var req MuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	control := req.Control
	if control == "" {
		control = "Master"
	}

	// HF06-03: Validate mixer control
	if err := validateMixerControl(control); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF06-08: Validate audio card
	if err := validateAudioCard(req.Card); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	muteArg := "unmute"
	if req.Muted {
		muteArg = "mute"
	}

	// HF06-09: Use execWithTimeout
	cardArg := fmt.Sprintf("-c%d", req.Card)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	_, err := execWithTimeout(ctx, "amixer", cardArg, "sset", control, muteArg)
	if err != nil {
		// HF06-10: Sanitize error
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("set mute", err))
		return
	}

	state := "unmuted"
	if req.Muted {
		state = "muted"
	}
	successResponse(w, fmt.Sprintf("audio %s", state))
}

// ============================================================================
// Audio Test Handlers
// ============================================================================

// PlayTestTone plays a test tone.
// @Summary Play test tone
// @Description Plays a test tone on the specified audio device
// @Tags Audio
// @Accept json
// @Produce json
// @Param request body TestToneRequest false "Test tone parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /audio/test [post]
func (h *HALHandler) PlayTestTone(w http.ResponseWriter, r *http.Request) {
	var req TestToneRequest

	// Accept JSON body (preferred) or fall back to query params
	if r.Body != nil && r.ContentLength > 0 {
		r.Body = limitBody(r, 1<<20).Body // HF06-04: limit body
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid request body")
			return
		}
	} else {
		// Fall back to query params for backward compatibility
		if cardParam := r.URL.Query().Get("card"); cardParam != "" {
			req.Card, _ = strconv.Atoi(cardParam)
		}
		if deviceParam := r.URL.Query().Get("device"); deviceParam != "" {
			req.Device, _ = strconv.Atoi(deviceParam)
		}
		if durParam := r.URL.Query().Get("duration"); durParam != "" {
			req.Duration, _ = strconv.Atoi(durParam)
		}
	}

	// Apply defaults
	if req.Duration == 0 {
		req.Duration = 2
	}

	// HF06-08: Validate audio card/device
	if err := validateAudioCard(req.Card); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Device < 0 || req.Device > 31 {
		errorResponse(w, http.StatusBadRequest, "audio device out of range (0-31)")
		return
	}

	// HF06-07: Validate test tone duration (1-10s)
	if err := validateDuration(req.Duration); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF06-09: Use execWithTimeout
	deviceArg := fmt.Sprintf("plughw:%d,%d", req.Card, req.Device)
	durationArg := strconv.Itoa(req.Duration)

	// Timeout = duration + 5s buffer for startup/cleanup
	timeout := time.Duration(req.Duration+5) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	_, err := execWithTimeout(ctx, "speaker-test", "-D", deviceArg, "-t", "sine", "-f", "440", "-l", "1", "-p", durationArg)
	if err != nil {
		// HF06-10: Sanitize error
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("test tone", err))
		return
	}

	successResponse(w, "test tone played")
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) getAudioDevices(ctx context.Context, deviceType string) []AudioDevice {
	var devices []AudioDevice

	// HF06-09: Use execWithTimeout for aplay/arecord
	var cmdName string
	if deviceType == "playback" {
		cmdName = "aplay"
	} else {
		cmdName = "arecord"
	}

	output, err := execWithTimeout(ctx, cmdName, "-l")
	if err != nil {
		return devices
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		// Parse lines like: "card 0: Headphones [bcm2835 Headphones], device 0: bcm2835 Headphones [bcm2835 Headphones]"
		if strings.HasPrefix(line, "card ") {
			device := AudioDevice{Type: deviceType}

			// Extract card number
			if idx := strings.Index(line, "card "); idx != -1 {
				cardStr := line[idx+5:]
				if colonIdx := strings.Index(cardStr, ":"); colonIdx != -1 {
					device.Card, _ = strconv.Atoi(cardStr[:colonIdx])
				}
			}

			// Extract device number
			if idx := strings.Index(line, "device "); idx != -1 {
				devStr := line[idx+7:]
				if colonIdx := strings.Index(devStr, ":"); colonIdx != -1 {
					device.Device, _ = strconv.Atoi(devStr[:colonIdx])
				}
			}

			// Extract name from brackets
			if idx := strings.Index(line, "["); idx != -1 {
				if endIdx := strings.Index(line[idx:], "]"); endIdx != -1 {
					device.Name = line[idx+1 : idx+endIdx]
				}
			}

			// Extract description after colon
			if idx := strings.Index(line, ": "); idx != -1 {
				parts := strings.Split(line[idx+2:], ",")
				if len(parts) > 0 {
					// Get just the name part
					namePart := strings.TrimSpace(parts[0])
					if bracketIdx := strings.Index(namePart, " ["); bracketIdx != -1 {
						device.Description = namePart[:bracketIdx]
					} else {
						device.Description = namePart
					}
				}
			}

			if device.Name != "" {
				devices = append(devices, device)
			}
		}
	}

	return devices
}

func (h *HALHandler) getVolumeInfo(ctx context.Context, card int, control string) VolumeInfo {
	info := VolumeInfo{
		Control: control,
		Min:     0,
		Max:     100,
	}

	// HF06-09: Use execWithTimeout
	cardArg := fmt.Sprintf("-c%d", card)
	output, err := execWithTimeout(ctx, "amixer", cardArg, "sget", control)
	if err != nil {
		return info
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		// Parse lines like: "  Mono: Playback 400 [61%] [on]"
		// or: "  Front Left: Playback 400 [61%] [on]"
		if strings.Contains(line, "Playback") || strings.Contains(line, "Capture") {
			// Extract percentage
			if idx := strings.Index(line, "["); idx != -1 {
				percentStr := line[idx+1:]
				if endIdx := strings.Index(percentStr, "%"); endIdx != -1 {
					info.Volume, _ = strconv.Atoi(percentStr[:endIdx])
				}
			}

			// Check mute status
			info.Muted = strings.Contains(line, "[off]")
			break
		}
	}

	return info
}
