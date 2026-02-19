package handlers

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// GPS Types
// ============================================================================

// GPSDevice represents a GPS device.
// @Description GPS device information
type GPSDevice struct {
	Port     string `json:"port" example:"/dev/ttyUSB0"`
	Name     string `json:"name" example:"u-blox GPS"`
	Vendor   string `json:"vendor,omitempty" example:"u-blox AG"`
	Product  string `json:"product,omitempty" example:"u-blox 7 - GPS/GNSS Receiver"`
	BaudRate int    `json:"baud_rate" example:"9600"`
	Active   bool   `json:"active" example:"true"`
}

// GPSDevicesResponse represents GPS devices list.
// @Description List of GPS devices
type GPSDevicesResponse struct {
	Count   int         `json:"count" example:"1"`
	Devices []GPSDevice `json:"devices"`
}

// GPSStatus represents GPS status.
// @Description GPS status information
type GPSStatus struct {
	Available  bool    `json:"available" example:"true"`
	HasFix     bool    `json:"has_fix" example:"true"`
	FixQuality string  `json:"fix_quality" example:"3D Fix"`
	Satellites int     `json:"satellites" example:"8"`
	HDOP       float64 `json:"hdop,omitempty" example:"1.2"`
	LastUpdate string  `json:"last_update,omitempty"`
}

// GPSPosition represents GPS position.
// @Description GPS position data from NMEA
type GPSPosition struct {
	Latitude   float64 `json:"latitude" example:"52.3676"`
	Longitude  float64 `json:"longitude" example:"4.9041"`
	Altitude   float64 `json:"altitude,omitempty" example:"10.5"`
	Speed      float64 `json:"speed,omitempty" example:"0.0"`
	Course     float64 `json:"course,omitempty" example:"0.0"`
	Satellites int     `json:"satellites" example:"8"`
	FixQuality int     `json:"fix_quality" example:"1"`
	HDOP       float64 `json:"hdop,omitempty" example:"1.2"`
	Timestamp  string  `json:"timestamp" example:"2026-02-03T16:30:00Z"`
	Valid      bool    `json:"valid" example:"true"`
}

// ============================================================================
// GPS Handlers
// ============================================================================

// GetGPSDevices lists GPS devices.
// @Summary List GPS devices
// @Description Returns list of connected GPS/GNSS devices
// @Tags GPS
// @Accept json
// @Produce json
// @Success 200 {object} GPSDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /gps/devices [get]
func (h *HALHandler) GetGPSDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.scanGPSDevices()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// GetGPSStatus returns GPS status.
// @Summary Get GPS status
// @Description Returns GPS fix status and satellite information
// @Tags GPS
// @Accept json
// @Produce json
// @Param port query string false "GPS port" default(/dev/ttyUSB0)
// @Success 200 {object} GPSStatus
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gps/status [get]
func (h *HALHandler) GetGPSStatus(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")
	if port == "" {
		port = "/dev/ttyUSB0"
	}

	// HF05-01: Validate serial port path
	if err := validateSerialPort(port); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	status := h.readGPSStatus(r.Context(), port)
	jsonResponse(w, http.StatusOK, status)
}

// GetGPSPosition returns current GPS position.
// @Summary Get GPS position
// @Description Returns current GPS position from NMEA data
// @Tags GPS
// @Accept json
// @Produce json
// @Param port query string false "GPS port" default(/dev/ttyUSB0)
// @Param timeout query int false "Read timeout in seconds (1-30)" default(5)
// @Success 200 {object} GPSPosition
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gps/position [get]
func (h *HALHandler) GetGPSPosition(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")
	if port == "" {
		port = "/dev/ttyUSB0"
	}

	// HF05-01: Validate serial port path
	if err := validateSerialPort(port); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	timeoutParam := r.URL.Query().Get("timeout")
	timeout := 5
	if timeoutParam != "" {
		if t, err := strconv.Atoi(timeoutParam); err == nil && t > 0 {
			timeout = t
		}
	}

	// HF05-10: Bound GPS timeout to max 30 seconds
	if timeout > 30 {
		timeout = 30
	}

	position := h.readGPSPosition(r.Context(), port, timeout)
	jsonResponse(w, http.StatusOK, position)
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) scanGPSDevices() []GPSDevice {
	var devices []GPSDevice

	// Check common GPS serial ports
	ports := []string{
		"/dev/ttyUSB0", "/dev/ttyUSB1", "/dev/ttyUSB2",
		"/dev/ttyACM0", "/dev/ttyACM1",
		"/dev/serial0", "/dev/ttyAMA0",
	}

	for _, port := range ports {
		if _, err := os.Stat(port); err == nil {
			device := GPSDevice{
				Port:     port,
				BaudRate: 9600,
				Active:   true,
			}

			// Try to get USB device info — devName comes from hardcoded ports list (safe)
			if strings.Contains(port, "USB") || strings.Contains(port, "ACM") {
				devName := strings.TrimPrefix(port, "/dev/")
				sysPath := "/sys/class/tty/" + devName + "/device"

				if vendorData, err := os.ReadFile(sysPath + "/../manufacturer"); err == nil {
					device.Vendor = strings.TrimSpace(string(vendorData))
				}
				if productData, err := os.ReadFile(sysPath + "/../product"); err == nil {
					device.Product = strings.TrimSpace(string(productData))
					device.Name = device.Product
				}
			}

			if device.Name == "" {
				device.Name = "GPS Device"
			}

			devices = append(devices, device)
		}
	}

	return devices
}

func (h *HALHandler) readGPSStatus(ctx context.Context, port string) GPSStatus {
	status := GPSStatus{
		Available: false,
	}

	// Check if port exists
	if _, err := os.Stat(port); err != nil {
		return status
	}

	status.Available = true

	// Try to read NMEA sentences
	position := h.readGPSPosition(ctx, port, 3)
	if position.Valid {
		status.HasFix = true
		status.Satellites = position.Satellites
		status.HDOP = position.HDOP
		status.LastUpdate = position.Timestamp

		// Determine fix quality
		switch position.FixQuality {
		case 1:
			status.FixQuality = "GPS Fix"
		case 2:
			status.FixQuality = "DGPS Fix"
		default:
			if position.Satellites >= 4 {
				status.FixQuality = "3D Fix"
			} else {
				status.FixQuality = "2D Fix"
			}
		}
	}

	return status
}

// HF05-06: Fixed goroutine leak — scanner now runs synchronously in the calling goroutine.
// A helper goroutine closes the file on context expiry to unblock scanner.Scan().
// Old code: unbuffered done channel + fire-and-forget goroutine → goroutine stuck on Scan()
// after timeout, then blocked forever on unbuffered channel send.
func (h *HALHandler) readGPSPosition(ctx context.Context, port string, timeout int) GPSPosition {
	position := GPSPosition{
		Valid:     false,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// HF05-11: Configure serial port with bounded timeout
	sttyCtx, sttyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sttyCancel()
	if _, err := execWithTimeout(sttyCtx, "stty", "-F", port, "9600", "raw", "-echo"); err != nil {
		log.Printf("gps: stty configure failed for %s: %v", port, err)
	}

	// Open port
	f, err := os.Open(port)
	if err != nil {
		return position
	}
	defer f.Close()

	// Create a timeout context for the read operation
	readCtx, readCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer readCancel()

	// Close file on context expiry to unblock blocking scanner.Scan()
	closeDone := make(chan struct{})
	go func() {
		select {
		case <-readCtx.Done():
			f.Close() // unblocks scanner.Scan()
		case <-closeDone:
		}
	}()
	defer close(closeDone)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if readCtx.Err() != nil {
			break
		}

		line := scanner.Text()

		// Parse GGA sentence (position + fix quality)
		if strings.HasPrefix(line, "$GPGGA") || strings.HasPrefix(line, "$GNGGA") {
			parts := strings.Split(line, ",")
			if len(parts) >= 10 {
				// Fix quality
				if fq, err := strconv.Atoi(parts[6]); err == nil {
					position.FixQuality = fq
					position.Valid = fq > 0
				}

				// Satellites
				if sats, err := strconv.Atoi(parts[7]); err == nil {
					position.Satellites = sats
				}

				// HDOP
				if hdop, err := strconv.ParseFloat(parts[8], 64); err == nil {
					position.HDOP = hdop
				}

				// Latitude
				if parts[2] != "" && parts[3] != "" {
					if lat := parseNMEACoord(parts[2], parts[3]); lat != 0 {
						position.Latitude = lat
					}
				}

				// Longitude
				if parts[4] != "" && parts[5] != "" {
					if lon := parseNMEACoord(parts[4], parts[5]); lon != 0 {
						position.Longitude = lon
					}
				}

				// Altitude
				if parts[9] != "" {
					if alt, err := strconv.ParseFloat(parts[9], 64); err == nil {
						position.Altitude = alt
					}
				}
			}
		}

		// Parse RMC sentence (speed + course)
		if strings.HasPrefix(line, "$GPRMC") || strings.HasPrefix(line, "$GNRMC") {
			parts := strings.Split(line, ",")
			if len(parts) >= 9 {
				// Speed (knots to km/h)
				if parts[7] != "" {
					if speed, err := strconv.ParseFloat(parts[7], 64); err == nil {
						position.Speed = speed * 1.852 // knots to km/h
					}
				}

				// Course
				if parts[8] != "" {
					if course, err := strconv.ParseFloat(parts[8], 64); err == nil {
						position.Course = course
					}
				}
			}
		}

		// If we have a valid position, we're done
		if position.Valid && position.Latitude != 0 {
			break
		}
	}

	return position
}

// parseNMEACoord converts NMEA coordinate format to decimal degrees
func parseNMEACoord(coord, dir string) float64 {
	if coord == "" {
		return 0
	}

	// NMEA format: DDDMM.MMMM or DDMM.MMMM
	var degrees, minutes float64

	if len(coord) > 4 {
		// Longitude (DDDMM.MMMM) or Latitude (DDMM.MMMM)
		dotIdx := strings.Index(coord, ".")
		if dotIdx == -1 {
			return 0
		}

		degLen := dotIdx - 2
		if degLen < 1 || degLen > 3 {
			return 0
		}

		degrees, _ = strconv.ParseFloat(coord[:degLen], 64)
		minutes, _ = strconv.ParseFloat(coord[degLen:], 64)
	}

	result := degrees + minutes/60.0

	// Apply direction
	if dir == "S" || dir == "W" {
		result = -result
	}

	return result
}
