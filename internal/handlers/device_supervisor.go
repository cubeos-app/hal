package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Device Supervisor — USB Inventory + Serial Reconciliation Engine
// ============================================================================

// DeviceSupervisor periodically scans USB devices and manages serial port
// identification, claiming, and driver reconnection.
//
// Two tiers:
//   - Tier 1 (30s): USB inventory via sysfs — all device classes
//   - Tier 2 (15s): Serial reconciliation — identify, claim, connect/reconnect
type DeviceSupervisor struct {
	registry   *DeviceRegistry
	meshtastic *MeshtasticDriver
	iridium    *IridiumDriver

	stopCh    chan struct{}
	scanNowCh chan struct{}

	// SSE subscribers for device events
	eventMu      sync.RWMutex
	eventClients map[uint64]chan DeviceEvent
	nextClientID uint64
}

// DeviceEvent is emitted via SSE when a device state changes.
type DeviceEvent struct {
	Type   string       `json:"type"`
	Device *DeviceEntry `json:"device"`
	Time   string       `json:"time"`
}

// NewDeviceSupervisor creates a new supervisor with references to serial drivers.
func NewDeviceSupervisor(meshtastic *MeshtasticDriver, iridium *IridiumDriver) *DeviceSupervisor {
	return &DeviceSupervisor{
		registry:     NewDeviceRegistry(),
		meshtastic:   meshtastic,
		iridium:      iridium,
		stopCh:       make(chan struct{}),
		scanNowCh:    make(chan struct{}, 1),
		eventClients: make(map[uint64]chan DeviceEvent),
	}
}

// Registry returns the underlying device registry (for handlers).
func (s *DeviceSupervisor) Registry() *DeviceRegistry {
	return s.registry
}

// Start launches the background scan goroutine.
func (s *DeviceSupervisor) Start() {
	go s.run()
	log.Printf("device-supervisor: started")
}

// Stop signals the supervisor to shut down.
func (s *DeviceSupervisor) Stop() {
	close(s.stopCh)
	log.Printf("device-supervisor: stopped")
}

// TriggerScan forces an immediate USB + serial scan (non-blocking).
func (s *DeviceSupervisor) TriggerScan() {
	select {
	case s.scanNowCh <- struct{}{}:
	default:
	}
}

// run is the main loop: periodic USB inventory + serial reconciliation.
func (s *DeviceSupervisor) run() {
	// Initial scan on startup
	s.scanUSBInventory()
	s.reconcileSerialDevices()

	usbTicker := time.NewTicker(30 * time.Second)
	serialTicker := time.NewTicker(15 * time.Second)
	defer usbTicker.Stop()
	defer serialTicker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-usbTicker.C:
			s.scanUSBInventory()
		case <-serialTicker.C:
			s.reconcileSerialDevices()
		case <-s.scanNowCh:
			s.scanUSBInventory()
			s.reconcileSerialDevices()
		}
	}
}

// ============================================================================
// Tier 1: USB Inventory (sysfs walk — all device classes)
// ============================================================================

func (s *DeviceSupervisor) scanUSBInventory() {
	const sysUSBPath = "/sys/bus/usb/devices"

	entries, err := os.ReadDir(sysUSBPath)
	if err != nil {
		log.Printf("device-supervisor: cannot read %s: %v", sysUSBPath, err)
		return
	}

	now := time.Now()
	currentPaths := make(map[string]bool)

	for _, e := range entries {
		name := e.Name()
		// Skip interface directories (contain ':')  and usb root hubs (start with "usb")
		if strings.Contains(name, ":") || strings.HasPrefix(name, "usb") {
			continue
		}

		devDir := filepath.Join(sysUSBPath, name)

		// Read VID:PID — if not present this is likely a root hub
		vid := readSysfsAttr(devDir, "idVendor")
		pid := readSysfsAttr(devDir, "idProduct")
		if vid == "" || pid == "" {
			continue
		}

		vidpid := fmt.Sprintf("%s:%s", vid, pid)

		// Read optional attributes
		vendor := readSysfsAttr(devDir, "manufacturer")
		product := readSysfsAttr(devDir, "product")
		serial := readSysfsAttr(devDir, "serial")
		busStr := readSysfsAttr(devDir, "busnum")
		devStr := readSysfsAttr(devDir, "devnum")

		busNum, _ := strconv.Atoi(busStr)
		devNum, _ := strconv.Atoi(devStr)

		// Classify by interface class
		class := classifyUSBDevice(devDir, vidpid)

		// Find associated /dev path
		devPath := findDevPath(devDir, class)

		entry := DeviceEntry{
			BusNum:    busNum,
			DevNum:    devNum,
			VIDPID:    vidpid,
			Vendor:    vendor,
			Product:   product,
			Serial:    serial,
			SysPath:   devDir,
			Class:     class,
			State:     StateDetected,
			DevPath:   devPath,
			FirstSeen: now,
			LastSeen:  now,
		}

		isNew := s.registry.Upsert(devDir, entry)
		currentPaths[devDir] = true

		if isNew {
			s.emitEvent(DeviceEvent{
				Type:   "device_added",
				Device: &entry,
			})
		}
	}

	// Reconcile — remove devices no longer in sysfs
	removed := s.registry.Reconcile(currentPaths)
	for _, entry := range removed {
		s.emitEvent(DeviceEvent{
			Type:   "device_removed",
			Device: entry,
		})
		log.Printf("device-supervisor: removed %s (%s %s)", entry.SysPath, entry.VIDPID, entry.Product)
	}
}

// classifyUSBDevice determines the DeviceClass from sysfs interface class codes and VID:PID.
func classifyUSBDevice(devDir, vidpid string) DeviceClass {
	// Check VID:PID first for known device families
	switch {
	case isKnownSDRVIDPID(vidpid):
		return ClassSDR
	}

	// Walk interface subdirectories for bInterfaceClass
	ifaceDirs, _ := filepath.Glob(filepath.Join(devDir, "*:*"))
	for _, ifaceDir := range ifaceDirs {
		classCode := readSysfsAttr(ifaceDir, "bInterfaceClass")
		switch classCode {
		case "02", "0a": // CDC, CDC-Data → serial
			return ClassSerial
		case "03": // HID
			return ClassHID
		case "06": // Still imaging
			return ClassCamera
		case "08": // Mass storage
			return ClassStorage
		case "09": // Hub
			return ClassHub
		case "0e": // Video (UVC)
			return ClassCamera
		case "e0": // Wireless (BT adapter)
			return ClassNetwork
		case "ff": // Vendor-specific: could be modem, serial adapter, SDR
			// Check if it creates a tty device (serial adapter / modem)
			if hasTTYChild(ifaceDir) {
				return ClassSerial
			}
			return ClassUnknown
		}
	}

	// Check if this device creates a network interface
	if hasNetChild(devDir) {
		return ClassNetwork
	}

	// Check for audio
	if hasSoundChild(devDir) {
		return ClassAudio
	}

	return ClassUnknown
}

// isKnownSDRVIDPID checks if a VID:PID belongs to a known SDR dongle.
func isKnownSDRVIDPID(vidpid string) bool {
	sdrDevices := map[string]bool{
		"0bda:2832": true, // RTL2832U
		"0bda:2838": true, // RTL2838UHIDIR
	}
	return sdrDevices[vidpid]
}

// findDevPath attempts to find the /dev/ path associated with a USB device.
func findDevPath(devDir string, class DeviceClass) string {
	switch class {
	case ClassSerial:
		return findTTYDevPath(devDir)
	case ClassCamera:
		return findVideoDevPath(devDir)
	case ClassStorage:
		return findBlockDevPath(devDir)
	case ClassNetwork:
		return findNetDevPath(devDir)
	default:
		return ""
	}
}

// findTTYDevPath walks the sysfs tree to find a tty device path.
func findTTYDevPath(devDir string) string {
	// Search for tty subdirectories at any depth
	matches, _ := filepath.Glob(filepath.Join(devDir, "*", "tty", "tty*"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(devDir, "*", "*", "tty", "tty*"))
	}
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(devDir, "*", "*", "*", "tty", "tty*"))
	}
	for _, m := range matches {
		name := filepath.Base(m)
		devPath := "/dev/" + name
		if _, err := os.Stat(devPath); err == nil {
			return devPath
		}
	}
	return ""
}

// findVideoDevPath finds a /dev/video* path for a camera device.
func findVideoDevPath(devDir string) string {
	matches, _ := filepath.Glob(filepath.Join(devDir, "*", "*", "video4linux", "video*"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(devDir, "*", "video4linux", "video*"))
	}
	for _, m := range matches {
		name := filepath.Base(m)
		devPath := "/dev/" + name
		if _, err := os.Stat(devPath); err == nil {
			return devPath
		}
	}
	return ""
}

// findBlockDevPath finds a /dev/sd* path for a storage device.
func findBlockDevPath(devDir string) string {
	matches, _ := filepath.Glob(filepath.Join(devDir, "*", "*", "block", "sd*"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(devDir, "*", "block", "sd*"))
	}
	for _, m := range matches {
		name := filepath.Base(m)
		devPath := "/dev/" + name
		if _, err := os.Stat(devPath); err == nil {
			return devPath
		}
	}
	return ""
}

// findNetDevPath finds a network interface name for a USB network adapter.
func findNetDevPath(devDir string) string {
	matches, _ := filepath.Glob(filepath.Join(devDir, "*", "net", "*"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(devDir, "*", "*", "net", "*"))
	}
	if len(matches) > 0 {
		return filepath.Base(matches[0])
	}
	return ""
}

// hasTTYChild checks if a sysfs directory has a tty child.
func hasTTYChild(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "tty*"))
	if len(matches) > 0 {
		return true
	}
	matches, _ = filepath.Glob(filepath.Join(dir, "tty", "tty*"))
	return len(matches) > 0
}

// hasNetChild checks if a sysfs device has a net child.
func hasNetChild(devDir string) bool {
	matches, _ := filepath.Glob(filepath.Join(devDir, "*", "net", "*"))
	if len(matches) > 0 {
		return true
	}
	matches, _ = filepath.Glob(filepath.Join(devDir, "*", "*", "net", "*"))
	return len(matches) > 0
}

// hasSoundChild checks if a sysfs device has a sound child.
func hasSoundChild(devDir string) bool {
	matches, _ := filepath.Glob(filepath.Join(devDir, "*", "sound", "card*"))
	if len(matches) > 0 {
		return true
	}
	matches, _ = filepath.Glob(filepath.Join(devDir, "*", "*", "sound", "card*"))
	return len(matches) > 0
}

// readSysfsAttr reads a single sysfs attribute file.
func readSysfsAttr(dir, attr string) string {
	data, err := os.ReadFile(filepath.Join(dir, attr))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ============================================================================
// Tier 2: Serial Port Reconciliation
// ============================================================================

func (s *DeviceSupervisor) reconcileSerialDevices() {
	// Scan current serial ports
	var activePorts []string
	for _, pattern := range []string{"/dev/ttyUSB*", "/dev/ttyACM*"} {
		matches, _ := filepath.Glob(pattern)
		activePorts = append(activePorts, matches...)
	}

	activeSet := make(map[string]bool, len(activePorts))
	for _, p := range activePorts {
		activeSet[p] = true
	}

	// Prune registry entries for vanished serial ports
	for _, entry := range s.registry.FindByClass(ClassSerial) {
		if entry.DevPath != "" && !activeSet[entry.DevPath] {
			// Port vanished — mark disconnected, release claim
			s.registry.SetState(entry.SysPath, StateDisconnected)
			s.registry.ReleaseSerialPort(entry.DevPath)

			if entry.Role == RoleMeshtastic && s.meshtastic.IsConnected() {
				s.meshtastic.Disconnect()
			}
			if entry.Role == RoleIridium && s.iridium.IsConnected() {
				s.iridium.Disconnect()
			}

			s.emitEvent(DeviceEvent{
				Type:   "device_disconnected",
				Device: entry,
			})
			log.Printf("device-supervisor: serial port %s vanished (%s)", entry.DevPath, entry.Role)
		}
	}

	// Identify and claim unclaimed serial ports
	for _, port := range activePorts {
		if err := validateSerialPort(port); err != nil {
			continue
		}

		// Already claimed?
		if role := s.registry.GetSerialPortRole(port); role != RoleNone {
			continue
		}

		s.identifyAndClaimPort(port)
	}

	// Reconnect disconnected serial devices
	s.reconnectDisconnected()
}

// identifyAndClaimPort runs the identification pipeline on an unclaimed serial port.
// Order: VID:PID → AT probe → NMEA probe (fast to slow).
func (s *DeviceSupervisor) identifyAndClaimPort(port string) {
	vidpid := FindUSBVIDPID(port)

	// Step 1: VID:PID match (~1ms)
	if vidpid != "" {
		if IsKnownMeshtasticVIDPID(vidpid) {
			if s.registry.ClaimSerialPort(port, RoleMeshtastic) {
				s.updateSerialDeviceRole(port, RoleMeshtastic, StateReady)
				log.Printf("device-supervisor: %s claimed as meshtastic (VID:PID=%s)", port, vidpid)
				// Auto-connect if not already connected
				if !s.meshtastic.IsConnected() {
					s.connectMeshtastic(port)
				}
			}
			return
		}
		if IsKnownGPSVIDPID(vidpid) {
			if s.registry.ClaimSerialPort(port, RoleGPS) {
				s.updateSerialDeviceRole(port, RoleGPS, StateReady)
				log.Printf("device-supervisor: %s claimed as GPS (VID:PID=%s)", port, vidpid)
			}
			return
		}
		if isKnownSDRVIDPID(vidpid) {
			return // Not a serial device we manage
		}
	}

	// Step 2: AT handshake probe (~500ms)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if ProbeATDevice(ctx, port) {
		if s.registry.ClaimSerialPort(port, RoleIridium) {
			s.updateSerialDeviceRole(port, RoleIridium, StateReady)
			log.Printf("device-supervisor: %s claimed as iridium (AT probe)", port)
			if !s.iridium.IsConnected() {
				s.connectIridium(port)
			}
		}
		return
	}

	// Step 3: NMEA probe (~2s)
	if ProbeForNMEA(port) {
		if s.registry.ClaimSerialPort(port, RoleGPS) {
			s.updateSerialDeviceRole(port, RoleGPS, StateReady)
			log.Printf("device-supervisor: %s claimed as GPS (NMEA probe)", port)
		}
		return
	}

	// Unknown — will retry next cycle
}

// updateSerialDeviceRole updates the registry entry for a serial device.
func (s *DeviceSupervisor) updateSerialDeviceRole(devPath string, role DeviceRole, state DeviceState) {
	entry := s.registry.FindByDevPath(devPath)
	if entry != nil {
		s.registry.SetRole(entry.SysPath, role)
		s.registry.SetState(entry.SysPath, state)
	}
}

// reconnectDisconnected checks for devices that were connected but lost connection.
func (s *DeviceSupervisor) reconnectDisconnected() {
	// Meshtastic: if driver is disconnected but a meshtastic port is claimed
	if !s.meshtastic.IsConnected() {
		if entry := s.registry.FindByRole(RoleMeshtastic); entry != nil && entry.State != StateConnected {
			if entry.DevPath != "" {
				if _, err := os.Stat(entry.DevPath); err == nil {
					s.connectMeshtastic(entry.DevPath)
				}
			}
		}
	}

	// Iridium: same pattern
	if !s.iridium.IsConnected() {
		if entry := s.registry.FindByRole(RoleIridium); entry != nil && entry.State != StateConnected {
			if entry.DevPath != "" {
				if _, err := os.Stat(entry.DevPath); err == nil {
					s.connectIridium(entry.DevPath)
				}
			}
		}
	}
}

func (s *DeviceSupervisor) connectMeshtastic(port string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	log.Printf("device-supervisor: connecting meshtastic on %s", port)
	if err := s.meshtastic.Connect(ctx, port); err != nil {
		log.Printf("device-supervisor: meshtastic connect failed on %s: %v", port, err)
		s.updateSerialDeviceRole(port, RoleMeshtastic, StateDisconnected)
		return
	}

	s.updateSerialDeviceRole(port, RoleMeshtastic, StateConnected)
	entry := s.registry.FindByDevPath(port)
	s.emitEvent(DeviceEvent{
		Type:   "device_connected",
		Device: entry,
	})
}

func (s *DeviceSupervisor) connectIridium(port string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	log.Printf("device-supervisor: connecting iridium on %s", port)
	if err := s.iridium.Connect(ctx, port); err != nil {
		log.Printf("device-supervisor: iridium connect failed on %s: %v", port, err)
		s.updateSerialDeviceRole(port, RoleIridium, StateDisconnected)
		return
	}

	s.updateSerialDeviceRole(port, RoleIridium, StateConnected)
	entry := s.registry.FindByDevPath(port)
	s.emitEvent(DeviceEvent{
		Type:   "device_connected",
		Device: entry,
	})
}

// ============================================================================
// SSE Event System
// ============================================================================

// SubscribeEvents registers a new SSE client and returns a channel + unsubscribe func.
func (s *DeviceSupervisor) SubscribeEvents() (<-chan DeviceEvent, func()) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()

	id := s.nextClientID
	s.nextClientID++
	ch := make(chan DeviceEvent, 16)
	s.eventClients[id] = ch

	unsub := func() {
		s.eventMu.Lock()
		defer s.eventMu.Unlock()
		delete(s.eventClients, id)
		close(ch)
	}
	return ch, unsub
}

func (s *DeviceSupervisor) emitEvent(event DeviceEvent) {
	if event.Time == "" {
		event.Time = time.Now().UTC().Format(time.RFC3339)
	}

	s.eventMu.RLock()
	defer s.eventMu.RUnlock()

	for _, ch := range s.eventClients {
		select {
		case ch <- event:
		default:
			// Drop event if client is slow
		}
	}
}
