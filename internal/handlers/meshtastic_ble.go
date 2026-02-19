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

	"github.com/godbus/dbus/v5"
)

// ============================================================================
// BLE Transport — Meshtastic BLE Protocol via BlueZ DBus
// ============================================================================

// BLETransport implements MeshtasticTransport for Bluetooth Low Energy connections.
// It communicates with a Meshtastic device via GATT characteristics using the
// BlueZ DBus API. No additional Go dependencies are needed — only godbus/dbus
// (already an indirect dependency via go-systemd).
//
// Meshtastic BLE Service:
//
//	Service UUID:   6ba1b218-15a8-461f-9fa8-5dcae273eafd
//	toRadio:        f75c76d2-129e-4dad-a1dd-7866124401e7   (write)
//	fromRadio:      2c55e69e-4993-11ed-b878-0242ac120002   (read)
//	fromNum:        ed9da18c-a800-4f66-a670-aa7547e34453   (read, notify, write)
//
// Unlike serial transport, BLE does not use 0x94 0xC3 framing — BLE handles
// packetization natively. The protobuf payload is written/read directly.
type BLETransport struct {
	mu sync.Mutex

	address   string // BLE MAC address (empty = auto-scan)
	adapter   string // BlueZ adapter name (default: "hci0")
	conn      *dbus.Conn
	connected bool

	// DBus object paths discovered during connection
	devicePath    dbus.ObjectPath
	toRadioPath   dbus.ObjectPath
	fromRadioPath dbus.ObjectPath
	fromNumPath   dbus.ObjectPath

	// Received packet queue — fromRadio packets ready for the driver
	packetCh chan []byte
	stopCh   chan struct{}
}

// Meshtastic BLE UUIDs
const (
	meshBLEServiceUUID   = "6ba1b218-15a8-461f-9fa8-5dcae273eafd"
	meshBLEToRadioUUID   = "f75c76d2-129e-4dad-a1dd-7866124401e7"
	meshBLEFromRadioUUID = "2c55e69e-4993-11ed-b878-0242ac120002"
	meshBLEFromNumUUID   = "ed9da18c-a800-4f66-a670-aa7547e34453"

	// BlueZ DBus constants
	bluezBus          = "org.bluez"
	bluezAdapter1     = "org.bluez.Adapter1"
	bluezDevice1      = "org.bluez.Device1"
	bluezGattService  = "org.bluez.GattService1"
	bluezGattChar     = "org.bluez.GattCharacteristic1"
	dbusProperties    = "org.freedesktop.DBus.Properties"
	dbusObjectManager = "org.freedesktop.DBus.ObjectManager"

	// BLE timeouts
	bleScanTimeout    = 5 * time.Second
	bleConnectTimeout = 10 * time.Second
	bleMTUTarget      = 512
)

// BLESafetyError is returned when BLE operation is blocked due to shared radio conflict.
type BLESafetyError struct {
	Message string
}

func (e *BLESafetyError) Error() string {
	return e.Message
}

// isBLEOnOnboardAdapter checks if the BLE adapter shares the radio with WiFi.
// On Raspberry Pi, the onboard Broadcom chip handles both WiFi and BLE on the
// same 2.4GHz radio. A USB BLE dongle gets its own radio and is safe to scan.
func isBLEOnOnboardAdapter(adapter string) bool {
	if adapter == "" || adapter == "hci0" {
		devicePath := "/sys/class/bluetooth/hci0/device"
		link, err := os.Readlink(devicePath)
		if err != nil {
			// Can't determine — assume onboard for safety
			return true
		}
		// Onboard: symlink contains "platform" or "uart"
		// USB dongle: symlink contains "usb"
		return !strings.Contains(link, "usb")
	}
	// Non-hci0 adapters are always USB dongles
	return false
}

// isHostapdActive checks if the WiFi Access Point is running.
// Uses /proc scan instead of systemctl because HAL runs in an Alpine container
// (no systemctl binary) with pid:host namespace (can see host processes).
func isHostapdActive() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		// Only check numeric directories (PIDs)
		if !entry.IsDir() {
			continue
		}
		pid := entry.Name()
		if len(pid) == 0 || pid[0] < '1' || pid[0] > '9' {
			continue
		}
		comm, err := os.ReadFile("/proc/" + pid + "/comm")
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == "hostapd" {
			return true
		}
	}
	return false
}

// findBestBLEAdapter returns the best available BLE adapter.
// Prefers USB adapters (hci1+) over the onboard adapter (hci0) because
// onboard BLE shares the 2.4GHz radio with WiFi on Raspberry Pi.
func findBestBLEAdapter() string {
	for i := 1; i <= 5; i++ {
		path := fmt.Sprintf("/sys/class/bluetooth/hci%d", i)
		if _, err := os.Stat(path); err == nil {
			return fmt.Sprintf("hci%d", i)
		}
	}
	return "hci0" // fallback to onboard
}

// CheckBLESafety verifies that BLE operations are safe to proceed.
// Returns the adapter to use, or a BLESafetyError if BLE would disrupt WiFi AP.
//
// Logic:
//  1. If BLE_ADAPTER env var forces a specific adapter, use it (user override)
//  2. Otherwise, auto-select the best adapter (prefer USB over onboard)
//  3. If selected adapter is onboard AND hostapd is active → block
func CheckBLESafety(requestedAdapter string) (string, error) {
	// Resolve adapter: env var → requested → auto-detect
	adapter := os.Getenv("BLE_ADAPTER")
	if adapter == "" {
		adapter = requestedAdapter
	}
	if adapter == "" {
		adapter = findBestBLEAdapter()
	}

	// Sanitize
	adapter, err := sanitizeBLEAdapterName(adapter)
	if err != nil {
		return "", err
	}

	// Safety check: onboard adapter + active AP = blocked
	if isBLEOnOnboardAdapter(adapter) && isHostapdActive() {
		return "", &BLESafetyError{
			Message: "BLE scan blocked: onboard Bluetooth shares radio with WiFi AP. Connect a USB Bluetooth adapter or use USB serial.",
		}
	}

	return adapter, nil
}

// NewBLETransport creates a new BLE transport.
// If address is empty, auto-scanning for Meshtastic service UUID will be used.
// If adapter is empty, "hci0" (Pi built-in Bluetooth) is used.
func NewBLETransport(address, adapter string) *BLETransport {
	if adapter == "" {
		adapter = "hci0"
	}
	return &BLETransport{
		address:  address,
		adapter:  adapter,
		packetCh: make(chan []byte, 64),
	}
}

// Connect establishes a BLE connection to a Meshtastic device.
// Steps:
//  1. Connect to system DBus
//  2. If no address, scan for Meshtastic service UUID
//  3. Connect to BLE device
//  4. Discover GATT services and characteristics
//  5. Request MTU 512
//  6. Subscribe to fromNum notifications
func (t *BLETransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.connected {
		return nil
	}

	// Connect to system DBus
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("failed to connect to system DBus: %w", err)
	}
	t.conn = conn

	// Find or scan for device
	address := t.address
	if address == "" {
		var scanErr error
		address, scanErr = t.scanForMeshtastic(ctx)
		if scanErr != nil {
			t.conn = nil
			return fmt.Errorf("BLE scan failed: %w", scanErr)
		}
	}
	t.address = address

	// Resolve device DBus path
	t.devicePath = adapterDevicePath(t.adapter, address)

	// Connect to device
	if err := t.connectDevice(ctx); err != nil {
		t.conn = nil
		return fmt.Errorf("BLE connect failed: %w", err)
	}

	// Wait for services to be resolved
	if err := t.waitServicesResolved(ctx); err != nil {
		t.disconnectDevice()
		t.conn = nil
		return fmt.Errorf("service discovery failed: %w", err)
	}

	// Discover GATT characteristics
	if err := t.discoverCharacteristics(); err != nil {
		t.disconnectDevice()
		t.conn = nil
		return fmt.Errorf("GATT discovery failed: %w", err)
	}

	// Request MTU 512 (best effort — not all devices support it)
	t.requestMTU()

	// Subscribe to fromNum notifications
	t.stopCh = make(chan struct{})
	if err := t.subscribeFromNum(); err != nil {
		t.disconnectDevice()
		t.conn = nil
		return fmt.Errorf("fromNum subscription failed: %w", err)
	}

	t.connected = true
	log.Printf("meshtastic: BLE connected to %s via %s", address, t.adapter)
	return nil
}

// Disconnect cleanly closes the BLE connection.
func (t *BLETransport) Disconnect() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.disconnectLocked()
}

func (t *BLETransport) disconnectLocked() error {
	t.connected = false

	// Stop notification listener
	if t.stopCh != nil {
		close(t.stopCh)
		t.stopCh = nil
	}

	// Unsubscribe from notifications
	if t.conn != nil && t.fromNumPath != "" {
		t.stopNotify(t.fromNumPath)
	}

	// Disconnect BLE device
	t.disconnectDevice()

	// Note: We do NOT close t.conn (system DBus connection) — it's a shared
	// cached connection from dbus.SystemBus(). Closing it would break other
	// DBus users in the process. The connection is lightweight and shared.
	// We just nil our reference.
	t.conn = nil

	return nil
}

// SendToRadio writes a protobuf payload to the toRadio GATT characteristic.
// No framing is needed — BLE handles packetization.
func (t *BLETransport) SendToRadio(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.connected || t.conn == nil {
		return fmt.Errorf("not connected")
	}

	if t.toRadioPath == "" {
		return fmt.Errorf("toRadio characteristic not found")
	}

	obj := t.conn.Object(bluezBus, t.toRadioPath)
	// WriteValue with empty options map — type "command" (no response)
	call := obj.Call(bluezGattChar+".WriteValue", 0, data, map[string]dbus.Variant{
		"type": dbus.MakeVariant("command"),
	})
	if call.Err != nil {
		return fmt.Errorf("BLE write failed: %w", call.Err)
	}

	return nil
}

// RecvFromRadio blocks until a FromRadio protobuf payload is available.
// Packets are queued by the notification handler which drains fromRadio
// whenever fromNum fires (Meshtastic BLE protocol).
func (t *BLETransport) RecvFromRadio(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case data, ok := <-t.packetCh:
		if !ok {
			return nil, fmt.Errorf("packet channel closed")
		}
		return data, nil
	}
}

// IsConnected returns the BLE connection state.
func (t *BLETransport) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected
}

// TransportType returns "ble".
func (t *BLETransport) TransportType() string {
	return "ble"
}

// DeviceAddress returns the BLE MAC address.
func (t *BLETransport) DeviceAddress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.address
}

// ============================================================================
// BLE Device Scanning
// ============================================================================

// scanForMeshtastic scans for BLE devices advertising the Meshtastic service UUID.
func (t *BLETransport) scanForMeshtastic(ctx context.Context) (string, error) {
	adapterPath := dbus.ObjectPath("/org/bluez/" + t.adapter)
	adapter := t.conn.Object(bluezBus, adapterPath)

	// Set discovery filter for BLE + Meshtastic service UUID
	filter := map[string]dbus.Variant{
		"Transport": dbus.MakeVariant("le"),
		"UUIDs":     dbus.MakeVariant([]string{meshBLEServiceUUID}),
	}
	call := adapter.Call(bluezAdapter1+".SetDiscoveryFilter", 0, filter)
	if call.Err != nil {
		return "", fmt.Errorf("failed to set discovery filter: %w", call.Err)
	}

	// Start discovery
	call = adapter.Call(bluezAdapter1+".StartDiscovery", 0)
	if call.Err != nil {
		return "", fmt.Errorf("failed to start discovery: %w", call.Err)
	}

	// Ensure we stop discovery when done
	defer func() {
		adapter.Call(bluezAdapter1+".StopDiscovery", 0)
	}()

	// Poll for discovered devices with Meshtastic service UUID
	scanCtx, cancel := context.WithTimeout(ctx, bleScanTimeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-scanCtx.Done():
			return "", fmt.Errorf("no Meshtastic BLE device found within %v", bleScanTimeout)
		case <-ticker.C:
			address, found := t.findMeshtasticDevice()
			if found {
				log.Printf("meshtastic: BLE scan found device %s", address)
				return address, nil
			}
		}
	}
}

// findMeshtasticDevice searches known BlueZ objects for a device with Meshtastic service UUID.
func (t *BLETransport) findMeshtasticDevice() (string, bool) {
	obj := t.conn.Object(bluezBus, "/")

	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	call := obj.Call(dbusObjectManager+".GetManagedObjects", 0)
	if call.Err != nil {
		return "", false
	}
	if err := call.Store(&objects); err != nil {
		return "", false
	}

	for path, ifaces := range objects {
		devProps, ok := ifaces[bluezDevice1]
		if !ok {
			continue
		}

		// Check if path is under our adapter
		if !strings.HasPrefix(string(path), "/org/bluez/"+t.adapter+"/") {
			continue
		}

		// Check UUIDs for Meshtastic service UUID
		uuidsVar, ok := devProps["UUIDs"]
		if !ok {
			continue
		}
		uuids, ok := uuidsVar.Value().([]string)
		if !ok {
			continue
		}

		for _, uuid := range uuids {
			if strings.EqualFold(uuid, meshBLEServiceUUID) {
				// Found it — extract address
				addrVar, ok := devProps["Address"]
				if !ok {
					continue
				}
				address, ok := addrVar.Value().(string)
				if !ok {
					continue
				}
				return address, true
			}
		}
	}

	return "", false
}

// ScanBLEDevices performs a non-destructive BLE scan and returns detected Meshtastic devices.
// This is independent of any active connection — used by the ScanDevices endpoint.
// Returns nil (no BLE results) if scanning is blocked by the safety gate.
func ScanBLEDevices(ctx context.Context, adapter string) ([]MeshtasticDeviceInfo, *BLESafetyError) {
	// Safety gate: check if BLE scanning is safe
	safeAdapter, err := CheckBLESafety(adapter)
	if err != nil {
		if safetyErr, ok := err.(*BLESafetyError); ok {
			log.Printf("meshtastic: BLE scan skipped — %s", safetyErr.Message)
			return nil, safetyErr
		}
		log.Printf("meshtastic: BLE scan skipped — adapter error: %v", err)
		return nil, nil
	}
	adapter = safeAdapter

	if adapter == "" {
		adapter = "hci0"
	}

	conn, err := dbus.SystemBus()
	if err != nil {
		log.Printf("meshtastic: BLE scan failed to connect to DBus: %v", err)
		return nil, nil
	}
	// Note: Do NOT close conn — it's a shared cached system bus connection

	adapterPath := dbus.ObjectPath("/org/bluez/" + adapter)
	adapterObj := conn.Object(bluezBus, adapterPath)

	// Check if adapter is powered
	powered, err := getDBusProperty[bool](conn, adapterPath, bluezAdapter1, "Powered")
	if err != nil || !powered {
		log.Printf("meshtastic: BLE adapter %s not powered or unavailable", adapter)
		return nil, nil
	}

	// Set discovery filter
	filter := map[string]dbus.Variant{
		"Transport": dbus.MakeVariant("le"),
		"UUIDs":     dbus.MakeVariant([]string{meshBLEServiceUUID}),
	}
	adapterObj.Call(bluezAdapter1+".SetDiscoveryFilter", 0, filter)

	// Start a brief scan (3 seconds)
	call := adapterObj.Call(bluezAdapter1+".StartDiscovery", 0)
	if call.Err != nil {
		// Discovery might already be running — that's fine, check existing devices
		log.Printf("meshtastic: BLE StartDiscovery: %v (checking cached devices)", call.Err)
	} else {
		// Brief scan then stop
		scanCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		<-scanCtx.Done()
		cancel()
		adapterObj.Call(bluezAdapter1+".StopDiscovery", 0)
	}

	// Enumerate known devices with Meshtastic service UUID
	var devices []MeshtasticDeviceInfo

	root := conn.Object(bluezBus, "/")
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	call = root.Call(dbusObjectManager+".GetManagedObjects", 0)
	if call.Err != nil {
		return nil, nil
	}
	if err := call.Store(&objects); err != nil {
		return nil, nil
	}

	for path, ifaces := range objects {
		devProps, ok := ifaces[bluezDevice1]
		if !ok {
			continue
		}
		if !strings.HasPrefix(string(path), "/org/bluez/"+adapter+"/") {
			continue
		}

		// Check for Meshtastic service UUID
		uuidsVar, ok := devProps["UUIDs"]
		if !ok {
			continue
		}
		uuids, ok := uuidsVar.Value().([]string)
		if !ok {
			continue
		}

		isMeshtastic := false
		for _, uuid := range uuids {
			if strings.EqualFold(uuid, meshBLEServiceUUID) {
				isMeshtastic = true
				break
			}
		}
		if !isMeshtastic {
			continue
		}

		info := MeshtasticDeviceInfo{
			Responding: true,
		}

		if addrVar, ok := devProps["Address"]; ok {
			if addr, ok := addrVar.Value().(string); ok {
				info.Port = "ble://" + addr
			}
		}
		if nameVar, ok := devProps["Name"]; ok {
			if name, ok := nameVar.Value().(string); ok {
				info.Description = name + " (BLE)"
			}
		} else {
			info.Description = "Meshtastic BLE device"
		}

		devices = append(devices, info)
	}

	return devices, nil
}

// ============================================================================
// BLE Connection Helpers
// ============================================================================

// connectDevice initiates the BLE connection via BlueZ.
func (t *BLETransport) connectDevice(ctx context.Context) error {
	device := t.conn.Object(bluezBus, t.devicePath)

	// Check if already connected
	connected, err := getDBusProperty[bool](t.conn, t.devicePath, bluezDevice1, "Connected")
	if err == nil && connected {
		log.Printf("meshtastic: BLE device %s already connected", t.address)
		return nil
	}

	// Connect
	connectCtx, cancel := context.WithTimeout(ctx, bleConnectTimeout)
	defer cancel()

	call := device.CallWithContext(connectCtx, bluezDevice1+".Connect", 0)
	if call.Err != nil {
		return fmt.Errorf("BlueZ Connect failed for %s: %w", t.address, call.Err)
	}

	// Verify connection
	time.Sleep(500 * time.Millisecond)
	connected, err = getDBusProperty[bool](t.conn, t.devicePath, bluezDevice1, "Connected")
	if err != nil || !connected {
		return fmt.Errorf("device %s did not confirm connection", t.address)
	}

	log.Printf("meshtastic: BLE device %s connected", t.address)
	return nil
}

// disconnectDevice disconnects the BLE device via BlueZ.
func (t *BLETransport) disconnectDevice() {
	if t.conn == nil || t.devicePath == "" {
		return
	}
	device := t.conn.Object(bluezBus, t.devicePath)
	device.Call(bluezDevice1+".Disconnect", 0)
}

// waitServicesResolved waits for BlueZ to complete GATT service discovery.
func (t *BLETransport) waitServicesResolved(ctx context.Context) error {
	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("service discovery timed out after 15s")
		case <-ticker.C:
			resolved, err := getDBusProperty[bool](t.conn, t.devicePath, bluezDevice1, "ServicesResolved")
			if err == nil && resolved {
				return nil
			}
		}
	}
}

// discoverCharacteristics finds the Meshtastic GATT characteristics by UUID.
func (t *BLETransport) discoverCharacteristics() error {
	root := t.conn.Object(bluezBus, "/")

	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	call := root.Call(dbusObjectManager+".GetManagedObjects", 0)
	if call.Err != nil {
		return fmt.Errorf("GetManagedObjects failed: %w", call.Err)
	}
	if err := call.Store(&objects); err != nil {
		return fmt.Errorf("failed to parse managed objects: %w", err)
	}

	devicePrefix := string(t.devicePath) + "/"

	for path, ifaces := range objects {
		charProps, ok := ifaces[bluezGattChar]
		if !ok {
			continue
		}

		// Only consider characteristics under our device
		if !strings.HasPrefix(string(path), devicePrefix) {
			continue
		}

		uuidVar, ok := charProps["UUID"]
		if !ok {
			continue
		}
		uuid, ok := uuidVar.Value().(string)
		if !ok {
			continue
		}

		switch strings.ToLower(uuid) {
		case meshBLEToRadioUUID:
			t.toRadioPath = path
			log.Printf("meshtastic: found toRadio characteristic at %s", path)
		case meshBLEFromRadioUUID:
			t.fromRadioPath = path
			log.Printf("meshtastic: found fromRadio characteristic at %s", path)
		case meshBLEFromNumUUID:
			t.fromNumPath = path
			log.Printf("meshtastic: found fromNum characteristic at %s", path)
		}
	}

	if t.toRadioPath == "" {
		return fmt.Errorf("toRadio characteristic not found")
	}
	if t.fromRadioPath == "" {
		return fmt.Errorf("fromRadio characteristic not found")
	}
	if t.fromNumPath == "" {
		return fmt.Errorf("fromNum characteristic not found")
	}

	return nil
}

// requestMTU attempts to negotiate a larger MTU for BLE transfers.
// BlueZ handles MTU negotiation automatically on connect. We read the
// negotiated MTU for logging. If the device supports 512, it typically
// negotiates to 512 automatically. This is best-effort.
func (t *BLETransport) requestMTU() {
	mtu, err := getDBusProperty[uint16](t.conn, t.devicePath, bluezDevice1, "MTU")
	if err != nil {
		log.Printf("meshtastic: could not read BLE MTU: %v", err)
		return
	}
	log.Printf("meshtastic: BLE negotiated MTU = %d", mtu)
	if mtu < 256 {
		log.Printf("meshtastic: WARNING: BLE MTU %d is low — large protobufs may be fragmented", mtu)
	}
}

// ============================================================================
// GATT Characteristic I/O
// ============================================================================

// readCharacteristic reads a GATT characteristic value via BlueZ DBus.
func (t *BLETransport) readCharacteristic(charPath dbus.ObjectPath) ([]byte, error) {
	obj := t.conn.Object(bluezBus, charPath)
	call := obj.Call(bluezGattChar+".ReadValue", 0, map[string]dbus.Variant{})
	if call.Err != nil {
		return nil, call.Err
	}

	var data []byte
	if err := call.Store(&data); err != nil {
		return nil, fmt.Errorf("failed to decode read result: %w", err)
	}

	return data, nil
}

// subscribeFromNum sets up BLE notifications on the fromNum characteristic.
// When fromNum fires, it drains all available fromRadio packets into packetCh.
func (t *BLETransport) subscribeFromNum() error {
	// Subscribe to PropertiesChanged signals for the fromNum characteristic
	matchRule := fmt.Sprintf(
		"type='signal',sender='%s',interface='%s',member='PropertiesChanged',path='%s'",
		bluezBus, dbusProperties, t.fromNumPath,
	)
	call := t.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, matchRule)
	if call.Err != nil {
		return fmt.Errorf("failed to add signal match: %w", call.Err)
	}

	// Start notification on the characteristic
	fromNumObj := t.conn.Object(bluezBus, t.fromNumPath)
	call = fromNumObj.Call(bluezGattChar+".StartNotify", 0)
	if call.Err != nil {
		return fmt.Errorf("StartNotify failed: %w", call.Err)
	}

	// Drain any buffered data on connect
	go t.drainFromRadio()

	// Listen for DBus signals in background goroutine
	sigCh := make(chan *dbus.Signal, 64)
	t.conn.Signal(sigCh)

	go func() {
		for {
			select {
			case <-t.stopCh:
				t.conn.RemoveSignal(sigCh)
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				// Filter for PropertiesChanged on our fromNum path
				if sig.Path != t.fromNumPath {
					continue
				}
				if sig.Name != dbusProperties+".PropertiesChanged" {
					continue
				}
				// Check if "Value" property changed (notification fired)
				if len(sig.Body) >= 2 {
					changed, ok := sig.Body[1].(map[string]dbus.Variant)
					if ok {
						if _, hasValue := changed["Value"]; hasValue {
							// Drain all available fromRadio packets into packetCh
							t.drainFromRadio()
						}
					}
				}
			}
		}
	}()

	log.Printf("meshtastic: subscribed to fromNum notifications")
	return nil
}

// drainFromRadio reads all available fromRadio packets and queues them.
// Called when fromNum notification fires, or on initial connect.
func (t *BLETransport) drainFromRadio() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil || t.fromRadioPath == "" {
		return
	}

	for i := 0; i < 100; i++ { // Safety limit
		data, err := t.readCharacteristic(t.fromRadioPath)
		if err != nil || len(data) == 0 {
			break
		}
		// Queue the packet for the driver's readerLoop
		select {
		case t.packetCh <- data:
		default:
			log.Printf("meshtastic: BLE packet queue full, dropping packet")
		}
	}
}

// stopNotify stops BLE notifications on a characteristic.
func (t *BLETransport) stopNotify(charPath dbus.ObjectPath) {
	obj := t.conn.Object(bluezBus, charPath)
	obj.Call(bluezGattChar+".StopNotify", 0)
}

// ============================================================================
// DBus Helpers
// ============================================================================

// adapterDevicePath converts a BLE MAC address to a BlueZ DBus object path.
// Example: "AA:BB:CC:DD:EE:FF" → "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"
func adapterDevicePath(adapter, address string) dbus.ObjectPath {
	devAddr := strings.ReplaceAll(address, ":", "_")
	return dbus.ObjectPath(fmt.Sprintf("/org/bluez/%s/dev_%s", adapter, devAddr))
}

// getDBusProperty reads a property from a BlueZ DBus object.
func getDBusProperty[T any](conn *dbus.Conn, path dbus.ObjectPath, iface, property string) (T, error) {
	var zero T
	obj := conn.Object(bluezBus, path)

	variant, err := obj.GetProperty(iface + "." + property)
	if err != nil {
		return zero, err
	}

	val, ok := variant.Value().(T)
	if !ok {
		return zero, fmt.Errorf("property %s.%s has unexpected type %T", iface, property, variant.Value())
	}
	return val, nil
}

// ============================================================================
// BLE Address Validation
// ============================================================================

// validateBLEAddress validates a BLE MAC address format (XX:XX:XX:XX:XX:XX).
func validateBLEAddress(address string) error {
	if address == "" {
		return fmt.Errorf("BLE address is required")
	}
	parts := strings.Split(address, ":")
	if len(parts) != 6 {
		return fmt.Errorf("invalid BLE address format (expected XX:XX:XX:XX:XX:XX)")
	}
	for _, part := range parts {
		if len(part) != 2 {
			return fmt.Errorf("invalid BLE address format (expected XX:XX:XX:XX:XX:XX)")
		}
		for _, c := range part {
			if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')) {
				return fmt.Errorf("invalid BLE address format (non-hex character)")
			}
		}
	}
	return nil
}

// ============================================================================
// BLE Auto-Reconnect (used by MeshtasticDriver)
// ============================================================================

// BLEReconnectConfig holds configuration for BLE auto-reconnect.
type BLEReconnectConfig struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxAttempts  int // 0 = infinite
}

// DefaultBLEReconnectConfig returns the default reconnect configuration.
// Exponential backoff: 1s → 2s → 4s → 8s → 16s → 30s (capped)
func DefaultBLEReconnectConfig() BLEReconnectConfig {
	return BLEReconnectConfig{
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		MaxAttempts:  0, // Infinite
	}
}

// BLEReconnector manages auto-reconnect logic for BLE transport.
type BLEReconnector struct {
	mu      sync.Mutex
	config  BLEReconnectConfig
	driver  *MeshtasticDriver
	running bool
	stopCh  chan struct{}
}

// NewBLEReconnector creates a new auto-reconnect manager.
func NewBLEReconnector(driver *MeshtasticDriver, config BLEReconnectConfig) *BLEReconnector {
	return &BLEReconnector{
		config: config,
		driver: driver,
	}
}

// Start begins monitoring the BLE connection and reconnecting on drops.
func (r *BLEReconnector) Start() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.stopCh = make(chan struct{})
	r.mu.Unlock()

	go r.reconnectLoop()
}

// Stop stops the reconnect loop.
func (r *BLEReconnector) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running && r.stopCh != nil {
		close(r.stopCh)
		r.running = false
	}
}

func (r *BLEReconnector) reconnectLoop() {
	delay := r.config.InitialDelay
	attempts := 0

	for {
		select {
		case <-r.stopCh:
			return
		case <-time.After(2 * time.Second): // Check interval
		}

		if r.driver.IsConnected() {
			// Connected — reset backoff
			delay = r.config.InitialDelay
			attempts = 0
			continue
		}

		// Check if we should be reconnecting (was previously connected via BLE)
		r.driver.mu.RLock()
		transport := r.driver.transport
		r.driver.mu.RUnlock()
		if transport == nil {
			continue // Never connected — don't auto-reconnect
		}
		if transport.TransportType() != "ble" {
			continue // Serial transport — no reconnect needed
		}

		// Attempt reconnect
		if r.config.MaxAttempts > 0 && attempts >= r.config.MaxAttempts {
			log.Printf("meshtastic: BLE reconnect gave up after %d attempts", attempts)
			return
		}

		log.Printf("meshtastic: BLE connection lost — reconnecting in %v (attempt %d)", delay, attempts+1)

		select {
		case <-r.stopCh:
			return
		case <-time.After(delay):
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := r.driver.Connect(ctx, "ble://"+transport.DeviceAddress())
		cancel()

		if err != nil {
			log.Printf("meshtastic: BLE reconnect failed: %v", err)
			attempts++
			// Exponential backoff
			delay = delay * 2
			if delay > r.config.MaxDelay {
				delay = r.config.MaxDelay
			}
		} else {
			log.Printf("meshtastic: BLE reconnected successfully")
			delay = r.config.InitialDelay
			attempts = 0
		}
	}
}

// ============================================================================
// BLE Utility — Check if BlueZ is available
// ============================================================================

// IsBLEAvailable checks if the BlueZ DBus service is reachable and the
// specified adapter exists. Used for graceful degradation when BLE hardware
// is not available.
func IsBLEAvailable(adapter string) bool {
	if adapter == "" {
		adapter = "hci0"
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		return false
	}
	// Do not close conn — shared cached system bus

	adapterPath := dbus.ObjectPath("/org/bluez/" + adapter)
	_, err = getDBusProperty[bool](conn, adapterPath, bluezAdapter1, "Powered")
	return err == nil
}

// knownBLEMeshtasticName checks if a BLE device name matches known Meshtastic patterns.
func knownBLEMeshtasticName(name string) string {
	lower := strings.ToLower(name)
	patterns := map[string]string{
		"meshtastic": "Meshtastic device",
		"t-echo":     "LilyGo T-Echo",
		"t_echo":     "LilyGo T-Echo",
		"heltec":     "Heltec LoRa",
		"rak4631":    "RAK WisBlock 4631",
		"tbeam":      "LILYGO T-Beam",
	}

	// Check if the device path's final segment hints at a known BLE device
	for pattern, desc := range patterns {
		if strings.Contains(lower, pattern) {
			return desc + " (BLE)"
		}
	}
	return ""
}

// BLEDeviceKey generates a stable key for BLE device deduplication based on
// the last 6 hex characters of the MAC address (matches Meshtastic short ID style).
func BLEDeviceKey(address string) string {
	clean := strings.ReplaceAll(address, ":", "")
	if len(clean) >= 6 {
		return clean[len(clean)-6:]
	}
	return clean
}

// IsBLEAddress checks if a string looks like a "ble://" prefixed address
// used for transport routing in Connect().
func IsBLEAddress(s string) bool {
	return strings.HasPrefix(s, "ble://")
}

// ParseBLEAddress strips the "ble://" prefix and validates the MAC.
func ParseBLEAddress(s string) (string, error) {
	if !strings.HasPrefix(s, "ble://") {
		return "", fmt.Errorf("not a BLE address (missing ble:// prefix)")
	}
	addr := strings.TrimPrefix(s, "ble://")
	if err := validateBLEAddress(addr); err != nil {
		return "", err
	}
	return addr, nil
}

// sanitizeBLEAdapterName validates and cleans the adapter name to prevent path traversal.
func sanitizeBLEAdapterName(adapter string) (string, error) {
	if adapter == "" {
		return "hci0", nil
	}
	// Only allow alphanumeric + underscore
	clean := filepath.Base(adapter)
	for _, c := range clean {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return "", fmt.Errorf("invalid adapter name: %s", adapter)
		}
	}
	return clean, nil
}
