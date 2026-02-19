package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Camera Types
// ============================================================================

// CameraDevice represents a camera device.
// @Description Camera device information
type CameraDevice struct {
	Index       int      `json:"index" example:"0"`
	Name        string   `json:"name" example:"Pi Camera Module 3"`
	Path        string   `json:"path" example:"/dev/video0"`
	Driver      string   `json:"driver,omitempty" example:"bcm2835-v4l2"`
	Type        string   `json:"type" example:"csi"`
	Resolutions []string `json:"resolutions,omitempty"`
	Formats     []string `json:"formats,omitempty"`
	Available   bool     `json:"available" example:"true"`
}

// CameraDevicesResponse represents camera list.
// @Description List of camera devices
type CameraDevicesResponse struct {
	Count   int            `json:"count" example:"2"`
	Cameras []CameraDevice `json:"cameras"`
}

// CaptureRequest represents image capture request.
// @Description Camera capture parameters
type CaptureRequest struct {
	Width    int    `json:"width,omitempty" example:"1920"`
	Height   int    `json:"height,omitempty" example:"1080"`
	Quality  int    `json:"quality,omitempty" example:"85"`
	Format   string `json:"format,omitempty" example:"jpeg"`
	Camera   int    `json:"camera,omitempty" example:"0"`
	Rotation int    `json:"rotation,omitempty" example:"0"`
	HFlip    bool   `json:"hflip,omitempty" example:"false"`
	VFlip    bool   `json:"vflip,omitempty" example:"false"`
}

// CaptureResponse represents capture result.
// @Description Camera capture result
type CaptureResponse struct {
	Success   bool   `json:"success" example:"true"`
	Path      string `json:"path,omitempty" example:"/tmp/capture.jpg"`
	Size      int64  `json:"size,omitempty" example:"245678"`
	Width     int    `json:"width,omitempty" example:"1920"`
	Height    int    `json:"height,omitempty" example:"1080"`
	Timestamp string `json:"timestamp" example:"2026-02-03T16:30:00Z"`
	Base64    string `json:"base64,omitempty"`
}

// StreamRequest represents stream start request.
// @Description Camera stream parameters
type StreamRequest struct {
	Camera int `json:"camera,omitempty" example:"0"`
	Width  int `json:"width,omitempty" example:"1280"`
	Height int `json:"height,omitempty" example:"720"`
	FPS    int `json:"fps,omitempty" example:"30"`
}

// StreamInfo represents stream information.
// @Description Camera stream information
type StreamInfo struct {
	Active bool   `json:"active" example:"true"`
	URL    string `json:"url,omitempty" example:"http://cubeos.cube:8080/stream"`
	Camera int    `json:"camera" example:"0"`
	Width  int    `json:"width" example:"1280"`
	Height int    `json:"height" example:"720"`
	FPS    int    `json:"fps" example:"30"`
	Format string `json:"format" example:"mjpeg"`
}

// ============================================================================
// Stream environment helpers
// ============================================================================

// getStreamHost returns the stream hostname from env or default.
func getStreamHost() string {
	if h := os.Getenv("CUBEOS_STREAM_HOST"); h != "" {
		return h
	}
	return "cubeos.cube"
}

// getStreamPort returns the stream port from env or default.
func getStreamPort() string {
	if p := os.Getenv("CUBEOS_STREAM_PORT"); p != "" {
		return p
	}
	return "8080"
}

// ============================================================================
// Camera Handlers
// ============================================================================

// GetCameras lists available cameras.
// @Summary List cameras
// @Description Returns list of available camera devices (Pi Camera + USB webcams)
// @Tags Camera
// @Accept json
// @Produce json
// @Success 200 {object} CameraDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /camera/devices [get]
func (h *HALHandler) GetCameras(w http.ResponseWriter, r *http.Request) {
	cameras := h.scanCameras(r.Context())
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(cameras),
		"cameras": cameras,
	})
}

// GetCameraInfo returns info about a specific camera.
// @Summary Get camera info
// @Description Returns detailed information about a specific camera
// @Tags Camera
// @Accept json
// @Produce json
// @Param index query int false "Camera index" default(0)
// @Success 200 {object} CameraDevice
// @Failure 400 {object} ErrorResponse "Invalid camera index"
// @Failure 404 {object} ErrorResponse "Camera not found"
// @Failure 500 {object} ErrorResponse
// @Router /camera/info [get]
func (h *HALHandler) GetCameraInfo(w http.ResponseWriter, r *http.Request) {
	index := 0
	if idx := r.URL.Query().Get("index"); idx != "" {
		var err error
		index, err = strconv.Atoi(idx)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid camera index")
			return
		}
	}
	if err := validateCameraIndex(index); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	cameras := h.scanCameras(r.Context())
	if index >= len(cameras) {
		errorResponse(w, http.StatusNotFound, "camera not found")
		return
	}

	jsonResponse(w, http.StatusOK, cameras[index])
}

// CaptureImage captures a still image.
// @Summary Capture image
// @Description Captures a still image from the camera
// @Tags Camera
// @Accept json
// @Produce json
// @Param request body CaptureRequest false "Capture parameters"
// @Success 200 {object} CaptureResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /camera/capture [post]
func (h *HALHandler) CaptureImage(w http.ResponseWriter, r *http.Request) {
	var req CaptureRequest
	if r.Body != nil {
		r.Body = limitBody(r, 1<<20).Body // HF06-04: limit body
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Set defaults
	if req.Width == 0 {
		req.Width = 1920
	}
	if req.Height == 0 {
		req.Height = 1080
	}
	if req.Quality == 0 {
		req.Quality = 85
	}
	if req.Format == "" {
		req.Format = "jpeg"
	}

	// HF06-05: Validate capture params
	if err := validateResolution(req.Width, req.Height); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateImageQuality(req.Quality); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Rotation != 0 {
		if err := validateRotation(req.Rotation); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := validateCameraIndex(req.Camera); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Generate output path
	timestamp := time.Now().Format("20060102-150405")
	outputPath := fmt.Sprintf("/tmp/capture_%s.jpg", timestamp)

	// Try libcamera-still first (Pi Camera)
	args := []string{
		"-o", outputPath,
		"--width", strconv.Itoa(req.Width),
		"--height", strconv.Itoa(req.Height),
		"-q", strconv.Itoa(req.Quality),
		"-n",      // No preview
		"-t", "1", // 1ms timeout (immediate capture)
	}

	if req.Rotation != 0 {
		args = append(args, "--rotation", strconv.Itoa(req.Rotation))
	}
	if req.HFlip {
		args = append(args, "--hflip")
	}
	if req.VFlip {
		args = append(args, "--vflip")
	}
	if req.Camera > 0 {
		args = append(args, "--camera", strconv.Itoa(req.Camera))
	}

	// HF06-09: Use execWithTimeout instead of raw exec.Command
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	_, err := execWithTimeout(ctx, "libcamera-still", args...)
	if err != nil {
		// Try fswebcam for USB cameras
		args = []string{
			"-r", fmt.Sprintf("%dx%d", req.Width, req.Height),
			"--jpeg", strconv.Itoa(req.Quality),
			"-d", fmt.Sprintf("/dev/video%d", req.Camera),
			"--no-banner",
			outputPath,
		}
		_, err2 := execWithTimeout(ctx, "fswebcam", args...)
		if err2 != nil {
			// HF06-10: Sanitize error messages
			errorResponse(w, http.StatusInternalServerError,
				sanitizeExecError("libcamera-still", err)+"; "+sanitizeExecError("fswebcam", err2))
			return
		}
	}

	// Get file info
	fileInfo, err := os.Stat(outputPath)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to read captured image")
		return
	}

	response := CaptureResponse{
		Success:   true,
		Path:      outputPath,
		Size:      fileInfo.Size(),
		Width:     req.Width,
		Height:    req.Height,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Include base64 if image is small enough
	if fileInfo.Size() < 2*1024*1024 { // < 2MB
		if data, err := os.ReadFile(outputPath); err == nil {
			response.Base64 = base64.StdEncoding.EncodeToString(data)
		}
	}

	jsonResponse(w, http.StatusOK, response)
}

// GetCapturedImage serves a captured image.
// @Summary Get captured image
// @Description Returns a previously captured image file
// @Tags Camera
// @Produce image/jpeg
// @Param path query string true "Image path"
// @Success 200 {file} binary "JPEG image"
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /camera/image [get]
func (h *HALHandler) GetCapturedImage(w http.ResponseWriter, r *http.Request) {
	imagePath := r.URL.Query().Get("path")
	if imagePath == "" {
		errorResponse(w, http.StatusBadRequest, "path required")
		return
	}

	// HF06-01 (P1): Use validateCapturePath for path traversal protection.
	// filepath.Clean + /tmp/ prefix check + capture_*.jpg pattern match.
	if err := validateCapturePath(imagePath); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid path")
		return
	}

	// Use the cleaned path for file access
	cleanPath := filepath.Clean(imagePath)

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "image not found")
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// GetStreamInfo returns stream information.
// @Summary Get stream info
// @Description Returns camera stream status and URL
// @Tags Camera
// @Accept json
// @Produce json
// @Success 200 {object} StreamInfo
// @Failure 500 {object} ErrorResponse
// @Router /camera/stream/status [get]
func (h *HALHandler) GetStreamInfo(w http.ResponseWriter, r *http.Request) {
	info := StreamInfo{
		Active: false,
	}

	// HF06-02: Check tracked stream process first
	h.streamMu.Lock()
	if h.streamCmd != nil && h.streamCmd.Process != nil {
		info.Active = true
		info.URL = fmt.Sprintf("http://%s:%s/?action=stream", getStreamHost(), getStreamPort())
		info.Format = "mjpeg"
	}
	h.streamMu.Unlock()

	// Also check for externally started streaming processes
	if !info.Active {
		ctx := r.Context()
		if _, err := execWithTimeout(ctx, "pgrep", "-f", "mjpg_streamer"); err == nil {
			info.Active = true
			info.URL = fmt.Sprintf("http://%s:%s/?action=stream", getStreamHost(), getStreamPort())
			info.Format = "mjpeg"
			info.FPS = 30
		} else if _, err := execWithTimeout(ctx, "pgrep", "-f", "libcamera-vid"); err == nil {
			info.Active = true
		}
	}

	jsonResponse(w, http.StatusOK, info)
}

// StartStream starts camera streaming.
// @Summary Start stream
// @Description Starts MJPEG camera streaming
// @Tags Camera
// @Accept json
// @Produce json
// @Param request body StreamRequest false "Stream parameters"
// @Success 200 {object} StreamInfo
// @Failure 400 {object} ErrorResponse "Invalid parameters or stream already active"
// @Failure 500 {object} ErrorResponse
// @Router /camera/stream/start [post]
func (h *HALHandler) StartStream(w http.ResponseWriter, r *http.Request) {
	// Parse from JSON body or query params
	var req StreamRequest
	if r.Body != nil && r.ContentLength > 0 {
		r.Body = limitBody(r, 1<<20).Body
		json.NewDecoder(r.Body).Decode(&req)
	}
	// Fall back to query params for backward compatibility
	if req.Camera == 0 {
		if idx := r.URL.Query().Get("camera"); idx != "" {
			req.Camera, _ = strconv.Atoi(idx)
		}
	}
	if req.Width == 0 {
		if ws := r.URL.Query().Get("width"); ws != "" {
			req.Width, _ = strconv.Atoi(ws)
		}
	}
	if req.Height == 0 {
		if hs := r.URL.Query().Get("height"); hs != "" {
			req.Height, _ = strconv.Atoi(hs)
		}
	}
	if req.FPS == 0 {
		if f := r.URL.Query().Get("fps"); f != "" {
			req.FPS, _ = strconv.Atoi(f)
		}
	}

	// Apply defaults
	if req.Width == 0 {
		req.Width = 1280
	}
	if req.Height == 0 {
		req.Height = 720
	}
	if req.FPS == 0 {
		req.FPS = 30
	}

	// HF06-06: Validate stream params
	if err := validateCameraIndex(req.Camera); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateResolution(req.Width, req.Height); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.FPS < 1 || req.FPS > 120 {
		errorResponse(w, http.StatusBadRequest, "fps out of range (1-120)")
		return
	}

	// HF06-02: Prevent double-start
	h.streamMu.Lock()
	defer h.streamMu.Unlock()

	if h.streamCmd != nil && h.streamCmd.Process != nil {
		errorResponse(w, http.StatusBadRequest, "stream already active, stop first")
		return
	}

	streamPort := getStreamPort()

	// Create a cancellable context for the stream process
	ctx, cancel := context.WithCancel(context.Background())

	// Try mjpg-streamer
	cmd := exec.CommandContext(ctx,
		"mjpg_streamer",
		"-i", fmt.Sprintf("input_uvc.so -d /dev/video%d -r %dx%d -f %d", req.Camera, req.Width, req.Height, req.FPS),
		"-o", fmt.Sprintf("output_http.so -p %s -w /usr/share/mjpg-streamer/www", streamPort),
	)

	if err := cmd.Start(); err != nil {
		// Try libcamera approach
		cmd = exec.CommandContext(ctx,
			"libcamera-vid",
			"-t", "0",
			"--width", strconv.Itoa(req.Width),
			"--height", strconv.Itoa(req.Height),
			"--framerate", strconv.Itoa(req.FPS),
			"--codec", "mjpeg",
			"-o", fmt.Sprintf("tcp://0.0.0.0:%s", streamPort),
		)
		if err := cmd.Start(); err != nil {
			cancel()
			errorResponse(w, http.StatusInternalServerError, sanitizeExecError("start stream", err))
			return
		}
	}

	// Store process reference for lifecycle management
	h.streamCmd = cmd
	h.streamCancel = cancel

	// Goroutine to clean up when process exits unexpectedly
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("stream process exited: %v", err)
		}
		h.streamMu.Lock()
		if h.streamCmd == cmd {
			h.streamCmd = nil
			h.streamCancel = nil
		}
		h.streamMu.Unlock()
	}()

	info := StreamInfo{
		Active: true,
		URL:    fmt.Sprintf("http://%s:%s/?action=stream", getStreamHost(), streamPort),
		Camera: req.Camera,
		Width:  req.Width,
		Height: req.Height,
		FPS:    req.FPS,
		Format: "mjpeg",
	}

	jsonResponse(w, http.StatusOK, info)
}

// StopStream stops camera streaming.
// @Summary Stop stream
// @Description Stops camera streaming
// @Tags Camera
// @Accept json
// @Produce json
// @Success 200 {object} SuccessResponse
// @Failure 500 {object} ErrorResponse
// @Router /camera/stream/stop [post]
func (h *HALHandler) StopStream(w http.ResponseWriter, r *http.Request) {
	h.stopStreamProcess()

	// Also kill any externally started streaming processes
	ctx := r.Context()
	execWithTimeout(ctx, "pkill", "-f", "mjpg_streamer")
	execWithTimeout(ctx, "pkill", "-f", "libcamera-vid")

	successResponse(w, "stream stopped")
}

// stopStreamProcess kills the tracked stream process, if any.
func (h *HALHandler) stopStreamProcess() {
	h.streamMu.Lock()
	defer h.streamMu.Unlock()

	if h.streamCancel != nil {
		h.streamCancel()
	}
	if h.streamCmd != nil && h.streamCmd.Process != nil {
		h.streamCmd.Process.Kill()
	}
	h.streamCmd = nil
	h.streamCancel = nil
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) scanCameras(ctx context.Context) []CameraDevice {
	var cameras []CameraDevice

	// Check for Pi Camera using libcamera
	output, err := execWithTimeout(ctx, "libcamera-hello", "--list-cameras")
	if err == nil {
		if strings.Contains(output, "Available cameras") {
			lines := strings.Split(output, "\n")
			for i, line := range lines {
				if strings.Contains(line, ": ") && strings.Contains(line, "imx") {
					camera := CameraDevice{
						Index:     len(cameras),
						Type:      "csi",
						Driver:    "libcamera",
						Available: true,
					}

					// Extract name
					parts := strings.SplitN(line, ":", 2)
					if len(parts) == 2 {
						camera.Name = strings.TrimSpace(parts[1])
					}

					// Get resolutions from next lines
					for j := i + 1; j < len(lines) && j < i+5; j++ {
						if strings.Contains(lines[j], "x") && strings.Contains(lines[j], "/") {
							res := strings.TrimSpace(lines[j])
							camera.Resolutions = append(camera.Resolutions, res)
						}
					}

					cameras = append(cameras, camera)
				}
			}
		}
	}

	// Check for V4L2 devices (USB webcams)
	entries, _ := filepath.Glob("/dev/video*")
	for _, entry := range entries {
		// Skip odd-numbered devices (usually metadata)
		devNum := strings.TrimPrefix(entry, "/dev/video")
		num, _ := strconv.Atoi(devNum)
		if num%2 != 0 {
			continue
		}

		// Get device info using v4l2-ctl
		devOutput, err := execWithTimeout(ctx, "v4l2-ctl", "--device", entry, "--info")
		if err == nil {
			camera := CameraDevice{
				Index:     len(cameras),
				Path:      entry,
				Type:      "usb",
				Available: true,
			}

			lines := strings.Split(devOutput, "\n")
			for _, line := range lines {
				if strings.Contains(line, "Card type") {
					parts := strings.SplitN(line, ":", 2)
					if len(parts) == 2 {
						camera.Name = strings.TrimSpace(parts[1])
					}
				}
				if strings.Contains(line, "Driver name") {
					parts := strings.SplitN(line, ":", 2)
					if len(parts) == 2 {
						camera.Driver = strings.TrimSpace(parts[1])
					}
				}
			}

			// Get supported formats
			fmtOutput, err := execWithTimeout(ctx, "v4l2-ctl", "--device", entry, "--list-formats-ext")
			if err == nil {
				fmtLines := strings.Split(fmtOutput, "\n")
				for _, line := range fmtLines {
					if strings.Contains(line, "Size:") {
						parts := strings.Split(line, "Size:")
						if len(parts) == 2 {
							res := strings.TrimSpace(parts[1])
							camera.Resolutions = append(camera.Resolutions, res)
						}
					}
				}
			}

			if camera.Name != "" {
				cameras = append(cameras, camera)
			}
		}
	}

	return cameras
}
