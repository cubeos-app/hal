package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ============================================================================
// GPIO Types
// ============================================================================

// GPIOPin represents a GPIO pin.
// @Description GPIO pin status
type GPIOPin struct {
	Pin      int    `json:"pin" example:"17"`
	Mode     string `json:"mode" example:"output"`
	Value    int    `json:"value" example:"1"`
	Pull     string `json:"pull,omitempty" example:"none"`
	Function string `json:"function,omitempty" example:"GPIO17"`
	Exported bool   `json:"exported" example:"true"`
}

// GPIOPinsResponse represents GPIO pins status.
// @Description GPIO pins status
type GPIOPinsResponse struct {
	Chip  string    `json:"chip" example:"gpiochip4"`
	Count int       `json:"count" example:"26"`
	Pins  []GPIOPin `json:"pins"`
}

// GPIOSetRequest represents GPIO set request.
// @Description GPIO pin set parameters
type GPIOSetRequest struct {
	Pin   int `json:"pin" example:"17"`
	Value int `json:"value" example:"1"`
}

// GPIOModeRequest represents GPIO mode request.
// @Description GPIO pin mode parameters
type GPIOModeRequest struct {
	Pin  int    `json:"pin" example:"17"`
	Mode string `json:"mode" example:"output"`
	Pull string `json:"pull,omitempty" example:"up"`
}

// ============================================================================
// GPIO Handlers
// ============================================================================

// GetGPIOStatus returns GPIO pins status.
// @Summary Get GPIO status
// @Description Returns status of all GPIO pins
// @Tags GPIO
// @Accept json
// @Produce json
// @Success 200 {object} GPIOPinsResponse
// @Failure 500 {object} ErrorResponse
// @Router /gpio/pins [get]
func (h *HALHandler) GetGPIOStatus(w http.ResponseWriter, r *http.Request) {
	pins := h.scanGPIOPins(r)

	// Determine chip
	chip := "gpiochip4" // Pi 5
	if _, err := os.Stat("/sys/class/gpio/gpiochip0"); err == nil {
		// Check if it's Pi 4 or earlier
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			if !strings.Contains(string(data), "Raspberry Pi 5") {
				chip = "gpiochip0"
			}
		}
	}

	jsonResponse(w, http.StatusOK, GPIOPinsResponse{
		Chip:  chip,
		Count: len(pins),
		Pins:  pins,
	})
}

// GetGPIOPin returns a specific GPIO pin status.
// @Summary Get GPIO pin
// @Description Returns status of a specific GPIO pin
// @Tags GPIO
// @Accept json
// @Produce json
// @Param pin path int true "GPIO pin number (BCM)" example(17)
// @Success 200 {object} GPIOPin
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gpio/pin/{pin} [get]
func (h *HALHandler) GetGPIOPin(w http.ResponseWriter, r *http.Request) {
	pinStr := chi.URLParam(r, "pin")
	pin, err := strconv.Atoi(pinStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid pin number")
		return
	}

	if pin < 0 || pin > 27 {
		errorResponse(w, http.StatusBadRequest, "pin must be 0-27")
		return
	}

	pinInfo := h.readGPIOPin(r, pin)
	jsonResponse(w, http.StatusOK, pinInfo)
}

// SetGPIOPin sets a GPIO pin value.
// @Summary Set GPIO pin
// @Description Sets the value of a GPIO pin
// @Tags GPIO
// @Accept json
// @Produce json
// @Param request body GPIOSetRequest true "Pin and value"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gpio/pin [post]
func (h *HALHandler) SetGPIOPin(w http.ResponseWriter, r *http.Request) {
	// HF04-08: Apply limitBody
	r = limitBody(r, 1<<20)

	var req GPIOSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Pin < 0 || req.Pin > 27 {
		errorResponse(w, http.StatusBadRequest, "invalid pin number (0-27)")
		return
	}

	if req.Value != 0 && req.Value != 1 {
		errorResponse(w, http.StatusBadRequest, "value must be 0 or 1")
		return
	}

	// Determine GPIO chip
	chip := h.getGPIOChip()

	// Try gpioset first (gpiod)
	_, err := execWithTimeout(r.Context(), "gpioset", chip, fmt.Sprintf("%d=%d", req.Pin, req.Value))
	if err != nil {
		// Fallback to sysfs
		if err := h.setGPIOSysfs(req.Pin, req.Value); err != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to set GPIO: "+err.Error())
			return
		}
	}

	successResponse(w, fmt.Sprintf("GPIO %d set to %d", req.Pin, req.Value))
}

// SetGPIOMode sets GPIO pin mode.
// @Summary Set GPIO mode
// @Description Sets the mode (input/output) and pull resistor of a GPIO pin
// @Tags GPIO
// @Accept json
// @Produce json
// @Param request body GPIOModeRequest true "Pin mode parameters"
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gpio/mode [post]
func (h *HALHandler) SetGPIOMode(w http.ResponseWriter, r *http.Request) {
	// HF04-08: Apply limitBody
	r = limitBody(r, 1<<20)

	var req GPIOModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Pin < 0 || req.Pin > 27 {
		errorResponse(w, http.StatusBadRequest, "invalid pin number (0-27)")
		return
	}

	if req.Mode != "input" && req.Mode != "output" && req.Mode != "in" && req.Mode != "out" {
		errorResponse(w, http.StatusBadRequest, "mode must be 'input' or 'output'")
		return
	}

	// Export pin via sysfs
	exportPath := "/sys/class/gpio/export"
	valuePath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", req.Pin)
	dirPath := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", req.Pin)

	// Export if needed
	if _, err := os.Stat(valuePath); os.IsNotExist(err) {
		os.WriteFile(exportPath, []byte(strconv.Itoa(req.Pin)), 0644)
		time.Sleep(100 * time.Millisecond)
	}

	// Set direction
	direction := "out"
	if req.Mode == "input" || req.Mode == "in" {
		direction = "in"
	}

	if err := os.WriteFile(dirPath, []byte(direction), 0644); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to set mode: "+err.Error())
		return
	}

	successResponse(w, fmt.Sprintf("GPIO %d mode set to %s", req.Pin, req.Mode))
}

// ExportGPIOPin exports a GPIO pin.
// @Summary Export GPIO pin
// @Description Exports a GPIO pin for userspace control
// @Tags GPIO
// @Accept json
// @Produce json
// @Param pin path int true "GPIO pin number (BCM)" example(17)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gpio/export/{pin} [post]
func (h *HALHandler) ExportGPIOPin(w http.ResponseWriter, r *http.Request) {
	pinStr := chi.URLParam(r, "pin")
	pin, err := strconv.Atoi(pinStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid pin number")
		return
	}

	if pin < 0 || pin > 27 {
		errorResponse(w, http.StatusBadRequest, "pin must be 0-27")
		return
	}

	exportPath := "/sys/class/gpio/export"
	if err := os.WriteFile(exportPath, []byte(strconv.Itoa(pin)), 0644); err != nil {
		if !os.IsExist(err) {
			errorResponse(w, http.StatusInternalServerError, "failed to export GPIO: "+err.Error())
			return
		}
	}

	successResponse(w, fmt.Sprintf("GPIO %d exported", pin))
}

// UnexportGPIOPin unexports a GPIO pin.
// @Summary Unexport GPIO pin
// @Description Unexports a GPIO pin from userspace control
// @Tags GPIO
// @Accept json
// @Produce json
// @Param pin path int true "GPIO pin number (BCM)" example(17)
// @Success 200 {object} SuccessResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /gpio/unexport/{pin} [post]
func (h *HALHandler) UnexportGPIOPin(w http.ResponseWriter, r *http.Request) {
	pinStr := chi.URLParam(r, "pin")
	pin, err := strconv.Atoi(pinStr)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid pin number")
		return
	}

	// HF04-07: Add range validation (was missing, unlike ExportGPIOPin)
	if pin < 0 || pin > 27 {
		errorResponse(w, http.StatusBadRequest, "pin must be 0-27")
		return
	}

	unexportPath := "/sys/class/gpio/unexport"
	if err := os.WriteFile(unexportPath, []byte(strconv.Itoa(pin)), 0644); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to unexport GPIO: "+err.Error())
		return
	}

	successResponse(w, fmt.Sprintf("GPIO %d unexported", pin))
}

// ============================================================================
// Helper Functions
// ============================================================================

func (h *HALHandler) getGPIOChip() string {
	// Pi 5 uses gpiochip4, Pi 4 and earlier use gpiochip0
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		if strings.Contains(string(data), "Raspberry Pi 5") {
			return "gpiochip4"
		}
	}
	return "gpiochip0"
}

func (h *HALHandler) scanGPIOPins(r *http.Request) []GPIOPin {
	var pins []GPIOPin

	// BCM GPIO pins commonly used on Raspberry Pi header
	bcmPins := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27}

	for _, pin := range bcmPins {
		pinInfo := h.readGPIOPin(r, pin)
		pins = append(pins, pinInfo)
	}

	return pins
}

func (h *HALHandler) readGPIOPin(r *http.Request, pin int) GPIOPin {
	pinInfo := GPIOPin{
		Pin:      pin,
		Mode:     "unknown",
		Value:    0,
		Exported: false,
		Function: fmt.Sprintf("GPIO%d", pin),
	}

	// Check if exported via sysfs
	valuePath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	dirPath := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", pin)

	if _, err := os.Stat(valuePath); err == nil {
		pinInfo.Exported = true

		// Read value
		if data, err := os.ReadFile(valuePath); err == nil {
			val, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			pinInfo.Value = val
		}

		// Read direction
		if data, err := os.ReadFile(dirPath); err == nil {
			pinInfo.Mode = strings.TrimSpace(string(data))
		}
	} else {
		// Try gpioget
		chip := h.getGPIOChip()
		output, err := execWithTimeout(r.Context(), "gpioget", chip, strconv.Itoa(pin))
		if err == nil {
			val, _ := strconv.Atoi(strings.TrimSpace(output))
			pinInfo.Value = val
			pinInfo.Mode = "input" // gpioget implies input mode
		}
	}

	return pinInfo
}

func (h *HALHandler) setGPIOSysfs(pin, value int) error {
	exportPath := "/sys/class/gpio/export"
	valuePath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	dirPath := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", pin)

	// Export if needed
	if _, err := os.Stat(valuePath); os.IsNotExist(err) {
		if err := os.WriteFile(exportPath, []byte(strconv.Itoa(pin)), 0644); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Set direction to output
	if err := os.WriteFile(dirPath, []byte("out"), 0644); err != nil {
		return err
	}

	// Set value
	return os.WriteFile(valuePath, []byte(strconv.Itoa(value)), 0644)
}
