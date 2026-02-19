package devices

import (
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"syscall"
	"unsafe"
)

// I2C ioctl commands
const (
	I2C_SLAVE       = 0x0703
	I2C_SLAVE_FORCE = 0x0706
	I2C_RDWR        = 0x0707
	I2C_SMBUS       = 0x0720
)

// I2CBus represents an I2C bus device
type I2CBus struct {
	file *os.File
	bus  int
}

// OpenI2CBus opens an I2C bus (typically bus 1 on Raspberry Pi)
func OpenI2CBus(bus int) (*I2CBus, error) {
	path := fmt.Sprintf("/dev/i2c-%d", bus)
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open I2C bus %d: %w", bus, err)
	}
	return &I2CBus{file: file, bus: bus}, nil
}

// Close closes the I2C bus
func (b *I2CBus) Close() error {
	if b.file != nil {
		return b.file.Close()
	}
	return nil
}

// SetAddress sets the I2C slave address for subsequent operations
func (b *I2CBus) SetAddress(addr uint8) error {
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		b.file.Fd(),
		I2C_SLAVE,
		uintptr(addr),
	)
	if errno != 0 {
		return fmt.Errorf("failed to set I2C address 0x%02X: %v", addr, errno)
	}
	return nil
}

// ReadByte reads a single byte from a register
func (b *I2CBus) ReadRegByte(addr uint8, reg uint8) (uint8, error) {
	if err := b.SetAddress(addr); err != nil {
		return 0, err
	}

	// Write register address
	if _, err := b.file.Write([]byte{reg}); err != nil {
		return 0, fmt.Errorf("failed to write register 0x%02X: %w", reg, err)
	}

	// Read one byte
	buf := make([]byte, 1)
	if _, err := b.file.Read(buf); err != nil {
		return 0, fmt.Errorf("failed to read from register 0x%02X: %w", reg, err)
	}

	return buf[0], nil
}

// ReadWord reads a 16-bit word from a register (big-endian, as used by MAX17040)
func (b *I2CBus) ReadWord(addr uint8, reg uint8) (uint16, error) {
	if err := b.SetAddress(addr); err != nil {
		return 0, err
	}

	// Write register address
	if _, err := b.file.Write([]byte{reg}); err != nil {
		return 0, fmt.Errorf("failed to write register 0x%02X: %w", reg, err)
	}

	// Read two bytes
	buf := make([]byte, 2)
	if _, err := b.file.Read(buf); err != nil {
		return 0, fmt.Errorf("failed to read from register 0x%02X: %w", reg, err)
	}

	// MAX17040 uses big-endian
	return binary.BigEndian.Uint16(buf), nil
}

// WriteByte writes a single byte to a register
func (b *I2CBus) WriteRegByte(addr uint8, reg uint8, value uint8) error {
	if err := b.SetAddress(addr); err != nil {
		return err
	}

	_, err := b.file.Write([]byte{reg, value})
	if err != nil {
		return fmt.Errorf("failed to write to register 0x%02X: %w", reg, err)
	}

	return nil
}

// WriteWord writes a 16-bit word to a register (big-endian)
func (b *I2CBus) WriteWord(addr uint8, reg uint8, value uint16) error {
	if err := b.SetAddress(addr); err != nil {
		return err
	}

	buf := make([]byte, 3)
	buf[0] = reg
	binary.BigEndian.PutUint16(buf[1:], value)

	_, err := b.file.Write(buf)
	if err != nil {
		return fmt.Errorf("failed to write to register 0x%02X: %w", reg, err)
	}

	return nil
}

// ScanBus scans the I2C bus for devices and returns found addresses.
// Scans standard range 0x03-0x77, skipping reserved addresses 0x00-0x02 and 0x78-0x7F.
func (b *I2CBus) ScanBus() []uint8 {
	var found []uint8

	for addr := uint8(0x03); addr <= 0x77; addr++ {
		// HF04-10: Removed incorrect 0x30-0x37 skip — these are valid EEPROM addresses.
		// Standard I2C reserved ranges (0x00-0x02 and 0x78-0x7F) are excluded by the loop bounds.
		if err := b.SetAddress(addr); err != nil {
			continue
		}

		// Try to read a byte - if successful, device is present
		buf := make([]byte, 1)
		_, err := b.file.Read(buf)
		if err == nil {
			found = append(found, addr)
		}
	}

	return found
}

// I2CDeviceInfo contains information about a detected I2C device
type I2CDeviceInfo struct {
	Address     uint8  `json:"address"`
	AddressHex  string `json:"address_hex"`
	Description string `json:"description,omitempty"`
}

// KnownI2CDevices maps addresses to device descriptions
var KnownI2CDevices = map[uint8]string{
	0x20: "PCF8574 GPIO Expander",
	0x27: "LCD Display (PCF8574)",
	0x36: "MAX17040/MAX17048 Fuel Gauge",
	0x40: "INA219 Power Monitor",
	0x41: "INA219 (alt address)",
	0x48: "TMP102/ADS1115",
	0x50: "AT24C32 EEPROM",
	0x51: "AT24C32 EEPROM (alt)",
	0x57: "MAX30102 Pulse Oximeter",
	0x68: "DS3231 RTC / MPU6050",
	0x69: "MPU6050 (alt address)",
	0x76: "BME280/BMP280",
	0x77: "BME280/BMP280 (alt)",
}

// GetDeviceInfo returns info about a device at the given address
func GetDeviceInfo(addr uint8) I2CDeviceInfo {
	info := I2CDeviceInfo{
		Address:    addr,
		AddressHex: fmt.Sprintf("0x%02X", addr),
	}
	if desc, ok := KnownI2CDevices[addr]; ok {
		info.Description = desc
	}
	return info
}

// GPIO handling for Pi 5 (gpiochip4)

// GPIOChip represents a GPIO chip device
type GPIOChip struct {
	file *os.File
	path string
}

// GPIO ioctl structures and constants
const (
	GPIO_GET_LINEHANDLE_IOCTL        = 0xC16CB403
	GPIO_GET_LINEEVENT_IOCTL         = 0xC030B404
	GPIOHANDLE_SET_LINE_VALUES_IOCTL = 0xC040B409
	GPIOHANDLE_GET_LINE_VALUES_IOCTL = 0xC040B408

	GPIOHANDLE_REQUEST_INPUT  = 1 << 0
	GPIOHANDLE_REQUEST_OUTPUT = 1 << 1
)

// gpioHandleRequest is the ioctl structure for requesting a line handle
type gpioHandleRequest struct {
	lineOffsets   [64]uint32
	flags         uint32
	defaultValues [64]uint8
	consumerLabel [32]byte
	lines         uint32
	fd            int32
}

// gpioHandleData is the ioctl structure for getting/setting line values
type gpioHandleData struct {
	values [64]uint8
}

// reGPIOChipNameLocal validates GPIO chip names (e.g., "gpiochip0", "gpiochip4")
var reGPIOChipNameLocal = regexp.MustCompile(`^gpiochip[0-9]+$`)

// OpenGPIOChip opens a GPIO chip (gpiochip4 for Pi 5, gpiochip0 for Pi 4)
func OpenGPIOChip(chip string) (*GPIOChip, error) {
	// HF04-09: Validate chip name to prevent path traversal (e.g., "../../etc/shadow")
	if !reGPIOChipNameLocal.MatchString(chip) {
		return nil, fmt.Errorf("invalid GPIO chip name: %s", chip)
	}
	path := fmt.Sprintf("/dev/%s", chip)
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open GPIO chip %s: %w", chip, err)
	}
	return &GPIOChip{file: file, path: path}, nil
}

// Close closes the GPIO chip
func (g *GPIOChip) Close() error {
	if g.file != nil {
		return g.file.Close()
	}
	return nil
}

// GPIOLine represents a requested GPIO line
type GPIOLine struct {
	fd     int
	offset uint32
	output bool
}

// RequestLine requests a GPIO line for input or output
func (g *GPIOChip) RequestLine(offset uint32, output bool, defaultValue uint8, consumer string) (*GPIOLine, error) {
	req := gpioHandleRequest{
		lines: 1,
	}
	req.lineOffsets[0] = offset

	if output {
		req.flags = GPIOHANDLE_REQUEST_OUTPUT
		req.defaultValues[0] = defaultValue
	} else {
		req.flags = GPIOHANDLE_REQUEST_INPUT
	}

	// Copy consumer label
	copy(req.consumerLabel[:], consumer)

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		g.file.Fd(),
		GPIO_GET_LINEHANDLE_IOCTL,
		uintptr(unsafe.Pointer(&req)),
	)
	if errno != 0 {
		return nil, fmt.Errorf("failed to request GPIO line %d: %v", offset, errno)
	}

	return &GPIOLine{
		fd:     int(req.fd),
		offset: offset,
		output: output,
	}, nil
}

// GetValue reads the current value of the GPIO line
func (l *GPIOLine) GetValue() (uint8, error) {
	data := gpioHandleData{}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(l.fd),
		GPIOHANDLE_GET_LINE_VALUES_IOCTL,
		uintptr(unsafe.Pointer(&data)),
	)
	if errno != 0 {
		return 0, fmt.Errorf("failed to get GPIO value: %v", errno)
	}

	return data.values[0], nil
}

// SetValue sets the value of the GPIO line (output only)
func (l *GPIOLine) SetValue(value uint8) error {
	if !l.output {
		return fmt.Errorf("cannot set value on input line")
	}

	data := gpioHandleData{}
	data.values[0] = value

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(l.fd),
		GPIOHANDLE_SET_LINE_VALUES_IOCTL,
		uintptr(unsafe.Pointer(&data)),
	)
	if errno != 0 {
		return fmt.Errorf("failed to set GPIO value: %v", errno)
	}

	return nil
}

// Close releases the GPIO line
func (l *GPIOLine) Close() error {
	return syscall.Close(l.fd)
}

// DetectPiVersion attempts to detect Pi 4 vs Pi 5 for GPIO chip selection
func DetectPiVersion() (int, string) {
	// Check for Pi 5 by looking for gpiochip4
	if _, err := os.Stat("/dev/gpiochip4"); err == nil {
		return 5, "gpiochip4"
	}
	// Default to Pi 4 behavior
	return 4, "gpiochip0"
}
