package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Serial Transport — Meshtastic USB Serial Protocol (0x94 0xC3 framing)
// ============================================================================

// SerialTransport implements MeshtasticTransport for USB serial connections.
// Uses the Meshtastic 4-byte framed protobuf protocol:
//
//	Byte 0: 0x94 (START1)
//	Byte 1: 0xC3 (START2)
//	Byte 2: MSB of protobuf length
//	Byte 3: LSB of protobuf length
//	Byte 4..N: Protobuf payload (FromRadio or ToRadio)
type SerialTransport struct {
	mu sync.Mutex

	port      string // e.g., "/dev/ttyACM0" (empty = auto-detect)
	baud      int
	file      *os.File
	connected bool

	// Persistent read buffer — survives between RecvFromRadio calls so that
	// leftover bytes from multi-frame OS reads are not silently discarded.
	// Only accessed by readerLoop (single goroutine), no lock needed.
	accum []byte
}

const (
	meshStart1      byte = 0x94
	meshStart2      byte = 0xC3
	meshMaxPayload       = 512
	meshWakeLen          = 32 // Number of START2 bytes to send as wake sequence
	meshReadBufSize      = 1024
	meshReadTimeout      = 500 * time.Millisecond // Max time file.Read() can block
)

// NewSerialTransport creates a new USB serial transport.
// If port is empty, auto-detection will be attempted on Connect().
func NewSerialTransport(port string, baud int) *SerialTransport {
	if baud <= 0 {
		baud = 115200
	}
	return &SerialTransport{
		port: port,
		baud: baud,
	}
}

// Connect opens the serial port to the Meshtastic device.
// If no port is configured, it scans for Meshtastic-compatible devices.
func (t *SerialTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.connected && t.file != nil {
		return nil // Already connected
	}

	port := t.port
	if port == "" {
		var err error
		port, err = t.autoDetect(ctx)
		if err != nil {
			return fmt.Errorf("auto-detect failed: %w", err)
		}
	}

	// Validate port path
	if err := validateSerialPort(port); err != nil {
		return err
	}

	// Configure serial port via stty (115200 baud, 8N1, raw mode).
	// -hupcl prevents DTR from dropping on close, which would reboot
	// CDC-ACM devices like the T-Echo. The Meshtastic Python CLI does
	// the same via termios before opening the port.
	// Note: VMIN/VTIME are set but ignored by Go's runtime — it switches
	// the FD to O_NONBLOCK and uses epoll. We use SetReadDeadline instead.
	if _, err := execWithTimeout(ctx, "stty", "-F", port,
		fmt.Sprintf("%d", t.baud),
		"raw", "-echo", "-echoe", "-echok",
		"cs8", "-cstopb", "-parenb",
		"-crtscts", // No hardware flow control
		"-hupcl",   // Prevent DTR drop on close (avoids device reboot)
		"min", "1",
		"time", "1",
	); err != nil {
		return fmt.Errorf("stty config failed on %s: %w", port, err)
	}

	// Open serial port
	file, err := os.OpenFile(port, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", port, err)
	}

	t.file = file
	t.port = port
	t.connected = true
	t.accum = nil // Clear any stale read buffer from previous connection

	// Send wake sequence: 32 bytes of START2 (0xC3)
	// This forces the Meshtastic device to resync its serial parser
	wake := make([]byte, meshWakeLen)
	for i := range wake {
		wake[i] = meshStart2
	}
	if _, err := t.file.Write(wake); err != nil {
		t.closeLocked()
		return fmt.Errorf("failed to send wake sequence: %w", err)
	}

	// Small delay for device to process wake
	time.Sleep(200 * time.Millisecond)

	log.Printf("meshtastic: serial connected to %s at %d baud", port, t.baud)
	return nil
}

// Disconnect closes the serial port.
func (t *SerialTransport) Disconnect() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeLocked()
}

func (t *SerialTransport) closeLocked() error {
	t.connected = false
	t.accum = nil // Clear read buffer
	if t.file != nil {
		err := t.file.Close()
		t.file = nil
		return err
	}
	return nil
}

// SendToRadio sends a protobuf payload with 0x94 0xC3 framing.
func (t *SerialTransport) SendToRadio(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.connected || t.file == nil {
		return fmt.Errorf("not connected")
	}

	if len(data) > meshMaxPayload {
		return fmt.Errorf("payload too large (%d > %d bytes)", len(data), meshMaxPayload)
	}

	// Build framed packet: [START1, START2, len_msb, len_lsb, payload...]
	frame := make([]byte, 4+len(data))
	frame[0] = meshStart1
	frame[1] = meshStart2
	frame[2] = byte(len(data) >> 8)   // MSB
	frame[3] = byte(len(data) & 0xFF) // LSB
	copy(frame[4:], data)

	_, err := t.file.Write(frame)
	if err != nil {
		return fmt.Errorf("serial write failed: %w", err)
	}

	return nil
}

// RecvFromRadio blocks until a complete FromRadio protobuf is received.
// It scans for the 0x94 0xC3 start marker, reads the 2-byte length,
// then reads the full protobuf payload.
//
// The accumulation buffer (t.accum) persists across calls so that leftover
// bytes from multi-frame OS reads are not lost. During config download the
// radio sends ~25 packets in a burst — a single file.Read() often returns
// multiple frames. Without persistence the trailing frames were silently
// discarded, causing config_complete_id to never arrive and mesh packets
// to be lost after connect.
func (t *SerialTransport) RecvFromRadio(ctx context.Context) ([]byte, error) {
	buf := make([]byte, meshReadBufSize)

	for {
		// Check for a complete frame already in the persistent buffer
		// (leftover from a previous multi-frame read)
		if payload := t.extractFrame(); payload != nil {
			return payload, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		t.mu.Lock()
		if !t.connected || t.file == nil {
			t.mu.Unlock()
			return nil, fmt.Errorf("not connected")
		}
		file := t.file
		t.mu.Unlock()

		// Set a read deadline so file.Read() cannot block forever.
		// The stty "time 1" (100ms) setting may not survive Go's runtime
		// switching the FD to non-blocking mode. This deadline guarantees
		// we return to the ctx.Done() check within meshReadTimeout.
		file.SetReadDeadline(time.Now().Add(meshReadTimeout))
		n, err := file.Read(buf)
		if err != nil {
			if os.IsTimeout(err) {
				continue // Deadline hit, loop back to check ctx.Done()
			}
			return nil, fmt.Errorf("serial read failed: %w", err)
		}
		if n == 0 {
			continue
		}

		t.accum = append(t.accum, buf[:n]...)

		if payload := t.extractFrame(); payload != nil {
			return payload, nil
		}
	}
}

// extractFrame scans the persistent accum buffer for a complete framed
// protobuf message. Returns the payload if found, nil if more data needed.
func (t *SerialTransport) extractFrame() []byte {
	for {
		startIdx := findStartMarker(t.accum)
		if startIdx < 0 {
			// No start marker found — keep last byte in case it's START1
			if len(t.accum) > 1 {
				t.accum = t.accum[len(t.accum)-1:]
			}
			return nil
		}

		// Discard any bytes before the start marker
		if startIdx > 0 {
			t.accum = t.accum[startIdx:]
		}

		// Need at least 4 bytes for header
		if len(t.accum) < 4 {
			return nil // Wait for more data
		}

		// Read length (big-endian uint16)
		payloadLen := int(t.accum[2])<<8 | int(t.accum[3])

		// Sanity check
		if payloadLen > meshMaxPayload {
			// Corrupted — skip this start marker and look for next
			log.Printf("meshtastic: corrupted frame (len=%d > max=%d), re-scanning", payloadLen, meshMaxPayload)
			t.accum = t.accum[2:] // Skip past START1+START2, re-scan
			continue
		}

		if payloadLen == 0 {
			// Empty packet — skip
			t.accum = t.accum[4:]
			continue
		}

		// Check if we have the full payload
		totalLen := 4 + payloadLen
		if len(t.accum) < totalLen {
			return nil // Wait for more data
		}

		// Extract payload
		payload := make([]byte, payloadLen)
		copy(payload, t.accum[4:totalLen])

		// Advance past this frame (remaining data stays in t.accum)
		t.accum = t.accum[totalLen:]

		return payload
	}
}

// IsConnected returns the connection state.
func (t *SerialTransport) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected && t.file != nil
}

// TransportType returns "serial".
func (t *SerialTransport) TransportType() string {
	return "serial"
}

// DeviceAddress returns the serial port path.
func (t *SerialTransport) DeviceAddress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.port
}

// ============================================================================
// Auto-Detection
// ============================================================================

// GPS VID:PID pairs that should be excluded from Meshtastic/Iridium scans.
// These devices respond on /dev/ttyACM* and /dev/ttyUSB* but are GPS receivers
// managed by gpsd, not radio modems. (B68 fix)
var gpsVIDPIDs = map[string]bool{
	"1546:01a6": true, // u-blox 7 older variant (ACM)
	"1546:01a7": true, // u-blox 7 (ACM)
	"1546:01a8": true, // u-blox 8 (ACM)
	"1546:01a9": true, // u-blox 9 (ACM)
	"1546:0502": true, // u-blox M8 (generic)
	"067b:23a3": true, // Prolific PL2303 (common GPS USB-serial)
	"067b:2303": true, // Prolific PL2303 legacy (GPS adapters)
}

// resolveSysfsDevice resolves the sysfs "device" symlink for a tty port.
// /sys/class/tty/ttyACM0/device is a symlink to the actual device directory
// in /sys/devices/... — filepath.Join(path, "..") resolves ".." lexically,
// which walks up the LINK path, not the TARGET path. We must resolve the
// symlink first so that ".." navigation walks the real sysfs tree. (B68 fix)
func resolveSysfsDevice(port string) (string, error) {
	devName := filepath.Base(port)
	sysPath := fmt.Sprintf("/sys/class/tty/%s/device", devName)

	resolved, err := filepath.EvalSymlinks(sysPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve sysfs symlink for %s: %w", port, err)
	}
	return resolved, nil
}

// findUSBVIDPID walks up the sysfs tree from a tty device to find the USB
// device's idVendor/idProduct. CDC-ACM devices (like u-blox GPS) have the
// USB device node 2–3 levels above /sys/class/tty/ttyACMx/device, not 1.
// Returns "vid:pid" or empty string if not found. (B68 fix)
func findUSBVIDPID(port string) string {
	// B68: Resolve the symlink FIRST so that filepath.Join(current, "..")
	// walks the real sysfs tree, not the lexical link path.
	current, err := resolveSysfsDevice(port)
	if err != nil {
		return ""
	}

	// Walk up the sysfs tree looking for idVendor/idProduct.
	// ttyUSB devices: 1 level up (../idVendor)
	// ttyACM CDC-ACM: 2–3 levels up depending on hub topology
	for i := 0; i < 5; i++ {
		current = filepath.Dir(current) // B68: use Dir() instead of Join(current, "..")
		vidPath := filepath.Join(current, "idVendor")
		pidPath := filepath.Join(current, "idProduct")

		vidData, err := os.ReadFile(vidPath)
		if err != nil {
			continue
		}
		pidData, err := os.ReadFile(pidPath)
		if err != nil {
			continue
		}

		vid := strings.TrimSpace(string(vidData))
		pid := strings.TrimSpace(string(pidData))
		if vid != "" && pid != "" {
			return fmt.Sprintf("%s:%s", vid, pid)
		}
	}
	return ""
}

// isGPSDevice checks if a serial port belongs to a GPS receiver by VID:PID.
// Walks up the sysfs tree to handle CDC-ACM devices. (B68 fix)
func isGPSDevice(port string) bool {
	vidpid := findUSBVIDPID(port)
	if vidpid == "" {
		return false
	}
	if gpsVIDPIDs[vidpid] {
		log.Printf("meshtastic: %s identified as GPS device (VID:PID=%s)", port, vidpid)
		return true
	}
	return false
}

// isGPSClaimedPort checks if gpsd is configured to use a specific port.
// Reads /etc/default/gpsd DEVICES= line. (B68)
func isGPSClaimedPort(port string) bool {
	data, err := os.ReadFile("/etc/default/gpsd")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DEVICES=") {
			devices := strings.Trim(strings.TrimPrefix(line, "DEVICES="), "\"")
			for _, dev := range strings.Fields(devices) {
				if dev == port {
					return true
				}
			}
		}
	}
	return false
}

// isExcludedFromRadioScan returns true if a port should be excluded from
// Meshtastic/Iridium device scanning. Checks GPS VID:PID and gpsd claim. (B68)
func isExcludedFromRadioScan(port string) bool {
	if isGPSDevice(port) {
		return true
	}
	if isGPSClaimedPort(port) {
		log.Printf("meshtastic: %s claimed by gpsd, excluding from scan", port)
		return true
	}
	return false
}

// autoDetect scans /dev/ttyUSB* and /dev/ttyACM* for Meshtastic devices.
// It sends a wake sequence and attempts a protobuf handshake.
func (t *SerialTransport) autoDetect(ctx context.Context) (string, error) {
	candidates := findSerialCandidates()
	if len(candidates) == 0 {
		return "", fmt.Errorf("no serial devices found")
	}

	// First pass: match by VID:PID (most reliable)
	for _, port := range candidates {
		if err := validateSerialPort(port); err != nil {
			continue
		}
		// B68: Skip GPS devices
		if isExcludedFromRadioScan(port) {
			log.Printf("meshtastic: skipping %s (GPS/excluded device)", port)
			continue
		}

		// Check if this looks like a Meshtastic device by VID:PID
		if isMeshtasticVIDPID(port) {
			log.Printf("meshtastic: auto-detect found candidate %s (VID:PID match)", port)
			return port, nil
		}
	}

	// Second pass: try ACM devices that aren't GPS (ESP32-S3 native USB)
	for _, port := range candidates {
		if isExcludedFromRadioScan(port) {
			continue
		}
		if strings.Contains(port, "ttyACM") {
			log.Printf("meshtastic: auto-detect trying %s (ACM device, not GPS)", port)
			return port, nil
		}
	}

	// B68: Do NOT fall back to "first available" — that's how GPS gets grabbed.
	// If no VID:PID match and no ACM device, there's no Meshtastic device.
	return "", fmt.Errorf("no Meshtastic device found (checked %d ports, all excluded or unrecognized)", len(candidates))
}

// findSerialCandidates returns all candidate serial ports.
func findSerialCandidates() []string {
	var candidates []string

	// ACM devices (ESP32-S3 native USB — Heltec V3, T-Beam S3, etc.)
	matches, _ := filepath.Glob("/dev/ttyACM*")
	candidates = append(candidates, matches...)

	// USB devices (CP210x, FTDI, CH340 bridges — older boards)
	matches, _ = filepath.Glob("/dev/ttyUSB*")
	candidates = append(candidates, matches...)

	return candidates
}

// isMeshtasticVIDPID checks if a serial port's USB VID:PID matches known Meshtastic devices.
// B68: Uses findUSBVIDPID() with proper symlink resolution instead of manual path construction.
func isMeshtasticVIDPID(port string) bool {
	// Known Meshtastic VID:PID pairs
	knownDevices := map[string]bool{
		"303a:1001": true, // ESP32-S3 (Heltec V3, etc.)
		"1a86:55d4": true, // CH343 (T-Beam, Heltec V2)
		"1a86:7523": true, // CH340 (generic ESP32)
		"10c4:ea60": true, // CP2102/CP2104 (generic ESP32)
		"239a:8029": true, // RAK WisBlock (nRF52840)
		"1915:520f": true, // Nordic nRF52840 (RAK, T-Echo)
	}

	vidpid := findUSBVIDPID(port)
	return knownDevices[vidpid]
}

// scanMeshtasticPorts performs a non-destructive scan for Meshtastic devices.
// This is independent of any active connection. B68: excludes GPS devices.
func scanMeshtasticPorts(ctx context.Context) []MeshtasticDeviceInfo {
	var devices []MeshtasticDeviceInfo

	candidates := findSerialCandidates()
	for _, port := range candidates {
		if err := validateSerialPort(port); err != nil {
			continue
		}

		// B68: Skip GPS devices — they show up as ttyACM/ttyUSB but are not radios
		if isExcludedFromRadioScan(port) {
			log.Printf("meshtastic: scan skipping %s (GPS/excluded device)", port)
			continue
		}

		info := MeshtasticDeviceInfo{
			Port:          port,
			TransportType: "serial",
		}

		// B68: Use findUSBVIDPID() with proper symlink resolution to get VID:PID.
		// Previously used filepath.Join(sysPath, "../idVendor") which failed on
		// CDC-ACM symlinks — same bug as findUSBVIDPID() had.
		vidpid := findUSBVIDPID(port)
		if vidpid != "" {
			parts := strings.SplitN(vidpid, ":", 2)
			if len(parts) == 2 {
				info.VID = parts[0]
				info.PID = parts[1]
			}
		}

		// Check if VID:PID matches known Meshtastic devices
		if isMeshtasticVIDPID(port) {
			info.Responding = true
			info.Description = meshtasticDeviceName(info.VID, info.PID)
		} else {
			info.Description = "Unknown USB serial device"
		}

		devices = append(devices, info)
	}

	return devices
}

// meshtasticDeviceName returns a human-readable name for known VID:PID pairs.
func meshtasticDeviceName(vid, pid string) string {
	vidpid := fmt.Sprintf("%s:%s", vid, pid)
	names := map[string]string{
		"303a:1001": "ESP32-S3 (Heltec V3 / T-Beam S3)",
		"1a86:55d4": "CH343 (T-Beam / Heltec V2)",
		"1a86:7523": "CH340 (generic Meshtastic)",
		"10c4:ea60": "CP2102 (generic Meshtastic)",
		"239a:8029": "RAK WisBlock (nRF52840)",
		"1915:520f": "Nordic nRF52840 (RAK / T-Echo)",
	}
	if name, ok := names[vidpid]; ok {
		return name
	}
	return "Meshtastic-compatible device"
}

// ============================================================================
// Framing Helpers
// ============================================================================

// findStartMarker finds the index of the 0x94 0xC3 start marker in data.
// Returns -1 if not found.
func findStartMarker(data []byte) int {
	for i := 0; i < len(data)-1; i++ {
		if data[i] == meshStart1 && data[i+1] == meshStart2 {
			return i
		}
	}
	return -1
}
