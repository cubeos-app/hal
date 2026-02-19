package handlers

import (
	"context"
	"encoding/json"

	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// Sensor Types
// ============================================================================

// OneWireDevice represents a 1-Wire device.
// @Description 1-Wire device (e.g., DS18B20 temperature sensor)
type OneWireDevice struct {
	ID          string  `json:"id" example:"28-00000abcdef0"`
	Family      string  `json:"family" example:"28"`
	Type        string  `json:"type" example:"DS18B20"`
	Path        string  `json:"path" example:"/sys/bus/w1/devices/28-00000abcdef0"`
	Temperature float64 `json:"temperature,omitempty" example:"23.5"`
	Unit        string  `json:"unit,omitempty" example:"celsius"`
	Valid       bool    `json:"valid" example:"true"`
}

// OneWireDevicesResponse represents 1-Wire devices list.
// @Description List of 1-Wire devices
type OneWireDevicesResponse struct {
	Count   int             `json:"count" example:"3"`
	Devices []OneWireDevice `json:"devices"`
}

// BME280Reading represents BME280 sensor reading.
// @Description BME280 environmental sensor reading
type BME280Reading struct {
	Available   bool    `json:"available" example:"true"`
	Temperature float64 `json:"temperature" example:"23.5"`
	TempUnit    string  `json:"temp_unit" example:"celsius"`
	Humidity    float64 `json:"humidity" example:"45.2"`
	HumidUnit   string  `json:"humidity_unit" example:"percent"`
	Pressure    float64 `json:"pressure" example:"1013.25"`
	PressUnit   string  `json:"pressure_unit" example:"hPa"`
	Altitude    float64 `json:"altitude,omitempty" example:"50.0"`
	AltUnit     string  `json:"altitude_unit,omitempty" example:"meters"`
	Timestamp   string  `json:"timestamp" example:"2026-02-03T16:30:00Z"`
	I2CAddress  string  `json:"i2c_address" example:"0x76"`
	I2CBus      int     `json:"i2c_bus" example:"1"`
}

// SensorReading represents a generic sensor reading.
// @Description Generic sensor reading
type SensorReading struct {
	SensorID  string  `json:"sensor_id" example:"28-00000abcdef0"`
	Type      string  `json:"type" example:"temperature"`
	Value     float64 `json:"value" example:"23.5"`
	Unit      string  `json:"unit" example:"celsius"`
	Timestamp string  `json:"timestamp" example:"2026-02-03T16:30:00Z"`
	Valid     bool    `json:"valid" example:"true"`
}

// ============================================================================
// 1-Wire Handlers
// ============================================================================

// Get1WireDevices lists 1-Wire devices.
// @Summary List 1-Wire devices
// @Description Returns list of 1-Wire devices (DS18B20, etc.)
// @Tags Sensors
// @Accept json
// @Produce json
// @Success 200 {object} OneWireDevicesResponse
// @Failure 500 {object} ErrorResponse
// @Router /sensors/1wire/devices [get]
func (h *HALHandler) Get1WireDevices(w http.ResponseWriter, r *http.Request) {
	devices := h.scan1WireDevices()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

// Read1WireDevice reads a specific 1-Wire device.
// @Summary Read 1-Wire device
// @Description Reads temperature from a specific 1-Wire sensor
// @Tags Sensors
// @Accept json
// @Produce json
// @Param id path string true "Device ID" example(28-00000abcdef0)
// @Success 200 {object} OneWireDevice
// @Failure 404 {object} ErrorResponse "Device not found"
// @Failure 500 {object} ErrorResponse
// @Router /sensors/1wire/device/{id} [get]
func (h *HALHandler) Read1WireDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "id")
	if err := validate1WireDeviceID(deviceID); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	device := h.read1WireDevice(deviceID)
	if device.ID == "" {
		errorResponse(w, http.StatusNotFound, "device not found")
		return
	}

	jsonResponse(w, http.StatusOK, device)
}

// Read1WireTemperatures reads all 1-Wire temperature sensors.
// @Summary Read all temperatures
// @Description Reads temperature from all DS18B20 sensors
// @Tags Sensors
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /sensors/1wire/temperatures [get]
func (h *HALHandler) Read1WireTemperatures(w http.ResponseWriter, r *http.Request) {
	devices := h.scan1WireDevices()

	var readings []SensorReading
	for _, device := range devices {
		if device.Valid && device.Family == "28" { // DS18B20
			readings = append(readings, SensorReading{
				SensorID:  device.ID,
				Type:      "temperature",
				Value:     device.Temperature,
				Unit:      "celsius",
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Valid:     true,
			})
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":    len(readings),
		"readings": readings,
	})
}

// ============================================================================
// BME280 Handlers
// ============================================================================

// ReadBME280 reads BME280 sensor.
// @Summary Read BME280
// @Description Reads temperature, humidity, and pressure from BME280 sensor
// @Tags Sensors
// @Accept json
// @Produce json
// @Param address query string false "I2C address" default(0x76) Enums(0x76, 0x77)
// @Param bus query int false "I2C bus" default(1)
// @Success 200 {object} BME280Reading
// @Failure 404 {object} ErrorResponse "Sensor not found"
// @Failure 500 {object} ErrorResponse
// @Router /sensors/bme280 [get]
func (h *HALHandler) ReadBME280(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if address == "" {
		address = "0x76"
	}
	if err := validateI2CAddress(address); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	bus := 1
	if busParam := r.URL.Query().Get("bus"); busParam != "" {
		var err error
		bus, err = strconv.Atoi(busParam)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid bus number")
			return
		}
	}
	if err := validateI2CBus(bus); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	reading := h.readBME280(bus, address)

	if !reading.Available {
		errorResponse(w, http.StatusNotFound, "BME280 sensor not found at "+address)
		return
	}

	jsonResponse(w, http.StatusOK, reading)
}

// DetectBME280 detects BME280 sensors.
// @Summary Detect BME280
// @Description Scans I2C bus for BME280 sensors
// @Tags Sensors
// @Accept json
// @Produce json
// @Param bus query int false "I2C bus" default(1)
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /sensors/bme280/detect [get]
func (h *HALHandler) DetectBME280(w http.ResponseWriter, r *http.Request) {
	bus := 1
	if busParam := r.URL.Query().Get("bus"); busParam != "" {
		var err error
		bus, err = strconv.Atoi(busParam)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid bus number")
			return
		}
	}
	if err := validateI2CBus(bus); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	var found []map[string]interface{}

	// Try common BME280 addresses
	addresses := []string{"0x76", "0x77"}
	for _, addr := range addresses {
		reading := h.readBME280(bus, addr)
		if reading.Available {
			found = append(found, map[string]interface{}{
				"address": addr,
				"bus":     bus,
				"type":    "BME280",
			})
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(found),
		"sensors": found,
	})
}

// ============================================================================
// Generic Sensor Handlers
// ============================================================================

// GetAllSensorReadings returns all sensor readings.
// @Summary Get all sensors
// @Description Returns readings from all available sensors
// @Tags Sensors
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} ErrorResponse
// @Router /sensors/all [get]
func (h *HALHandler) GetAllSensorReadings(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	// 1-Wire sensors
	oneWireDevices := h.scan1WireDevices()
	if len(oneWireDevices) > 0 {
		response["1wire"] = oneWireDevices
	}

	// BME280
	bme280 := h.readBME280(1, "0x76")
	if bme280.Available {
		response["bme280"] = bme280
	} else {
		// Try alternate address
		bme280 = h.readBME280(1, "0x77")
		if bme280.Available {
			response["bme280"] = bme280
		}
	}

	jsonResponse(w, http.StatusOK, response)
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) scan1WireDevices() []OneWireDevice {
	var devices []OneWireDevice

	// 1-Wire devices are in /sys/bus/w1/devices/
	w1Path := "/sys/bus/w1/devices"
	entries, err := os.ReadDir(w1Path)
	if err != nil {
		return devices
	}

	for _, entry := range entries {
		name := entry.Name()

		// Skip w1_bus_master
		if strings.HasPrefix(name, "w1_bus") {
			continue
		}

		// Parse device ID (format: XX-XXXXXXXXXXXX)
		parts := strings.Split(name, "-")
		if len(parts) != 2 {
			continue
		}

		device := OneWireDevice{
			ID:     name,
			Family: parts[0],
			Path:   filepath.Join(w1Path, name),
		}

		// Determine type by family code
		switch device.Family {
		case "28":
			device.Type = "DS18B20"
		case "10":
			device.Type = "DS18S20"
		case "22":
			device.Type = "DS1822"
		case "3b":
			device.Type = "DS1825"
		default:
			device.Type = "unknown"
		}

		// Read temperature if it's a temperature sensor
		if device.Family == "28" || device.Family == "10" || device.Family == "22" || device.Family == "3b" {
			tempFile := filepath.Join(device.Path, "temperature")
			if data, err := os.ReadFile(tempFile); err == nil {
				// Temperature is in millidegrees
				if temp, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
					device.Temperature = float64(temp) / 1000.0
					device.Unit = "celsius"
					device.Valid = true
				}
			} else {
				// Try w1_slave file (older format)
				slaveFile := filepath.Join(device.Path, "w1_slave")
				if data, err := os.ReadFile(slaveFile); err == nil {
					lines := strings.Split(string(data), "\n")
					for _, line := range lines {
						if strings.Contains(line, "t=") {
							parts := strings.Split(line, "t=")
							if len(parts) == 2 {
								if temp, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
									device.Temperature = float64(temp) / 1000.0
									device.Unit = "celsius"
									device.Valid = temp != 85000 // 85000 = power-on reset value
								}
							}
						}
					}
				}
			}
		}

		devices = append(devices, device)
	}

	return devices
}

func (h *HALHandler) read1WireDevice(deviceID string) OneWireDevice {
	device := OneWireDevice{}

	w1Path := filepath.Join("/sys/bus/w1/devices", deviceID)
	if _, err := os.Stat(w1Path); os.IsNotExist(err) {
		return device
	}

	parts := strings.Split(deviceID, "-")
	if len(parts) != 2 {
		return device
	}

	device.ID = deviceID
	device.Family = parts[0]
	device.Path = w1Path

	// Determine type
	switch device.Family {
	case "28":
		device.Type = "DS18B20"
	case "10":
		device.Type = "DS18S20"
	default:
		device.Type = "unknown"
	}

	// Read temperature
	tempFile := filepath.Join(w1Path, "temperature")
	if data, err := os.ReadFile(tempFile); err == nil {
		if temp, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			device.Temperature = float64(temp) / 1000.0
			device.Unit = "celsius"
			device.Valid = true
		}
	}

	return device
}

func (h *HALHandler) readBME280(bus int, address string) BME280Reading {
	reading := BME280Reading{
		Available:  false,
		I2CAddress: address,
		I2CBus:     bus,
		TempUnit:   "celsius",
		HumidUnit:  "percent",
		PressUnit:  "hPa",
		AltUnit:    "meters",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	ctx := context.Background()

	// Try using bme280 tool if available
	if output, err := execWithTimeout(ctx, "bme280", "-a", address, "-b", strconv.Itoa(bus), "-j"); err == nil {
		var data map[string]interface{}
		if json.Unmarshal([]byte(output), &data) == nil {
			reading.Available = true
			if v, ok := data["temperature"].(float64); ok {
				reading.Temperature = v
			}
			if v, ok := data["humidity"].(float64); ok {
				reading.Humidity = v
			}
			if v, ok := data["pressure"].(float64); ok {
				reading.Pressure = v
			}
			return reading
		}
	}

	// Try Python script as fallback
	pythonScript := `
import smbus2
import bme280
import json
import sys

bus = smbus2.SMBus(int(sys.argv[1]))
address = int(sys.argv[2], 16)
calibration_params = bme280.load_calibration_params(bus, address)
data = bme280.sample(bus, address, calibration_params)
print(json.dumps({
    "temperature": round(data.temperature, 2),
    "humidity": round(data.humidity, 2),
    "pressure": round(data.pressure, 2)
}))
`

	if output, err := execWithTimeout(ctx, "python3", "-c", pythonScript, strconv.Itoa(bus), address); err == nil {
		var data map[string]interface{}
		if json.Unmarshal([]byte(output), &data) == nil {
			reading.Available = true
			if v, ok := data["temperature"].(float64); ok {
				reading.Temperature = v
			}
			if v, ok := data["humidity"].(float64); ok {
				reading.Humidity = v
			}
			if v, ok := data["pressure"].(float64); ok {
				reading.Pressure = v
				// Calculate approximate altitude from pressure
				reading.Altitude = 44330 * (1 - (reading.Pressure/1013.25)*0.190284)
			}
		}
	}

	return reading
}
