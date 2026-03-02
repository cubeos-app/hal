package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Iridium Driver — Serial AT Command Engine
// ============================================================================

// IridiumDriver manages a persistent serial connection to a RockBLOCK 9603
// Iridium satellite modem. All AT command access is serialized via mutex.
//
// The driver handles:
//   - Auto-detection of the modem on /dev/ttyUSB* ports
//   - Connection initialization (AT&K0, IMEI retrieval)
//   - AT command send/receive with timeout
//   - Binary SBD write (AT+SBDWB) with checksum
//   - Binary SBD read (AT+SBDRB) with checksum verification
//   - SBDIX session management
//   - Ring alert monitoring (SBDRING) for SSE
//   - Reconnection with backoff on disconnect
type IridiumDriver struct {
	mu sync.Mutex

	// Connection state
	port      string // e.g., "/dev/ttyUSB1"
	file      *os.File
	connected bool
	imei      string
	model     string

	// Ring alert monitoring
	ringCh      chan struct{} // signals when SBDRING is detected
	stopRingCh  chan struct{} // stops the ring monitor goroutine
	ringActive  bool
	monitorDone chan struct{} // closed when monitor goroutine exits

	// SBDIX rate limiting — Iridium spec requires min 10s between attempts
	lastSBDIX time.Time

	// Auto-reconnect
	lastPort         string // last successfully connected port (for reconnect)
	reconnecting     bool
	stopReconnect    chan struct{}
	reconnectBackoff time.Duration

	// SSE subscribers for events
	eventMu      sync.RWMutex
	eventClients map[uint64]chan IridiumEvent
	nextClientID uint64

	// Configuration
	baudRate     int
	readTimeout  time.Duration
	sbdixTimeout time.Duration
}

// IridiumEvent represents an event emitted on the SSE stream.
type IridiumEvent struct {
	Type    string `json:"type"`    // "ring_alert", "status_change", "connected", "disconnected"
	Message string `json:"message"` // Human-readable description
	Time    string `json:"time"`    // RFC3339 timestamp
}

// SBDIXResult holds the parsed response from an AT+SBDIX session.
type SBDIXResult struct {
	MOStatus int `json:"mo_status"` // 0-4 = success, others = failure
	MOMSN    int `json:"momsn"`     // MO message sequence number
	MTStatus int `json:"mt_status"` // 0 = no MT, 1 = MT received, 2 = error
	MTMSN    int `json:"mtmsn"`     // MT message sequence number
	MTLength int `json:"mt_length"` // byte length of MT message
	MTQueued int `json:"mt_queued"` // MT messages still queued at GSS
}

// MOSuccess returns true if the MO message was sent successfully.
// Per Iridium ISU spec: 0=success, 1=success (MT too big), 2=success
// (location rejected), 3=success (queued at ISU), 4=success (queued at gateway).
func (r SBDIXResult) MOSuccess() bool {
	return r.MOStatus >= 0 && r.MOStatus <= 4
}

// NewIridiumDriver creates a new Iridium driver instance.
func NewIridiumDriver() *IridiumDriver {
	sbdixTimeout := 90 * time.Second
	if envTimeout := os.Getenv("IRIDIUM_SBDIX_TIMEOUT"); envTimeout != "" {
		if parsed, err := time.ParseDuration(envTimeout); err == nil && parsed >= 10*time.Second && parsed <= 300*time.Second {
			sbdixTimeout = parsed
			log.Printf("iridium: SBDIX timeout set to %v from env", sbdixTimeout)
		} else {
			log.Printf("iridium: invalid IRIDIUM_SBDIX_TIMEOUT %q (must be 10s-300s duration), using default 90s", envTimeout)
		}
	}

	return &IridiumDriver{
		baudRate:     19200,
		readTimeout:  3 * time.Second,
		sbdixTimeout: sbdixTimeout,
		ringCh:       make(chan struct{}, 1),
		stopRingCh:   make(chan struct{}),
		eventClients: make(map[uint64]chan IridiumEvent),
	}
}

// ============================================================================
// Connection Management
// ============================================================================

// Connect opens the serial port and initializes the modem.
// If port is empty, auto-detection is attempted.
func (d *IridiumDriver) Connect(ctx context.Context, port string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Close existing connection if any
	d.closeLocked()

	if port == "" {
		var err error
		port, err = d.autoDetect(ctx)
		if err != nil {
			return fmt.Errorf("auto-detect failed: %w", err)
		}
	}

	// Validate port path
	if err := validateSerialPort(port); err != nil {
		return err
	}

	// Configure serial port via stty (19200 baud, 8N1, raw mode)
	if _, err := execWithTimeout(ctx, "stty", "-F", port,
		strconv.Itoa(d.baudRate), "raw", "-echo", "-echoe", "-echok",
		"cs8", "-cstopb", "-parenb", "-crtscts"); err != nil {
		return fmt.Errorf("stty configuration failed: %w", err)
	}

	// Open port
	f, err := os.OpenFile(port, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", port, err)
	}

	d.file = f
	d.port = port

	// Flush any pending data
	d.flushLocked()

	// CRITICAL: Disable flow control first (3-wire serial)
	resp, err := d.sendATLocked(ctx, "AT&K0", 2*time.Second)
	if err != nil {
		d.closeLocked()
		return fmt.Errorf("AT&K0 failed (is this an Iridium modem?): %w", err)
	}
	if !strings.Contains(resp, "OK") {
		d.closeLocked()
		return fmt.Errorf("AT&K0 did not return OK: %s", resp)
	}

	// Ignore DTR line changes — Linux FTDI driver momentarily pulses
	// DTR on port open, which with the default AT&D2 would reset the modem.
	if resp, err := d.sendATLocked(ctx, "AT&D0", 2*time.Second); err != nil || !strings.Contains(resp, "OK") {
		log.Printf("iridium: AT&D0 warning (non-fatal): err=%v resp=%q", err, resp)
	}

	// Basic connectivity check
	resp, err = d.sendATLocked(ctx, "AT", 2*time.Second)
	if err != nil || !strings.Contains(resp, "OK") {
		d.closeLocked()
		return fmt.Errorf("modem not responding to AT")
	}

	d.connected = true

	// Get IMEI
	resp, err = d.sendATLocked(ctx, "AT+CGSN", 2*time.Second)
	if err == nil {
		d.imei = parseIMEI(resp)
	}

	// Get model
	resp, err = d.sendATLocked(ctx, "AT+CGMM", 2*time.Second)
	if err == nil {
		d.model = parseModelResponse(resp)
	}

	log.Printf("iridium: connected to %s (IMEI: %s, Model: %s)", port, d.imei, d.model)
	d.lastPort = port

	// Enable ring alerts so the monitor goroutine sees SBDRING for MT messages.
	// Do this every connect — ATZ or power-cycle can reset SBDMTA to 0.
	if resp, err := d.sendATLocked(ctx, "AT+SBDMTA=1", 3*time.Second); err != nil || !strings.Contains(resp, "OK") {
		log.Printf("iridium: AT+SBDMTA=1 warning (non-fatal): err=%v resp=%q", err, resp)
	}

	d.emitEvent(IridiumEvent{
		Type:    "connected",
		Message: fmt.Sprintf("Connected to %s (IMEI: %s)", port, d.imei),
	})

	// Start ring alert monitor goroutine
	d.startMonitorLocked()

	return nil
}

// Disconnect closes the serial connection and stops monitoring.
// This is an explicit user action — stops reconnect and clears lastPort.
func (d *IridiumDriver) Disconnect() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopReconnectLocked() // Stop auto-reconnect first
	d.closeLocked()
	d.lastPort = "" // Explicit disconnect — don't auto-reconnect
	d.emitEvent(IridiumEvent{
		Type:    "disconnected",
		Message: "Modem disconnected",
	})
}

// IsConnected returns whether the modem is currently connected.
func (d *IridiumDriver) IsConnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// Port returns the currently connected port.
func (d *IridiumDriver) Port() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.port
}

// IMEI returns the modem's IMEI.
func (d *IridiumDriver) IMEI() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.imei
}

// Model returns the modem's model string.
func (d *IridiumDriver) Model() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model
}

// closeLocked closes the connection without acquiring the mutex.
// Caller must hold d.mu.
// NOTE: Does NOT stop reconnect — only Disconnect() should do that,
// because reconnectLoop calls Connect which calls closeLocked.
func (d *IridiumDriver) closeLocked() {
	// Stop ring alert monitor
	d.stopMonitorLocked()

	if d.file != nil {
		d.file.Close()
		d.file = nil
	}
	d.connected = false
	d.imei = ""
	d.model = ""
	d.port = ""
}

// flushLocked discards any pending data in the serial buffer.
// Uses SetDeadline to avoid leaking goroutines that steal subsequent AT responses.
func (d *IridiumDriver) flushLocked() {
	if d.file == nil {
		return
	}
	buf := make([]byte, 4096)
	d.file.SetDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		n, err := d.file.Read(buf)
		if n == 0 || err != nil {
			break
		}
	}
	d.file.SetDeadline(time.Time{}) // Clear deadline for subsequent operations
}

// ============================================================================
// Auto-Detection
// ============================================================================

// autoDetect scans /dev/ttyUSB* for an Iridium modem by sending AT and checking for OK.
// B68: Excludes GPS devices and gpsd-claimed ports.
func (d *IridiumDriver) autoDetect(ctx context.Context) (string, error) {
	// Find candidate ports
	matches, _ := filepath.Glob("/dev/ttyUSB*")

	for _, port := range matches {
		// B68: Skip GPS devices — uses shared filter from meshtastic_serial.go
		if isExcludedFromRadioScan(port) {
			log.Printf("iridium: auto-detect skipping %s (GPS/excluded device)", port)
			continue
		}

		// Configure port
		if _, err := execWithTimeout(ctx, "stty", "-F", port,
			strconv.Itoa(d.baudRate), "raw", "-echo", "-crtscts"); err != nil {
			continue
		}

		// Try opening and sending AT
		f, err := os.OpenFile(port, os.O_RDWR, 0)
		if err != nil {
			continue
		}

		// Write AT\r
		if _, err := f.WriteString("AT\r"); err != nil {
			f.Close()
			continue
		}

		// Read response with timeout
		resp := readWithTimeout(f, 2*time.Second)
		f.Close()

		if strings.Contains(resp, "OK") {
			log.Printf("iridium: auto-detected modem on %s", port)
			return port, nil
		}
	}

	return "", fmt.Errorf("no Iridium modem found on any /dev/ttyUSB* port")
}

// ============================================================================
// AT Command Engine
// ============================================================================

// SendAT sends an AT command and returns the response. Thread-safe.
func (d *IridiumDriver) SendAT(ctx context.Context, command string, timeout time.Duration) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sendATLocked(ctx, command, timeout)
}

// sendATLocked sends an AT command. Caller must hold d.mu.
// Uses SetDeadline for draining to avoid leaking goroutines that would
// race with readResponseLocked and steal the AT response.
func (d *IridiumDriver) sendATLocked(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if d.file == nil {
		return "", fmt.Errorf("not connected")
	}

	// Drain any pending data before sending (deadline-based, no goroutine leak)
	d.file.SetDeadline(time.Now().Add(100 * time.Millisecond))
	drainBuf := make([]byte, 1024)
	for {
		n, err := d.file.Read(drainBuf)
		if n == 0 || err != nil {
			break
		}
	}
	d.file.SetDeadline(time.Time{}) // Clear deadline

	// Send command with CR (NO LF — Iridium protocol)
	if _, err := d.file.WriteString(command + "\r"); err != nil {
		return "", fmt.Errorf("write failed: %w", err)
	}

	// Read response until OK, ERROR, or timeout
	return d.readResponseLocked(ctx, timeout)
}

// readResponseLocked reads the serial port until "OK" or "ERROR" is found,
// or until the timeout expires. Caller must hold d.mu.
//
// Uses SetDeadline-based polling (no goroutine) so the mutex is held throughout.
// This prevents monitorLoop from racing on d.file and stealing bytes from
// AT responses, which would cause parsers (e.g. parseCSQ) to silently return 0.
func (d *IridiumDriver) readResponseLocked(ctx context.Context, timeout time.Duration) (string, error) {
	if d.file == nil {
		return "", fmt.Errorf("not connected")
	}

	deadline := time.Now().Add(timeout)
	var resp strings.Builder
	buf := make([]byte, 256)

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("read timeout")
		}

		// Use short read slices so ctx cancellation is checked regularly.
		// SetDeadline applies to the fd globally — monitorLoop is blocked on
		// d.mu so it cannot race here, but the deadline also prevents hanging
		// if the modem goes silent mid-response.
		sliceEnd := time.Now().Add(50 * time.Millisecond)
		if sliceEnd.After(deadline) {
			sliceEnd = deadline
		}
		d.file.SetDeadline(sliceEnd)
		n, err := d.file.Read(buf)
		d.file.SetDeadline(time.Time{}) // clear immediately after each read

		if n > 0 {
			resp.WriteString(string(buf[:n]))
			full := resp.String()

			// Check for terminal responses
			if strings.Contains(full, "\r\nOK\r\n") ||
				strings.HasSuffix(strings.TrimSpace(full), "OK") ||
				strings.Contains(full, "\r\nERROR\r\n") ||
				strings.HasSuffix(strings.TrimSpace(full), "ERROR") ||
				strings.Contains(full, "READY") {
				return full, nil
			}
		}

		if err != nil && !isTimeoutError(err) {
			return resp.String(), err
		}
	}
}

// ============================================================================
// Signal Quality
// ============================================================================

// SignalDescriptions maps CSQ values to human descriptions.
var SignalDescriptions = map[int]string{
	0: "No signal",
	1: "Poor (~-110 dBm, minimum for TX)",
	2: "Fair (~-108 dBm)",
	3: "Good (~-106 dBm)",
	4: "Very good (~-104 dBm)",
	5: "Excellent (~-102 dBm)",
}

// GetSignal queries the modem signal strength (AT+CSQ).
// Per Iridium 9600 family spec, AT+CSQ may block up to 50s during system
// acquisition — use a 60s timeout to allow full acquisition to complete.
func (d *IridiumDriver) GetSignal(ctx context.Context) (int, string, error) {
	resp, err := d.SendAT(ctx, "AT+CSQ", 60*time.Second)
	if err != nil {
		return 0, "No signal", err
	}

	strength := parseCSQ(resp)
	desc, ok := SignalDescriptions[strength]
	if !ok {
		desc = "Unknown"
	}
	return strength, desc, nil
}

// GetSignalFast returns the last known signal strength (AT+CSQF).
// Returns immediately with a cached value (may be up to 15s old).
func (d *IridiumDriver) GetSignalFast(ctx context.Context) (int, string, error) {
	resp, err := d.SendAT(ctx, "AT+CSQF", 5*time.Second)
	if err != nil {
		return 0, "No signal", err
	}

	strength := parseCSQ(resp)
	desc, ok := SignalDescriptions[strength]
	if !ok {
		desc = "Unknown"
	}
	return strength, desc, nil
}

// ============================================================================
// SBD Status
// ============================================================================

// SBDStatus holds the result of AT+SBDSX.
type SBDStatus struct {
	MOFlag    bool `json:"mo_flag"`
	MOMSN     int  `json:"momsn"`
	MTFlag    bool `json:"mt_flag"`
	MTMSN     int  `json:"mtmsn"`
	RAFlag    bool `json:"ra_flag"` // Ring Alert pending
	MTWaiting int  `json:"mt_waiting"`
}

// GetSBDStatus queries the modem SBD status.
func (d *IridiumDriver) GetSBDStatus(ctx context.Context) (SBDStatus, error) {
	resp, err := d.SendAT(ctx, "AT+SBDSX", 5*time.Second)
	if err != nil {
		return SBDStatus{}, err
	}
	return parseSBDSX(resp)
}

// ============================================================================
// Text SBD Send
// ============================================================================

// SendText sends a text SBD message via AT+SBDWT + AT+SBDIX.
// Max 120 characters (AT line length limit for SBDWT).
// Returns the SBDIX result.
func (d *IridiumDriver) SendText(ctx context.Context, message string) (SBDIXResult, error) {
	if len(message) > 120 {
		return SBDIXResult{}, fmt.Errorf("text message too long (max 120 chars for AT+SBDWT)")
	}
	if strings.ContainsAny(message, "\r\n\x00") {
		return SBDIXResult{}, fmt.Errorf("message contains invalid characters (CR/LF/null)")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.connected {
		return SBDIXResult{}, fmt.Errorf("not connected")
	}

	// Clear MO buffer
	resp, err := d.sendATLocked(ctx, "AT+SBDD0", 3*time.Second)
	if err != nil || strings.Contains(resp, "ERROR") {
		return SBDIXResult{}, fmt.Errorf("failed to clear MO buffer")
	}

	// Write text to MO buffer
	resp, err = d.sendATLocked(ctx, fmt.Sprintf("AT+SBDWT=%s", message), 5*time.Second)
	if err != nil || !strings.Contains(resp, "OK") {
		return SBDIXResult{}, fmt.Errorf("failed to write text to MO buffer")
	}

	// Initiate SBD session
	return d.sbdixLocked(ctx)
}

// ============================================================================
// Binary SBD Send (AT+SBDWB)
// ============================================================================

// SendBinary sends a binary SBD message via AT+SBDWB + AT+SBDIX.
// Max 340 bytes.
// Returns the SBDIX result.
func (d *IridiumDriver) SendBinary(ctx context.Context, data []byte) (SBDIXResult, error) {
	if len(data) == 0 {
		return SBDIXResult{}, fmt.Errorf("data is empty")
	}
	if len(data) > 340 {
		return SBDIXResult{}, fmt.Errorf("data too large (max 340 bytes for SBD)")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.connected {
		return SBDIXResult{}, fmt.Errorf("not connected")
	}

	// Clear MO buffer
	resp, err := d.sendATLocked(ctx, "AT+SBDD0", 3*time.Second)
	if err != nil || strings.Contains(resp, "ERROR") {
		return SBDIXResult{}, fmt.Errorf("failed to clear MO buffer")
	}

	// Initiate binary write
	resp, err = d.sendATLocked(ctx, fmt.Sprintf("AT+SBDWB=%d", len(data)), 5*time.Second)
	if err != nil {
		return SBDIXResult{}, fmt.Errorf("AT+SBDWB command failed: %w", err)
	}
	if !strings.Contains(resp, "READY") {
		return SBDIXResult{}, fmt.Errorf("modem did not respond READY for binary write")
	}

	// Calculate checksum: sum of all data bytes as uint16 big-endian
	var checksum uint16
	for _, b := range data {
		checksum += uint16(b)
	}

	// Build payload: data + 2-byte checksum (big-endian)
	var payload bytes.Buffer
	payload.Write(data)
	if err := binary.Write(&payload, binary.BigEndian, checksum); err != nil {
		return SBDIXResult{}, fmt.Errorf("checksum encode failed: %w", err)
	}

	// Write binary payload directly to serial
	if d.file == nil {
		return SBDIXResult{}, fmt.Errorf("not connected")
	}
	if _, err := d.file.Write(payload.Bytes()); err != nil {
		return SBDIXResult{}, fmt.Errorf("binary write failed: %w", err)
	}

	// Read result: 0 = success, 1 = timeout, 2 = bad checksum, 3 = wrong size
	writeResp, err := d.readResponseLocked(ctx, 5*time.Second)
	if err != nil {
		return SBDIXResult{}, fmt.Errorf("binary write response failed: %w", err)
	}

	writeResult := strings.TrimSpace(writeResp)
	// Extract the status digit — could be "0\r\n\r\nOK\r\n" etc.
	for _, line := range strings.Split(writeResult, "\n") {
		line = strings.TrimSpace(line)
		if line == "0" {
			break // success
		}
		if line == "1" {
			return SBDIXResult{}, fmt.Errorf("binary write timeout on modem")
		}
		if line == "2" {
			return SBDIXResult{}, fmt.Errorf("binary write checksum mismatch")
		}
		if line == "3" {
			return SBDIXResult{}, fmt.Errorf("binary write wrong size")
		}
	}

	// Initiate SBD session
	return d.sbdixLocked(ctx)
}

// ============================================================================
// Binary SBD Read (AT+SBDRB)
// ============================================================================

// ReadBinaryMT reads the MT buffer in binary mode.
// Returns raw bytes and the verified checksum status.
func (d *IridiumDriver) ReadBinaryMT(ctx context.Context) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.connected || d.file == nil {
		return nil, fmt.Errorf("not connected")
	}

	// Send the command
	if _, err := d.file.WriteString("AT+SBDRB\r"); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}

	// Read response: 2-byte length (big-endian) + data + 2-byte checksum
	// First, read past any echo/header
	headerBuf := make([]byte, 512)
	deadline := time.Now().Add(5 * time.Second)

	var rawBytes []byte
	readDone := make(chan error, 1)

	go func() {
		var all []byte
		for time.Now().Before(deadline) {
			n, err := d.file.Read(headerBuf)
			if n > 0 {
				all = append(all, headerBuf[:n]...)
				// We need at least 4 bytes (2 length + at least 0 data + 2 checksum)
				// to be able to parse. Look for the binary header after any AT echo.
				if len(all) >= 4 {
					// Find the binary payload start — after the AT echo line
					// The response may include "AT+SBDRB\r\n" echo followed by raw binary
					rawBytes = all
					readDone <- nil
					return
				}
			}
			if err != nil {
				rawBytes = all
				readDone <- err
				return
			}
		}
		rawBytes = all
		readDone <- fmt.Errorf("timeout reading binary MT")
	}()

	select {
	case err := <-readDone:
		if err != nil && len(rawBytes) < 4 {
			return nil, fmt.Errorf("binary read failed: %w", err)
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(6 * time.Second):
		return nil, fmt.Errorf("binary read timeout")
	}

	// Strip AT echo if present — find the first non-ASCII portion
	// or parse from the binary start
	data, err := parseSBDRBResponse(rawBytes)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// ReadTextMT reads the MT buffer in text mode (AT+SBDRT).
func (d *IridiumDriver) ReadTextMT(ctx context.Context) (string, error) {
	resp, err := d.SendAT(ctx, "AT+SBDRT", 5*time.Second)
	if err != nil {
		return "", err
	}

	return parseSBDRT(resp), nil
}

// ============================================================================
// SBDIX Session
// ============================================================================

// PerformSBDIX initiates an SBD session (send MO + check MT). Thread-safe.
func (d *IridiumDriver) PerformSBDIX(ctx context.Context) (SBDIXResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sbdixLocked(ctx)
}

// sbdixLocked performs AT+SBDIX. Caller must hold d.mu.
func (d *IridiumDriver) sbdixLocked(ctx context.Context) (SBDIXResult, error) {
	if !d.connected || d.file == nil {
		return SBDIXResult{}, fmt.Errorf("not connected")
	}

	// Iridium spec requires minimum 10 seconds between SBDIX attempts.
	// Rapid calls can get the modem temporarily blocked by the gateway.
	const minSBDIXInterval = 10 * time.Second
	if elapsed := time.Since(d.lastSBDIX); elapsed < minSBDIXInterval {
		wait := minSBDIXInterval - elapsed
		log.Printf("iridium: SBDIX rate limit — waiting %v", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return SBDIXResult{}, ctx.Err()
		case <-time.After(wait):
		}
	}

	d.lastSBDIX = time.Now()
	resp, err := d.sendATLocked(ctx, "AT+SBDIX", d.sbdixTimeout)
	if err != nil {
		return SBDIXResult{}, fmt.Errorf("SBDIX failed: %w", err)
	}

	return parseSBDIX(resp)
}

// MailboxCheck performs an SBDIX without an MO message to check for MT.
func (d *IridiumDriver) MailboxCheck(ctx context.Context) (SBDIXResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.connected {
		return SBDIXResult{}, fmt.Errorf("not connected")
	}

	// Clear MO buffer first so we don't accidentally resend
	resp, err := d.sendATLocked(ctx, "AT+SBDD0", 3*time.Second)
	if err != nil || strings.Contains(resp, "ERROR") {
		return SBDIXResult{}, fmt.Errorf("failed to clear MO buffer")
	}

	// Perform SBDIX (will check for MT without sending)
	return d.sbdixLocked(ctx)
}

// ============================================================================
// Buffer Management
// ============================================================================

// ClearBuffers clears MO, MT, or both buffers.
// buffer: "mo" (0), "mt" (1), "both" (2)
func (d *IridiumDriver) ClearBuffers(ctx context.Context, buffer string) error {
	var cmd string
	switch strings.ToLower(buffer) {
	case "mo":
		cmd = "AT+SBDD0"
	case "mt":
		cmd = "AT+SBDD1"
	case "both":
		cmd = "AT+SBDD2"
	default:
		return fmt.Errorf("invalid buffer (must be mo, mt, or both)")
	}

	resp, err := d.SendAT(ctx, cmd, 3*time.Second)
	if err != nil {
		return fmt.Errorf("clear buffer failed: %w", err)
	}
	// AT+SBDD returns "0\r\n" on success
	if strings.Contains(resp, "ERROR") {
		return fmt.Errorf("modem returned ERROR")
	}
	return nil
}

// ============================================================================
// Ring Alert Monitoring
// ============================================================================

// EnableRingAlerts enables ring alert notifications on the modem (AT+SBDMTA=1).
func (d *IridiumDriver) EnableRingAlerts(ctx context.Context) error {
	resp, err := d.SendAT(ctx, "AT+SBDMTA=1", 3*time.Second)
	if err != nil || !strings.Contains(resp, "OK") {
		return fmt.Errorf("failed to enable ring alerts")
	}
	return nil
}

// DisableRingAlerts disables ring alert notifications (AT+SBDMTA=0).
func (d *IridiumDriver) DisableRingAlerts(ctx context.Context) error {
	resp, err := d.SendAT(ctx, "AT+SBDMTA=0", 3*time.Second)
	if err != nil || !strings.Contains(resp, "OK") {
		return fmt.Errorf("failed to disable ring alerts")
	}
	return nil
}

// startMonitorLocked starts the ring alert monitor goroutine.
// Caller must hold d.mu.
func (d *IridiumDriver) startMonitorLocked() {
	if d.ringActive {
		return
	}
	d.stopRingCh = make(chan struct{})
	d.monitorDone = make(chan struct{})
	d.ringActive = true
	go d.monitorLoop()
	log.Printf("iridium: ring alert monitor started")
}

// stopMonitorLocked stops the ring alert monitor goroutine.
// Caller must hold d.mu.
func (d *IridiumDriver) stopMonitorLocked() {
	if !d.ringActive {
		return
	}
	d.ringActive = false

	// Check if monitor already exited (e.g., serial error → reconnectLoop)
	select {
	case <-d.monitorDone:
		// Monitor already exited — just clean up state
	default:
		// Monitor is still running — signal it to stop and wait
		close(d.stopRingCh)
		done := d.monitorDone
		d.mu.Unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("iridium: monitor goroutine did not exit in time")
		}
		d.mu.Lock()
	}
	log.Printf("iridium: ring alert monitor stopped")
}

// stopReconnectLocked stops any auto-reconnect goroutine.
// Caller must hold d.mu.
func (d *IridiumDriver) stopReconnectLocked() {
	if d.reconnecting {
		close(d.stopReconnect)
		d.reconnecting = false
	}
}

// monitorLoop continuously reads the serial port for unsolicited SBDRING alerts.
// It acquires/releases the mutex for each read cycle to allow SendAT() fair access.
// On serial error, it emits a disconnected event and starts auto-reconnect.
func (d *IridiumDriver) monitorLoop() {
	defer close(d.monitorDone)

	buf := make([]byte, 1)
	var line []byte

	for {
		// Check for stop signal
		select {
		case <-d.stopRingCh:
			return
		default:
		}

		d.mu.Lock()
		if d.file == nil {
			d.mu.Unlock()
			return
		}

		// Set short read deadline so we release the lock frequently
		f := d.file
		d.mu.Unlock()

		// Read outside the lock using the file handle — the 100ms deadline
		// means we yield quickly. We still hold no lock during the blocking read.
		// NOTE: SetDeadline on os.File works on Linux /dev/tty* via poll.
		// For serial files that don't support SetDeadline, Read will block
		// until data arrives or the file is closed. The stopRingCh check above
		// and file closure in closeLocked() ensure we exit.
		f.SetDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := f.Read(buf)

		if err != nil {
			if isTimeoutError(err) {
				continue // Normal — no data available
			}
			// Check if we were told to stop
			select {
			case <-d.stopRingCh:
				return
			default:
			}
			// Real serial error — modem disconnected
			log.Printf("iridium: monitor detected serial error: %v", err)
			d.emitEvent(IridiumEvent{
				Type:    "disconnected",
				Message: "Serial connection lost",
			})
			// Start auto-reconnect
			d.mu.Lock()
			d.connected = false
			if d.file != nil {
				d.file.Close()
				d.file = nil
			}
			lastPort := d.lastPort
			d.mu.Unlock()
			go d.reconnectLoop(lastPort)
			return
		}

		if n > 0 {
			line = append(line, buf[0])
			if buf[0] == '\n' {
				s := strings.TrimSpace(string(line))
				if s == "SBDRING" {
					log.Printf("iridium: SBDRING received — MT message waiting")
					d.emitEvent(IridiumEvent{
						Type:    "ring_alert",
						Message: "MT message waiting at gateway",
					})
					// Signal on ringCh (non-blocking)
					select {
					case d.ringCh <- struct{}{}:
					default:
					}
				} else if s != "" {
					// Log unexpected unsolicited output for debugging
					log.Printf("iridium: unsolicited output: %q", s)
				}
				line = line[:0]
			}
			// Prevent unbounded accumulation
			if len(line) > 256 {
				line = line[:0]
			}
		}
	}
}

// isTimeoutError checks if an error is a timeout (net or os deadline).
func isTimeoutError(err error) bool {
	if os.IsTimeout(err) {
		return true
	}
	// Also check for the interface method
	type timeouter interface {
		Timeout() bool
	}
	if te, ok := err.(timeouter); ok {
		return te.Timeout()
	}
	return false
}

// reconnectLoop attempts to reconnect with exponential backoff.
// It runs as a separate goroutine started by monitorLoop on disconnect.
func (d *IridiumDriver) reconnectLoop(lastPort string) {
	d.mu.Lock()
	if d.reconnecting {
		d.mu.Unlock()
		return
	}
	d.reconnecting = true
	d.stopReconnect = make(chan struct{})
	d.mu.Unlock()

	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	log.Printf("iridium: starting auto-reconnect to %s", lastPort)
	d.emitEvent(IridiumEvent{
		Type:    "reconnecting",
		Message: fmt.Sprintf("Attempting to reconnect to %s", lastPort),
	})

	for {
		select {
		case <-d.stopReconnect:
			log.Printf("iridium: reconnect cancelled")
			return
		case <-time.After(backoff):
		}

		// Try to reconnect
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := d.Connect(ctx, lastPort)
		cancel()

		if err == nil {
			log.Printf("iridium: reconnected successfully to %s", lastPort)
			d.emitEvent(IridiumEvent{
				Type:    "reconnected",
				Message: fmt.Sprintf("Reconnected to %s", lastPort),
			})
			d.mu.Lock()
			d.reconnecting = false
			d.mu.Unlock()
			return
		}

		log.Printf("iridium: reconnect failed (%v), retrying in %v", err, backoff)

		// Exponential backoff with cap
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		select {
		case <-d.stopReconnect:
			log.Printf("iridium: reconnect cancelled")
			return
		default:
		}
	}
}

// ============================================================================
// SSE Event System
// ============================================================================

// SubscribeEvents registers a new SSE client and returns a channel + unsubscribe func.
func (d *IridiumDriver) SubscribeEvents() (<-chan IridiumEvent, func()) {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()

	id := d.nextClientID
	d.nextClientID++

	ch := make(chan IridiumEvent, 16)
	d.eventClients[id] = ch

	unsubscribe := func() {
		d.eventMu.Lock()
		defer d.eventMu.Unlock()
		delete(d.eventClients, id)
		close(ch)
	}

	return ch, unsubscribe
}

// emitEvent sends an event to all subscribed SSE clients.
func (d *IridiumDriver) emitEvent(event IridiumEvent) {
	event.Time = time.Now().UTC().Format(time.RFC3339)

	d.eventMu.RLock()
	defer d.eventMu.RUnlock()

	for _, ch := range d.eventClients {
		select {
		case ch <- event:
		default:
			// Client is slow; drop the event rather than blocking
		}
	}
}

// ============================================================================
// Device Scanning (stateless — does not use persistent connection)
// ============================================================================

// ScanDevices scans /dev/ttyUSB* for Iridium modems without maintaining a persistent connection.
// B68: Excludes GPS devices and gpsd-claimed ports.
func (d *IridiumDriver) ScanDevices(ctx context.Context) []IridiumDeviceInfo {
	var devices []IridiumDeviceInfo

	matches, _ := filepath.Glob("/dev/ttyUSB*")

	for _, port := range matches {
		if _, err := os.Stat(port); err != nil {
			continue
		}

		// B68: Skip GPS devices — uses shared filter from meshtastic_serial.go
		if isExcludedFromRadioScan(port) {
			log.Printf("iridium: scan skipping %s (GPS/excluded device)", port)
			continue
		}

		// Configure port
		if _, err := execWithTimeout(ctx, "stty", "-F", port,
			strconv.Itoa(d.baudRate), "raw", "-echo", "-crtscts"); err != nil {
			continue
		}

		f, err := os.OpenFile(port, os.O_RDWR, 0)
		if err != nil {
			continue
		}

		// Disable flow control
		f.WriteString("AT&K0\r")
		time.Sleep(200 * time.Millisecond)

		// Drain
		drain := make([]byte, 1024)
		go func() { f.Read(drain) }()
		time.Sleep(100 * time.Millisecond)

		// Send AT
		f.WriteString("AT\r")
		resp := readWithTimeout(f, 2*time.Second)

		if !strings.Contains(resp, "OK") {
			f.Close()
			continue
		}

		device := IridiumDeviceInfo{
			Port:      port,
			Name:      "Iridium Modem",
			Connected: true,
		}

		// Get IMEI
		f.WriteString("AT+CGSN\r")
		resp = readWithTimeout(f, 2*time.Second)
		device.IMEI = parseIMEI(resp)

		// Get model
		f.WriteString("AT+CGMM\r")
		resp = readWithTimeout(f, 2*time.Second)
		device.Model = parseModelResponse(resp)
		if strings.Contains(device.Model, "9603") {
			device.Name = "RockBLOCK 9603"
		}

		// Get signal
		f.WriteString("AT+CSQ\r")
		resp = readWithTimeout(f, 2*time.Second)
		device.Signal = parseCSQ(resp)

		f.Close()
		devices = append(devices, device)
	}

	return devices
}

// IridiumDeviceInfo represents a discovered Iridium modem.
type IridiumDeviceInfo struct {
	Port      string `json:"port"`
	Name      string `json:"name"`
	IMEI      string `json:"imei,omitempty"`
	Model     string `json:"model,omitempty"`
	Connected bool   `json:"connected"`
	Signal    int    `json:"signal"`
}

// ============================================================================
// Response Parsers
// ============================================================================

// parseIMEI extracts the IMEI from an AT+CGSN response.
func parseIMEI(resp string) string {
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 15 && isNumeric(line) && strings.HasPrefix(line, "3") {
			return line
		}
	}
	return ""
}

// parseModelResponse extracts the model from an AT+CGMM response.
func parseModelResponse(resp string) string {
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !strings.HasPrefix(line, "AT") {
			return line
		}
	}
	return ""
}

// parseCSQ extracts signal strength (0-5) from an AT+CSQ or AT+CSQF response.
func parseCSQ(resp string) int {
	// Try +CSQF: first (fast variant), then +CSQ:
	idx := strings.Index(resp, "+CSQF:")
	offset := 6
	if idx == -1 {
		idx = strings.Index(resp, "+CSQ:")
		offset = 5
	}
	if idx == -1 {
		return 0
	}
	remainder := strings.TrimSpace(resp[idx+offset:])
	sigStr := strings.Split(remainder, "\n")[0]
	sigStr = strings.TrimSpace(sigStr)
	sig, err := strconv.Atoi(sigStr)
	if err != nil {
		return 0
	}
	if sig < 0 || sig > 5 {
		return 0
	}
	return sig
}

// parseSBDIX parses an AT+SBDIX response.
// Format: +SBDIX: <MO_status>, <MOMSN>, <MT_status>, <MTMSN>, <MT_length>, <MT_queued>
func parseSBDIX(resp string) (SBDIXResult, error) {
	idx := strings.Index(resp, "+SBDIX:")
	if idx == -1 {
		return SBDIXResult{}, fmt.Errorf("no +SBDIX in response: %s", resp)
	}

	remainder := strings.TrimSpace(resp[idx+7:])
	// Take only the first line
	firstLine := strings.Split(remainder, "\n")[0]
	parts := strings.Split(firstLine, ",")
	if len(parts) < 6 {
		return SBDIXResult{}, fmt.Errorf("malformed SBDIX response (expected 6 fields, got %d)", len(parts))
	}

	result := SBDIXResult{}
	result.MOStatus, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
	result.MOMSN, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	result.MTStatus, _ = strconv.Atoi(strings.TrimSpace(parts[2]))
	result.MTMSN, _ = strconv.Atoi(strings.TrimSpace(parts[3]))
	result.MTLength, _ = strconv.Atoi(strings.TrimSpace(parts[4]))
	result.MTQueued, _ = strconv.Atoi(strings.TrimSpace(parts[5]))

	return result, nil
}

// parseSBDSX parses an AT+SBDSX response.
// Format: +SBDSX: MO flag, MOMSN, MT flag, MTMSN, RA flag, msg waiting
func parseSBDSX(resp string) (SBDStatus, error) {
	idx := strings.Index(resp, "+SBDSX:")
	if idx == -1 {
		return SBDStatus{}, fmt.Errorf("no +SBDSX in response")
	}

	remainder := strings.TrimSpace(resp[idx+7:])
	firstLine := strings.Split(remainder, "\n")[0]
	parts := strings.Split(firstLine, ",")
	if len(parts) < 6 {
		return SBDStatus{}, fmt.Errorf("malformed SBDSX response")
	}

	status := SBDStatus{}
	moFlag, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	status.MOFlag = moFlag != 0
	status.MOMSN, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	mtFlag, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
	status.MTFlag = mtFlag != 0
	status.MTMSN, _ = strconv.Atoi(strings.TrimSpace(parts[3]))
	raFlag, _ := strconv.Atoi(strings.TrimSpace(parts[4]))
	status.RAFlag = raFlag != 0
	status.MTWaiting, _ = strconv.Atoi(strings.TrimSpace(parts[5]))

	return status, nil
}

// parseSBDRT extracts the text message from an AT+SBDRT response.
func parseSBDRT(resp string) string {
	idx := strings.Index(resp, "+SBDRT:")
	if idx == -1 {
		return ""
	}
	remainder := resp[idx+7:]
	lines := strings.Split(remainder, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" {
			return line
		}
	}
	return ""
}

// parseSBDRBResponse parses the raw binary response from AT+SBDRB.
// The response contains: [AT echo...] [2-byte length] [data] [2-byte checksum]
func parseSBDRBResponse(raw []byte) ([]byte, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("response too short for binary read")
	}

	// Skip any AT echo text (ASCII chars until we hit the binary header)
	// The binary portion starts after any text preamble.
	// Look for a plausible length value — it should be < 270 (max MT).
	startIdx := 0
	for i := 0; i <= len(raw)-4; i++ {
		length := binary.BigEndian.Uint16(raw[i : i+2])
		if length <= 270 && i+2+int(length)+2 <= len(raw) {
			startIdx = i
			break
		}
	}

	if startIdx+4 > len(raw) {
		return nil, fmt.Errorf("cannot find binary payload in response")
	}

	length := binary.BigEndian.Uint16(raw[startIdx : startIdx+2])
	dataStart := startIdx + 2
	dataEnd := dataStart + int(length)

	if dataEnd+2 > len(raw) {
		return nil, fmt.Errorf("response truncated (expected %d data bytes + 2 checksum)", length)
	}

	data := raw[dataStart:dataEnd]
	receivedChecksum := binary.BigEndian.Uint16(raw[dataEnd : dataEnd+2])

	// Verify checksum
	var computed uint16
	for _, b := range data {
		computed += uint16(b)
	}
	if computed != receivedChecksum {
		return nil, fmt.Errorf("binary checksum mismatch (computed=%d, received=%d)", computed, receivedChecksum)
	}

	return data, nil
}

// ============================================================================
// Utilities
// ============================================================================

// readWithTimeout reads from a file with a simple timeout.
// Used for one-off serial reads during device scanning.
func readWithTimeout(f *os.File, timeout time.Duration) string {
	resultCh := make(chan string, 1)

	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 256)
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			n, err := f.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				s := buf.String()
				if strings.Contains(s, "OK") || strings.Contains(s, "ERROR") {
					resultCh <- s
					return
				}
			}
			if err != nil && err != io.EOF {
				resultCh <- buf.String()
				return
			}
		}
		resultCh <- buf.String()
	}()

	select {
	case result := <-resultCh:
		return result
	case <-time.After(timeout + 500*time.Millisecond):
		return ""
	}
}

// isNumeric returns true if all characters in s are digits.
func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
