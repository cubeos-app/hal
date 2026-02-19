package handlers

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// I2C Types
// ============================================================================

// I2CDevice represents an I2C device.
// @Description I2C device on bus
type I2CDevice struct {
	Address    string `json:"address" example:"0x36"`
	AddressDec int    `json:"address_dec" example:"54"`
	Type       string `json:"type,omitempty" example:"MAX17040"`
	Name       string `json:"name,omitempty" example:"Fuel Gauge"`
}

// I2CBus represents an I2C bus.
// @Description I2C bus information
type I2CBus struct {
	Number int    `json:"number" example:"1"`
	Path   string `json:"path" example:"/dev/i2c-1"`
	Name   string `json:"name,omitempty" example:"bcm2835 (i2c@7e804000)"`
}

// I2CBusesResponse represents I2C buses list.
// @Description List of I2C buses
type I2CBusesResponse struct {
	Count int      `json:"count" example:"2"`
	Buses []I2CBus `json:"buses"`
}

// I2CScanResponse represents I2C scan results.
// @Description I2C bus scan results
type I2CScanResponse struct {
	Bus     int         `json:"bus" example:"1"`
	Count   int         `json:"count" example:"3"`
	Devices []I2CDevice `json:"devices"`
}

// I2CReadRequest represents I2C read request.
// @Description I2C read parameters
type I2CReadRequest struct {
	Bus      int    `json:"bus" example:"1"`
	Address  string `json:"address" example:"0x36"`
	Register string `json:"register" example:"0x02"`
	Length   int    `json:"length,omitempty" example:"2"`
}

// I2CWriteRequest represents I2C write request.
// @Description I2C write parameters
type I2CWriteRequest struct {
	Bus      int    `json:"bus" example:"1"`
	Address  string `json:"address" example:"0x36"`
	Register string `json:"register" example:"0x06"`
	Value    string `json:"value" example:"0x40"`
}

// ============================================================================
// I2C Bus Handlers
// ============================================================================

// ListI2CBuses lists available I2C buses.
// @Summary List I2C buses
// @Description Returns list of available I2C buses
// @Tags I2C
// @Accept json
// @Produce json
// @Success 200 {object} I2CBusesResponse
// @Failure 500 {object} ErrorResponse
// @Router /i2c/buses [get]
func (h *HALHandler) ListI2CBuses(w http.ResponseWriter, r *http.Request) {
	var buses []I2CBus

	entries, err := filepath.Glob("/dev/i2c-*")
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to list I2C buses: "+err.Error())
		return
	}

	for _, entry := range entries {
		name := filepath.Base(entry)
		num, err := strconv.Atoi(strings.TrimPrefix(name, "i2c-"))
		if err != nil {
			continue
		}

		bus := I2CBus{
			Number: num,
			Path:   entry,
		}

		// Try to get bus name from sysfs
		sysPath := fmt.Sprintf("/sys/class/i2c-adapter/i2c-%d/name", num)
		if data, err := os.ReadFile(sysPath); err == nil {
			bus.Name = strings.TrimSpace(string(data))
		}

		buses = append(buses, bus)
	}

	jsonResponse(w, http.StatusOK, I2CBusesResponse{
		Count: len(buses),
		Buses: buses,
	})
}

// ScanI2CBus scans an I2C bus for devices.
// @Summary Scan I2C bus
// @Description Scans an I2C bus for connected devices
// @Tags I2C
// @Accept json
// @Produce json
// @Param bus query int false "I2C bus number" default(1)
// @Success 200 {object} I2CScanResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /i2c/scan [get]
func (h *HALHandler) ScanI2CBus(w http.ResponseWriter, r *http.Request) {
	busParam := r.URL.Query().Get("bus")
	bus := 1
	if busParam != "" {
		n, err := strconv.Atoi(busParam)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid bus number")
			return
		}
		bus = n
	}

	// HF04-04: Validate I2C bus range
	if err := validateI2CBus(bus); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	output, err := execWithTimeout(r.Context(), "i2cdetect", "-y", strconv.Itoa(bus))
	if err != nil {
		log.Printf("i2cdetect bus %d failed: %v", bus, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("I2C scan", err))
		return
	}

	devices := h.parseI2CDetectOutput(output)

	// Try to identify known devices
	for i := range devices {
		devices[i].Type, devices[i].Name = identifyI2CDevice(devices[i].AddressDec)
	}

	jsonResponse(w, http.StatusOK, I2CScanResponse{
		Bus:     bus,
		Count:   len(devices),
		Devices: devices,
	})
}

// GetI2CDevice gets info about a specific I2C device.
// @Summary Get I2C device
// @Description Returns information about a specific I2C device
// @Tags I2C
// @Accept json
// @Produce json
// @Param bus path int true "I2C bus number" example(1)
// @Param address path string true "I2C address" example(0x36)
// @Success 200 {object} I2CDevice
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /i2c/bus/{bus}/device/{address} [get]
func (h *HALHandler) GetI2CDevice(w http.ResponseWriter, r *http.Request) {
	busStr := chi.URLParam(r, "bus")
	bus, err := strconv.Atoi(busStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid bus number")
		return
	}

	// HF04-04: Validate I2C bus range
	if err := validateI2CBus(bus); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	address := chi.URLParam(r, "address")
	if address == "" {
		errorResponse(w, http.StatusBadRequest, "address required")
		return
	}

	// HF04-05: Fix silent address 0 default — return error on parse failure
	var addr int
	if strings.HasPrefix(address, "0x") || strings.HasPrefix(address, "0X") {
		n, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(address, "0x"), "0X"), 16, 8)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid I2C address format")
			return
		}
		addr = int(n)
	} else {
		n, err := strconv.Atoi(address)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid I2C address format")
			return
		}
		addr = n
	}

	// Validate address range (0x03-0x77)
	if addr < 0x03 || addr > 0x77 {
		errorResponse(w, http.StatusBadRequest, "I2C address out of range (0x03-0x77)")
		return
	}

	// Check if device responds
	_, err = execWithTimeout(r.Context(), "i2cget", "-y", strconv.Itoa(bus), fmt.Sprintf("0x%02x", addr))
	if err != nil {
		errorResponse(w, http.StatusNotFound, "device not found at address "+address)
		return
	}

	device := I2CDevice{
		Address:    fmt.Sprintf("0x%02x", addr),
		AddressDec: addr,
	}
	device.Type, device.Name = identifyI2CDevice(addr)

	jsonResponse(w, http.StatusOK, device)
}

// ReadI2CRegister reads an I2C register.
// @Summary Read I2C register
// @Description Reads a register from an I2C device
// @Tags I2C
// @Accept json
// @Produce json
// @Param bus query int true "I2C bus number" example(1)
// @Param address query string true "I2C address" example(0x36)
// @Param register query string true "Register address" example(0x02)
// @Param mode query string false "Read mode" Enums(b, w, i) default(b)
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /i2c/read [get]
func (h *HALHandler) ReadI2CRegister(w http.ResponseWriter, r *http.Request) {
	busStr := r.URL.Query().Get("bus")
	bus, err := strconv.Atoi(busStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid bus number")
		return
	}

	// HF04-03 + HF04-04: Validate I2C bus range
	if err := validateI2CBus(bus); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-03: Validate I2C address
	address := r.URL.Query().Get("address")
	if err := validateI2CAddress(address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-03: Validate I2C register
	register := r.URL.Query().Get("register")
	if err := validateI2CRegister(register); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-03: Validate I2C mode
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "b" // byte mode by default
	}
	if err := validateI2CMode(mode); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	output, err := execWithTimeout(r.Context(), "i2cget", "-y", strconv.Itoa(bus), address, register, mode)
	if err != nil {
		log.Printf("i2cget bus=%d addr=%s reg=%s failed: %v", bus, address, register, err)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("I2C read", err))
		return
	}

	value := strings.TrimSpace(output)

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"bus":      bus,
		"address":  address,
		"register": register,
		"value":    value,
	})
}

// WriteI2CRegister writes to an I2C register.
// @Summary Write I2C register
// @Description Writes a value to an I2C device register
// @Tags I2C
// @Accept json
// @Produce json
// @Param bus query int true "I2C bus number" example(1)
// @Param address query string true "I2C address" example(0x36)
// @Param register query string true "Register address" example(0x06)
// @Param value query string true "Value to write" example(0x40)
// @Param mode query string false "Write mode" Enums(b, w, i) default(b)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /i2c/write [post]
func (h *HALHandler) WriteI2CRegister(w http.ResponseWriter, r *http.Request) {
	busStr := r.URL.Query().Get("bus")
	bus, err := strconv.Atoi(busStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid bus number")
		return
	}

	// HF04-03 + HF04-04: Validate I2C bus range
	if err := validateI2CBus(bus); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-03: Validate I2C address
	address := r.URL.Query().Get("address")
	if err := validateI2CAddress(address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-03: Validate I2C register
	register := r.URL.Query().Get("register")
	if err := validateI2CRegister(register); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// HF04-03: Validate I2C value (same format as register — 0xNN)
	value := r.URL.Query().Get("value")
	if value == "" {
		errorResponse(w, http.StatusBadRequest, "value is required")
		return
	}
	if err := validateI2CRegister(value); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid I2C value format (expected 0xNN)")
		return
	}

	// HF04-03: Validate I2C mode
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "b"
	}
	if err := validateI2CMode(mode); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	output, err := execWithTimeout(r.Context(), "i2cset", "-y", strconv.Itoa(bus), address, register, value, mode)
	if err != nil {
		log.Printf("i2cset bus=%d addr=%s reg=%s val=%s failed: %v, output: %s", bus, address, register, value, err, output)
		errorResponse(w, http.StatusInternalServerError, sanitizeExecError("I2C write", err))
		return
	}

	successResponse(w, fmt.Sprintf("wrote %s to %s register %s", value, address, register))
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) parseI2CDetectOutput(output string) []I2CDevice {
	var devices []I2CDevice

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, ":") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				// Parse row address (first column)
				rowStr := strings.TrimSpace(parts[0])
				rowAddr, err := strconv.ParseInt(rowStr, 16, 64)
				if err != nil {
					continue
				}

				// Parse addresses in this row
				addresses := strings.Fields(parts[1])
				for i, addr := range addresses {
					if addr != "--" && addr != "UU" && addr != "" {
						fullAddr := int(rowAddr)*16 + i
						devices = append(devices, I2CDevice{
							Address:    fmt.Sprintf("0x%02x", fullAddr),
							AddressDec: fullAddr,
						})
					}
				}
			}
		}
	}

	return devices
}

// identifyI2CDevice returns device type and name based on common addresses
func identifyI2CDevice(address int) (deviceType, name string) {
	knownDevices := map[int]struct {
		Type string
		Name string
	}{
		0x36: {"MAX17040", "Fuel Gauge"},
		0x68: {"DS3231", "RTC"},
		0x69: {"DS1307", "RTC"},
		0x76: {"BME280", "Environmental Sensor"},
		0x77: {"BME280", "Environmental Sensor (alt addr)"},
		0x48: {"ADS1115", "ADC"},
		0x3C: {"SSD1306", "OLED Display"},
		0x3D: {"SSD1306", "OLED Display (alt addr)"},
		0x27: {"PCF8574", "I/O Expander"},
		0x20: {"PCF8574", "I/O Expander (alt addr)"},
		0x50: {"AT24C32", "EEPROM"},
		0x57: {"AT24C32", "EEPROM (RTC module)"},
		0x1E: {"HMC5883L", "Magnetometer"},
		0x53: {"ADXL345", "Accelerometer"},
		0x29: {"VL53L0X", "Distance Sensor"},
		0x39: {"APDS9960", "Gesture Sensor"},
		0x40: {"INA219", "Current Sensor"},
		0x44: {"SHT31", "Humidity Sensor"},
		0x5A: {"MLX90614", "IR Temperature"},
		0x60: {"MCP4725", "DAC"},
	}

	if device, ok := knownDevices[address]; ok {
		return device.Type, device.Name
	}

	return "", ""
}
