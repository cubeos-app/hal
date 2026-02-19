package handlers

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Meshtastic Driver — Protobuf Serial Protocol Engine
// ============================================================================

// MeshtasticDriver manages a persistent connection to a Meshtastic device
// running stock firmware. The driver maintains an in-memory NodeDB,
// streams incoming messages via SSE, and exposes methods for sending
// text and raw packets.
//
// Architecture:
//   - Transport-agnostic: uses MeshtasticTransport interface (serial or BLE)
//   - Phase 2a implements SerialTransport (0x94 0xC3 framed protobuf)
//   - Phase 2b will add BLETransport behind the same interface
//   - NodeDB is rebuilt on every connect via want_config_id handshake
//   - SSE fan-out to all subscribers for real-time message delivery
type MeshtasticDriver struct {
	mu sync.RWMutex

	// Transport layer (serial or BLE)
	transport MeshtasticTransport

	// Connection state
	connected      bool
	configComplete bool
	configID       uint32 // Random ID sent with want_config_id
	myNodeNum      uint32
	firmwareVer    string

	// Node database (populated during config download)
	nodes map[uint32]*MeshNode

	// Message ring buffer
	messages    []*MeshMessage
	msgBufSize  int
	msgBufIndex int

	// Background reader
	stopReader chan struct{}
	readerDone chan struct{}

	// SSE subscribers for real-time message streaming
	eventMu      sync.RWMutex
	eventClients map[uint64]chan MeshEvent
	nextClientID uint64

	// Configuration
	serialBaud int
	serialPort string // Preferred port (empty = auto-detect)
	bleAddress string // Preferred BLE address (empty = auto-scan)
	bleAdapter string // BlueZ adapter (default: hci0)

	// Radio config received during want_config_id handshake
	configData map[string]interface{}

	// BLE reconnect manager
	reconnector *BLEReconnector
}

// MeshNode represents a node in the Meshtastic mesh network.
// @Description Meshtastic mesh network node
type MeshNode struct {
	Num          uint32  `json:"num"`
	UserID       string  `json:"user_id,omitempty" example:"!a1b2c3d4"`
	LongName     string  `json:"long_name,omitempty" example:"CubeOS Node"`
	ShortName    string  `json:"short_name,omitempty" example:"CUBE"`
	HWModel      int     `json:"hw_model,omitempty"`
	HWModelName  string  `json:"hw_model_name,omitempty" example:"HELTEC_V3"`
	Latitude     float64 `json:"latitude,omitempty" example:"52.3676"`
	Longitude    float64 `json:"longitude,omitempty" example:"4.9041"`
	Altitude     int32   `json:"altitude,omitempty" example:"10"`
	Sats         int     `json:"sats,omitempty" example:"8"`
	BatteryLevel int     `json:"battery_level,omitempty" example:"85"`
	Voltage      float32 `json:"voltage,omitempty" example:"4.1"`
	SNR          float32 `json:"snr,omitempty" example:"10.5"`
	LastHeard    int64   `json:"last_heard,omitempty"`
	LastHeardStr string  `json:"last_heard_str,omitempty" example:"2026-02-08T12:00:00Z"`
}

// MeshMessage represents a decoded mesh message.
// @Description Meshtastic mesh message
type MeshMessage struct {
	From        uint32  `json:"from"`
	To          uint32  `json:"to"`
	Channel     uint32  `json:"channel"`
	ID          uint32  `json:"id"`
	PortNum     int     `json:"portnum"`
	PortNumName string  `json:"portnum_name,omitempty" example:"TEXT_MESSAGE_APP"`
	Payload     []byte  `json:"payload,omitempty"`
	DecodedText string  `json:"decoded_text,omitempty" example:"Hello mesh!"`
	RxTime      int64   `json:"rx_time,omitempty"`
	RxSNR       float32 `json:"rx_snr,omitempty"`
	HopLimit    int     `json:"hop_limit,omitempty"`
	HopStart    int     `json:"hop_start,omitempty"`
	Timestamp   string  `json:"timestamp" example:"2026-02-08T12:00:00Z"`
}

// MeshEvent represents an event emitted on the SSE stream.
type MeshEvent struct {
	Type    string      `json:"type"`    // "message", "node_update", "position", "connected", "disconnected", "config_complete"
	Message string      `json:"message"` // Human-readable description
	Data    interface{} `json:"data,omitempty"`
	Time    string      `json:"time"`
}

// MeshtasticTransport abstracts USB serial vs BLE connectivity.
// All protocol logic (protobuf parsing, NodeDB, message routing) sits above this.
type MeshtasticTransport interface {
	// Connect establishes the connection to the Meshtastic device.
	Connect(ctx context.Context) error
	// Disconnect cleanly closes the connection.
	Disconnect() error
	// SendToRadio sends a raw ToRadio protobuf payload to the device.
	SendToRadio(data []byte) error
	// RecvFromRadio blocks until a FromRadio protobuf payload is available.
	RecvFromRadio(ctx context.Context) ([]byte, error)
	// IsConnected returns the current connection state.
	IsConnected() bool
	// TransportType returns "serial" or "ble" for status reporting.
	TransportType() string
	// DeviceAddress returns the port path or BLE MAC for status reporting.
	DeviceAddress() string
}

// NewMeshtasticDriver creates a new Meshtastic driver instance.
func NewMeshtasticDriver() *MeshtasticDriver {
	// BLE adapter selection: env var → auto-detect (prefer USB over onboard)
	bleAdapter := os.Getenv("BLE_ADAPTER")
	if bleAdapter == "" {
		bleAdapter = findBestBLEAdapter()
	}
	log.Printf("meshtastic: using BLE adapter %s", bleAdapter)

	d := &MeshtasticDriver{
		nodes:        make(map[uint32]*MeshNode),
		messages:     make([]*MeshMessage, 0, 1000),
		msgBufSize:   1000,
		eventClients: make(map[uint64]chan MeshEvent),
		serialBaud:   115200,
		bleAdapter:   bleAdapter,
		configData:   make(map[string]interface{}),
	}
	// Start BLE reconnector (monitors connection, auto-reconnects BLE on drop)
	d.reconnector = NewBLEReconnector(d, DefaultBLEReconnectConfig())
	d.reconnector.Start()
	return d
}

// ============================================================================
// Connection Management
// ============================================================================

// Connect establishes a connection to a Meshtastic device.
// The port parameter controls transport selection:
//   - "" (empty)              → auto-detect: try serial first, then BLE
//   - "/dev/ttyACM0"          → explicit serial port
//   - "ble://AA:BB:CC:DD:EE:FF" → explicit BLE address
//   - "ble://"                → BLE auto-scan (skip serial)
//
// It performs the config handshake to download the NodeDB from the device.
func (d *MeshtasticDriver) Connect(ctx context.Context, port string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Close existing connection if any
	d.disconnectLocked()

	// Determine transport based on port parameter
	var transport MeshtasticTransport
	var transportErr error

	switch {
	case IsBLEAddress(port):
		// Explicit BLE: "ble://AA:BB:CC:DD:EE:FF" or "ble://" (auto-scan)
		addr := strings.TrimPrefix(port, "ble://")
		// Safety gate: check BLE/WiFi coexistence before proceeding
		safeAdapter, safeErr := CheckBLESafety(d.bleAdapter)
		if safeErr != nil {
			return safeErr
		}
		transport = NewBLETransport(addr, safeAdapter)
		log.Printf("meshtastic: connecting via BLE (address=%q, adapter=%s)", addr, safeAdapter)

	case port != "":
		// Explicit serial port
		transport = NewSerialTransport(port, d.serialBaud)
		log.Printf("meshtastic: connecting via serial (port=%s)", port)

	default:
		// Auto-detect: try serial first, then BLE fallback
		serialPort := d.serialPort
		if serialPort == "" {
			// Check if serial candidates exist
			candidates := findSerialCandidates()
			for _, c := range candidates {
				if isMeshtasticVIDPID(c) {
					serialPort = c
					break
				}
			}
			if serialPort == "" && len(candidates) > 0 {
				// Try first ACM device
				for _, c := range candidates {
					if strings.Contains(c, "ttyACM") {
						serialPort = c
						break
					}
				}
			}
		}

		if serialPort != "" {
			log.Printf("meshtastic: auto-detect trying serial %s", serialPort)
			serialTransport := NewSerialTransport(serialPort, d.serialBaud)
			if err := serialTransport.Connect(ctx); err == nil {
				transport = serialTransport
			} else {
				log.Printf("meshtastic: serial failed (%v), trying BLE", err)
				transportErr = err
			}
		}

		if transport == nil {
			// Try BLE fallback — but only if safe (not disrupting WiFi AP)
			safeAdapter, safeErr := CheckBLESafety(d.bleAdapter)
			if safeErr != nil {
				// BLE blocked by safety gate — report alongside any serial error
				if transportErr != nil {
					transportErr = fmt.Errorf("serial: %v; %w", transportErr, safeErr)
				} else {
					transportErr = safeErr
				}
			} else if IsBLEAvailable(safeAdapter) {
				log.Printf("meshtastic: auto-detect trying BLE via %s", safeAdapter)
				bleTransport := NewBLETransport(d.bleAddress, safeAdapter)
				if err := bleTransport.Connect(ctx); err == nil {
					transport = bleTransport
				} else {
					if transportErr != nil {
						transportErr = fmt.Errorf("serial: %v; BLE: %w", transportErr, err)
					} else {
						transportErr = fmt.Errorf("BLE: %w", err)
					}
				}
			} else if transportErr == nil {
				transportErr = fmt.Errorf("no serial devices found and BLE not available")
			}
		}

		if transport == nil {
			return fmt.Errorf("auto-detect failed: %w", transportErr)
		}
	}

	// Connect transport (if not already connected during auto-detect)
	if !transport.IsConnected() {
		if err := transport.Connect(ctx); err != nil {
			return fmt.Errorf("transport connect failed: %w", err)
		}
	}

	d.transport = transport
	d.connected = true
	d.configComplete = false
	d.nodes = make(map[uint32]*MeshNode)
	d.configData = make(map[string]interface{})

	// Generate random config ID for handshake
	d.configID = uint32(time.Now().UnixNano() & 0xFFFFFFFF)

	// Send want_config_id to initiate NodeDB download
	configReq := buildWantConfigID(d.configID)
	if err := d.transport.SendToRadio(configReq); err != nil {
		d.disconnectLocked()
		return fmt.Errorf("failed to send want_config_id: %w", err)
	}

	// Start background reader goroutine
	d.stopReader = make(chan struct{})
	d.readerDone = make(chan struct{})
	go d.readerLoop()

	// Wait for config_complete_id or timeout
	configTimeout := 15 * time.Second
	deadline := time.After(configTimeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// Release lock while waiting for config
	d.mu.Unlock()
	defer d.mu.Lock()

	for {
		select {
		case <-deadline:
			// Config download timed out — still connected, but NodeDB may be partial
			log.Printf("meshtastic: config download timed out after %v (continuing with partial NodeDB)", configTimeout)
			d.mu.Lock()
			d.configComplete = true // Mark as complete to avoid blocking
			d.mu.Unlock()
			d.emitEvent(MeshEvent{
				Type:    "connected",
				Message: fmt.Sprintf("connected to %s via %s (config timeout, partial NodeDB)", d.transport.DeviceAddress(), d.transport.TransportType()),
				Time:    time.Now().UTC().Format(time.RFC3339),
			})
			return nil
		case <-ticker.C:
			d.mu.RLock()
			complete := d.configComplete
			d.mu.RUnlock()
			if complete {
				d.emitEvent(MeshEvent{
					Type:    "connected",
					Message: fmt.Sprintf("connected to %s via %s (%d nodes)", d.transport.DeviceAddress(), d.transport.TransportType(), len(d.nodes)),
					Time:    time.Now().UTC().Format(time.RFC3339),
				})
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Disconnect cleanly closes the connection to the Meshtastic device.
func (d *MeshtasticDriver) Disconnect() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnectLocked()
	d.emitEvent(MeshEvent{
		Type:    "disconnected",
		Message: "disconnected from Meshtastic device",
		Time:    time.Now().UTC().Format(time.RFC3339),
	})
}

func (d *MeshtasticDriver) disconnectLocked() {
	if d.stopReader != nil {
		close(d.stopReader)
		d.stopReader = nil
	}
	if d.transport != nil {
		_ = d.transport.Disconnect()
	}
	d.connected = false
	d.configComplete = false
	d.myNodeNum = 0
}

// IsConnected returns whether the driver has an active connection.
func (d *MeshtasticDriver) IsConnected() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.connected && d.transport != nil && d.transport.IsConnected()
}

// ============================================================================
// Background Reader — processes all incoming FromRadio packets
// ============================================================================

func (d *MeshtasticDriver) readerLoop() {
	defer func() {
		if d.readerDone != nil {
			close(d.readerDone)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-d.stopReader
		cancel()
	}()

	for {
		select {
		case <-d.stopReader:
			return
		default:
		}

		data, err := d.transport.RecvFromRadio(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // Stopped
			}
			log.Printf("meshtastic: reader error: %v", err)
			// Connection lost — mark disconnected
			d.mu.Lock()
			d.connected = false
			d.mu.Unlock()
			d.emitEvent(MeshEvent{
				Type:    "disconnected",
				Message: fmt.Sprintf("connection lost: %v", err),
				Time:    time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		if len(data) == 0 {
			continue
		}

		d.processFromRadio(data)
	}
}

// processFromRadio parses a FromRadio protobuf and updates driver state.
func (d *MeshtasticDriver) processFromRadio(data []byte) {
	fr, err := parseFromRadio(data)
	if err != nil {
		log.Printf("meshtastic: failed to parse FromRadio: %v", err)
		return
	}

	switch {
	case fr.MyInfo != nil:
		d.mu.Lock()
		d.myNodeNum = fr.MyInfo.MyNodeNum
		d.mu.Unlock()
		log.Printf("meshtastic: my node num = %08x", fr.MyInfo.MyNodeNum)

	case fr.NodeInfo != nil:
		d.mu.Lock()
		node := d.nodeFromInfo(fr.NodeInfo)
		d.nodes[node.Num] = node
		d.mu.Unlock()
		d.emitEvent(MeshEvent{
			Type:    "node_update",
			Message: fmt.Sprintf("node %s (%s)", node.LongName, nodeIDStr(node.Num)),
			Data:    node,
			Time:    time.Now().UTC().Format(time.RFC3339),
		})

	case fr.ConfigCompleteID != 0:
		d.mu.Lock()
		if fr.ConfigCompleteID == d.configID {
			d.configComplete = true
			log.Printf("meshtastic: config complete (%d nodes)", len(d.nodes))
		}
		d.mu.Unlock()
		d.emitEvent(MeshEvent{
			Type:    "config_complete",
			Message: fmt.Sprintf("config download complete (%d nodes)", len(d.nodes)),
			Time:    time.Now().UTC().Format(time.RFC3339),
		})

	case fr.Packet != nil:
		d.handleMeshPacket(fr.Packet)
	}

	// Store config/module_config packets received during handshake (independent of above cases)
	if fr.ConfigRaw != nil {
		d.storeConfigSection("config", fr.ConfigRaw, configSectionNames)
	}
	if fr.ModuleConfigRaw != nil {
		d.storeConfigSection("module_config", fr.ModuleConfigRaw, moduleConfigSectionNames)
	}
	if fr.ChannelRaw != nil {
		d.storeChannelFromHandshake(fr.ChannelRaw)
	}
}

// handleMeshPacket processes a decoded MeshPacket.
func (d *MeshtasticDriver) handleMeshPacket(pkt *ProtoMeshPacket) {
	if pkt.Decoded == nil {
		return // Encrypted packet — can't decode without channel key
	}

	msg := &MeshMessage{
		From:        pkt.From,
		To:          pkt.To,
		Channel:     pkt.Channel,
		ID:          pkt.ID,
		PortNum:     pkt.Decoded.PortNum,
		PortNumName: portNumName(pkt.Decoded.PortNum),
		Payload:     pkt.Decoded.Payload,
		RxSNR:       pkt.RxSNR,
		HopLimit:    int(pkt.HopLimit),
		HopStart:    int(pkt.HopStart),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	if pkt.RxTime > 0 {
		msg.RxTime = int64(pkt.RxTime)
	}

	// Decode text messages
	if pkt.Decoded.PortNum == PortNumTextMessage && len(pkt.Decoded.Payload) > 0 {
		msg.DecodedText = string(pkt.Decoded.Payload)
	}

	// Update NodeDB from position/telemetry packets
	d.updateNodeFromPacket(pkt)

	// Add to ring buffer
	d.mu.Lock()
	if len(d.messages) < d.msgBufSize {
		d.messages = append(d.messages, msg)
	} else {
		d.messages[d.msgBufIndex] = msg
	}
	d.msgBufIndex = (d.msgBufIndex + 1) % d.msgBufSize
	d.mu.Unlock()

	// Emit SSE event
	d.emitEvent(MeshEvent{
		Type:    "message",
		Message: fmt.Sprintf("from %s portnum=%s", nodeIDStr(pkt.From), portNumName(pkt.Decoded.PortNum)),
		Data:    msg,
		Time:    msg.Timestamp,
	})
}

// updateNodeFromPacket updates the NodeDB from position/telemetry/nodeinfo packets.
func (d *MeshtasticDriver) updateNodeFromPacket(pkt *ProtoMeshPacket) {
	if pkt.Decoded == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	node, exists := d.nodes[pkt.From]
	if !exists {
		node = &MeshNode{Num: pkt.From}
		d.nodes[pkt.From] = node
	}

	node.LastHeard = time.Now().Unix()
	node.LastHeardStr = time.Now().UTC().Format(time.RFC3339)
	if pkt.RxSNR != 0 {
		node.SNR = pkt.RxSNR
	}

	switch pkt.Decoded.PortNum {
	case PortNumPosition:
		pos, err := parsePosition(pkt.Decoded.Payload)
		if err == nil {
			node.Latitude = float64(pos.LatitudeI) / 1e7
			node.Longitude = float64(pos.LongitudeI) / 1e7
			node.Altitude = pos.Altitude
			node.Sats = int(pos.SatsInView)
		}

	case PortNumNodeInfo:
		user, err := parseUser(pkt.Decoded.Payload)
		if err == nil {
			node.UserID = user.ID
			node.LongName = user.LongName
			node.ShortName = user.ShortName
			node.HWModel = int(user.HWModel)
			node.HWModelName = hwModelName(int(user.HWModel))
		}

	case PortNumTelemetry:
		dm, err := parseDeviceMetrics(pkt.Decoded.Payload)
		if err == nil {
			if dm.BatteryLevel > 0 {
				node.BatteryLevel = int(dm.BatteryLevel)
			}
			if dm.Voltage > 0 {
				node.Voltage = dm.Voltage
			}
		}
	}
}

// ============================================================================
// Send Operations
// ============================================================================

// SendText sends a text message to the mesh network.
func (d *MeshtasticDriver) SendText(ctx context.Context, text string, to uint32, channel uint32) error {
	d.mu.RLock()
	if !d.connected || d.transport == nil {
		d.mu.RUnlock()
		return fmt.Errorf("not connected to Meshtastic device")
	}
	d.mu.RUnlock()

	// Build MeshPacket with TEXT_MESSAGE_APP portnum
	packet := buildTextMessage(text, to, channel)
	toRadio := buildToRadioPacket(packet)

	if err := d.transport.SendToRadio(toRadio); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}

	return nil
}

// SendRaw sends a raw payload with the specified portnum.
func (d *MeshtasticDriver) SendRaw(ctx context.Context, payload []byte, portnum int, to uint32, channel uint32, wantAck bool) error {
	d.mu.RLock()
	if !d.connected || d.transport == nil {
		d.mu.RUnlock()
		return fmt.Errorf("not connected to Meshtastic device")
	}
	d.mu.RUnlock()

	packet := buildRawPacket(payload, portnum, to, channel, wantAck)
	toRadio := buildToRadioPacket(packet)

	if err := d.transport.SendToRadio(toRadio); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}

	return nil
}

// ============================================================================
// Status Queries
// ============================================================================

// GetNodes returns a copy of all known mesh nodes.
func (d *MeshtasticDriver) GetNodes() []*MeshNode {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nodes := make([]*MeshNode, 0, len(d.nodes))
	for _, node := range d.nodes {
		n := *node // Copy
		nodes = append(nodes, &n)
	}
	return nodes
}

// GetMyNodeNum returns the local node number.
func (d *MeshtasticDriver) GetMyNodeNum() uint32 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.myNodeNum
}

// GetMyNode returns the local node's info, if available.
func (d *MeshtasticDriver) GetMyNode() *MeshNode {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.myNodeNum == 0 {
		return nil
	}
	if node, ok := d.nodes[d.myNodeNum]; ok {
		n := *node
		return &n
	}
	return nil
}

// GetMessages returns recent messages from the ring buffer.
func (d *MeshtasticDriver) GetMessages(limit int) []*MeshMessage {
	d.mu.RLock()
	defer d.mu.RUnlock()

	total := len(d.messages)
	if limit <= 0 || limit > total {
		limit = total
	}

	result := make([]*MeshMessage, limit)
	// Return most recent first
	for i := 0; i < limit; i++ {
		idx := total - 1 - i
		if idx >= 0 {
			result[i] = d.messages[idx]
		}
	}
	return result
}

// GetPosition returns the local Meshtastic node's GPS position.
func (d *MeshtasticDriver) GetPosition() *MeshNode {
	return d.GetMyNode()
}

// ============================================================================
// Device Scanning (stateless — does not affect persistent connection)
// ============================================================================

// ScanDevices scans for Meshtastic devices on serial ports and BLE.
// This is independent of the persistent connection.
func (d *MeshtasticDriver) ScanDevices(ctx context.Context) ([]MeshtasticDeviceInfo, *BLESafetyError) {
	// Scan serial ports
	devices := scanMeshtasticPorts(ctx)

	// Scan BLE devices (may be skipped if safety gate blocks)
	adapter, _ := sanitizeBLEAdapterName(d.bleAdapter)
	bleDevices, bleWarning := ScanBLEDevices(ctx, adapter)
	devices = append(devices, bleDevices...)

	// Emit SSE event if BLE was blocked so dashboard can show warning
	if bleWarning != nil {
		d.emitEvent(MeshEvent{
			Type:    "ble_blocked",
			Message: bleWarning.Message,
			Data:    map[string]string{"reason": "shared_radio"},
			Time:    time.Now().UTC().Format(time.RFC3339),
		})
	}

	return devices, bleWarning
}

// MeshtasticDeviceInfo holds info about a detected Meshtastic device.
// @Description Detected Meshtastic device
type MeshtasticDeviceInfo struct {
	Port          string `json:"port" example:"/dev/ttyACM0"`
	Description   string `json:"description,omitempty" example:"Heltec LoRa V4"`
	VID           string `json:"vid,omitempty" example:"303a"`
	PID           string `json:"pid,omitempty" example:"1001"`
	Responding    bool   `json:"responding" example:"true"`
	TransportType string `json:"transport_type,omitempty" example:"serial"`
}

// ============================================================================
// SSE Event Streaming
// ============================================================================

// SubscribeEvents returns a channel for receiving mesh events and an
// unsubscribe function. The channel is buffered (32) — slow consumers
// will have events dropped rather than blocking the driver.
func (d *MeshtasticDriver) SubscribeEvents() (<-chan MeshEvent, func()) {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()

	ch := make(chan MeshEvent, 32)
	id := d.nextClientID
	d.nextClientID++
	d.eventClients[id] = ch

	unsubscribe := func() {
		d.eventMu.Lock()
		defer d.eventMu.Unlock()
		delete(d.eventClients, id)
		close(ch)
	}

	return ch, unsubscribe
}

// emitEvent fans out an event to all subscribers.
func (d *MeshtasticDriver) emitEvent(event MeshEvent) {
	d.eventMu.RLock()
	defer d.eventMu.RUnlock()

	for _, ch := range d.eventClients {
		select {
		case ch <- event:
		default:
			// Drop event for slow consumer
		}
	}
}

// ============================================================================
// Node DB Helper
// ============================================================================

func (d *MeshtasticDriver) nodeFromInfo(info *ProtoNodeInfo) *MeshNode {
	node := &MeshNode{
		Num: info.Num,
	}

	if info.User != nil {
		node.UserID = info.User.ID
		node.LongName = info.User.LongName
		node.ShortName = info.User.ShortName
		node.HWModel = int(info.User.HWModel)
		node.HWModelName = hwModelName(int(info.User.HWModel))
	}

	if info.Position != nil {
		node.Latitude = float64(info.Position.LatitudeI) / 1e7
		node.Longitude = float64(info.Position.LongitudeI) / 1e7
		node.Altitude = info.Position.Altitude
		node.Sats = int(info.Position.SatsInView)
	}

	if info.DeviceMetrics != nil {
		node.BatteryLevel = int(info.DeviceMetrics.BatteryLevel)
		node.Voltage = info.DeviceMetrics.Voltage
	}

	node.SNR = info.SNR

	if info.LastHeard > 0 {
		node.LastHeard = int64(info.LastHeard)
		node.LastHeardStr = time.Unix(int64(info.LastHeard), 0).UTC().Format(time.RFC3339)
	}

	return node
}

// ============================================================================
// Protobuf Builders — ToRadio messages
// ============================================================================

// buildWantConfigID builds a ToRadio protobuf with want_config_id set.
// ToRadio field 3 (uint32) = want_config_id
func buildWantConfigID(configID uint32) []byte {
	// Protobuf wire format: field 3, wire type 0 (varint)
	// Key = (3 << 3) | 0 = 24 = 0x18
	buf := make([]byte, 0, 8)
	buf = append(buf, 0x18) // field 3, varint
	buf = appendVarint(buf, uint64(configID))
	return buf
}

// buildToRadioPacket wraps a MeshPacket into a ToRadio message.
// ToRadio field 1 (MeshPacket) = packet
func buildToRadioPacket(meshPacket []byte) []byte {
	// Key = (1 << 3) | 2 = 0x0A (field 1, length-delimited)
	buf := make([]byte, 0, len(meshPacket)+8)
	buf = append(buf, 0x0A) // field 1, length-delimited
	buf = appendVarint(buf, uint64(len(meshPacket)))
	buf = append(buf, meshPacket...)
	return buf
}

// buildTextMessage builds a MeshPacket with TEXT_MESSAGE_APP portnum.
func buildTextMessage(text string, to uint32, channel uint32) []byte {
	// Build Data submessage
	payload := []byte(text)
	data := make([]byte, 0, len(payload)+8)
	// Data field 1: portnum (varint) = TEXT_MESSAGE_APP = 1
	data = append(data, 0x08) // field 1, varint
	data = appendVarint(data, uint64(PortNumTextMessage))
	// Data field 2: payload (bytes)
	data = append(data, 0x12) // field 2, length-delimited
	data = appendVarint(data, uint64(len(payload)))
	data = append(data, payload...)

	return buildMeshPacket(data, to, channel)
}

// buildRawPacket builds a MeshPacket with arbitrary portnum.
func buildRawPacket(payload []byte, portnum int, to uint32, channel uint32, wantAck bool) []byte {
	data := make([]byte, 0, len(payload)+16)
	// Data field 1: portnum
	data = append(data, 0x08)
	data = appendVarint(data, uint64(portnum))
	// Data field 2: payload
	data = append(data, 0x12)
	data = appendVarint(data, uint64(len(payload)))
	data = append(data, payload...)
	// Data field 3: want_response (bool = varint 1)
	if wantAck {
		data = append(data, 0x18, 0x01)
	}

	return buildMeshPacket(data, to, channel)
}

// buildMeshPacket builds the outer MeshPacket wrapper.
func buildMeshPacket(decodedData []byte, to uint32, channel uint32) []byte {
	pkt := make([]byte, 0, len(decodedData)+32)

	// MeshPacket field 2: to (uint32)
	if to == 0 {
		to = 0xFFFFFFFF // Broadcast
	}
	pkt = append(pkt, 0x10) // field 2, varint
	pkt = appendVarint(pkt, uint64(to))

	// MeshPacket field 3: channel (uint32) — only if non-zero
	if channel > 0 {
		pkt = append(pkt, 0x18) // field 3, varint
		pkt = appendVarint(pkt, uint64(channel))
	}

	// MeshPacket field 4: decoded (Data, length-delimited)
	pkt = append(pkt, 0x22) // field 4, length-delimited
	pkt = appendVarint(pkt, uint64(len(decodedData)))
	pkt = append(pkt, decodedData...)

	// MeshPacket field 9: hop_limit (uint32) = 3 (default)
	pkt = append(pkt, 0x48) // field 9, varint
	pkt = appendVarint(pkt, 3)

	return pkt
}

// ============================================================================
// Config Storage (populated during want_config_id handshake)
// ============================================================================

// configSectionNames maps FromRadio.Config field numbers to readable names.
// Meshtastic protobuf: Config is a one-of with these fields.
var configSectionNames = map[uint32]string{
	1: "device",
	2: "position",
	3: "power",
	4: "network",
	5: "display",
	6: "lora",
	7: "bluetooth",
}

// moduleConfigSectionNames maps FromRadio.ModuleConfig field numbers to readable names.
var moduleConfigSectionNames = map[uint32]string{
	1: "mqtt",
	2: "serial",
	3: "external_notification",
	4: "store_forward",
	5: "range_test",
	6: "telemetry",
	7: "canned_message",
	8: "audio",
	9: "remote_hardware",
}

// storeConfigSection parses a Config or ModuleConfig protobuf message to identify the
// section (by field number), then stores a generic decode of the inner message.
// category is "config" or "module_config".
func (d *MeshtasticDriver) storeConfigSection(category string, raw []byte, nameMap map[uint32]string) {
	pos := 0
	for pos < len(raw) {
		fieldNum, wireType, newPos, err := readTag(raw, pos)
		if err != nil {
			return
		}
		pos = newPos

		if wireType == 2 { // Length-delimited — this is the nested config section
			inner, newPos, err := readLengthDelimited(raw, pos)
			if err != nil {
				return
			}
			pos = newPos

			sectionName, ok := nameMap[fieldNum]
			if !ok {
				sectionName = fmt.Sprintf("unknown_%d", fieldNum)
			}

			decoded := decodeProtoToMap(inner)
			key := category + "." + sectionName

			d.mu.Lock()
			d.configData[key] = decoded
			d.mu.Unlock()
		} else {
			// Skip non-embedded fields (shouldn't occur in Config/ModuleConfig one-of)
			pos = skipField(raw, pos, wireType)
			if pos < 0 {
				return
			}
		}
	}
}

// decodeProtoToMap performs a generic decode of protobuf bytes into a map.
// Keys are field numbers as strings. Values are varints (uint64), fixed values,
// strings (valid UTF-8 length-delimited), or nested maps (invalid UTF-8 length-delimited).
// Repeated fields are collected into slices.
func decodeProtoToMap(data []byte) map[string]interface{} {
	result := make(map[string]interface{})
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			break
		}
		pos = newPos
		key := strconv.FormatUint(uint64(fieldNum), 10)

		var value interface{}
		switch wireType {
		case 0: // Varint
			val, n := readVarint(data, pos)
			if n <= 0 {
				return result
			}
			pos += n
			value = val

		case 1: // Fixed 64-bit
			if pos+8 > len(data) {
				return result
			}
			val := uint64(data[pos]) | uint64(data[pos+1])<<8 | uint64(data[pos+2])<<16 |
				uint64(data[pos+3])<<24 | uint64(data[pos+4])<<32 | uint64(data[pos+5])<<40 |
				uint64(data[pos+6])<<48 | uint64(data[pos+7])<<56
			pos += 8
			value = val

		case 2: // Length-delimited (string, bytes, or nested message)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return result
			}
			pos = newPos

			if len(val) == 0 {
				value = ""
			} else if isValidUTF8String(val) {
				value = string(val)
			} else {
				// Try to parse as nested message
				nested := decodeProtoToMap(val)
				if len(nested) > 0 {
					value = nested
				} else {
					value = base64.StdEncoding.EncodeToString(val)
				}
			}

		case 5: // Fixed 32-bit
			if pos+4 > len(data) {
				return result
			}
			val := uint32(data[pos]) | uint32(data[pos+1])<<8 | uint32(data[pos+2])<<16 |
				uint32(data[pos+3])<<24
			pos += 4
			value = val

		default:
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return result
			}
			continue
		}

		// Handle repeated fields by collecting into slices
		if existing, ok := result[key]; ok {
			switch v := existing.(type) {
			case []interface{}:
				result[key] = append(v, value)
			default:
				result[key] = []interface{}{v, value}
			}
		} else {
			result[key] = value
		}
	}

	return result
}

// isValidUTF8String checks if bytes look like a valid UTF-8 string (not binary data).
// Returns false for data with null bytes or non-printable control characters.
func isValidUTF8String(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return false // Null byte — likely binary
		}
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			return false // Non-printable control character
		}
	}
	// Must be valid UTF-8
	for i := 0; i < len(data); {
		if data[i] < 0x80 {
			i++
			continue
		}
		// Multi-byte sequence validation
		if data[i]&0xE0 == 0xC0 {
			if i+1 >= len(data) || data[i+1]&0xC0 != 0x80 {
				return false
			}
			i += 2
		} else if data[i]&0xF0 == 0xE0 {
			if i+2 >= len(data) || data[i+1]&0xC0 != 0x80 || data[i+2]&0xC0 != 0x80 {
				return false
			}
			i += 3
		} else if data[i]&0xF8 == 0xF0 {
			if i+3 >= len(data) || data[i+1]&0xC0 != 0x80 || data[i+2]&0xC0 != 0x80 || data[i+3]&0xC0 != 0x80 {
				return false
			}
			i += 4
		} else {
			return false
		}
	}
	return true
}

// GetConfig returns the radio config data received during the handshake.
// Returns nil if not connected or no config has been received.
func (d *MeshtasticDriver) GetConfig() map[string]interface{} {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if !d.connected || len(d.configData) == 0 {
		return nil
	}

	// Build structured response: separate config and module_config sections
	config := make(map[string]interface{})
	moduleConfig := make(map[string]interface{})

	for key, val := range d.configData {
		if len(key) > 7 && key[:7] == "config." {
			config[key[7:]] = val
		} else if len(key) > 14 && key[:14] == "module_config." {
			moduleConfig[key[14:]] = val
		}
	}

	result := make(map[string]interface{})
	if len(config) > 0 {
		result["config"] = config
	}
	if len(moduleConfig) > 0 {
		result["module_config"] = moduleConfig
	}

	// Include channel data if available
	channels := make(map[string]interface{})
	for key, val := range d.configData {
		if len(key) > 8 && key[:8] == "channel." {
			channels[key[8:]] = val
		}
	}
	if len(channels) > 0 {
		result["channels"] = channels
	}

	return result
}

// storeChannelFromHandshake parses a Channel protobuf message from the handshake
// and stores it in configData keyed by channel index (e.g., "channel.0").
// Channel proto: field 1 = index (uint32), field 2 = settings (ChannelSettings), field 3 = role (enum).
func (d *MeshtasticDriver) storeChannelFromHandshake(raw []byte) {
	var chIndex uint32
	var role uint64
	var settings map[string]interface{}

	pos := 0
	for pos < len(raw) {
		fieldNum, wireType, newPos, err := readTag(raw, pos)
		if err != nil {
			return
		}
		pos = newPos

		switch fieldNum {
		case 1: // index (uint32)
			val, n := readVarint(raw, pos)
			if n <= 0 {
				return
			}
			chIndex = uint32(val)
			pos += n
		case 2: // settings (ChannelSettings, length-delimited)
			if wireType == 2 {
				inner, newPos, err := readLengthDelimited(raw, pos)
				if err != nil {
					return
				}
				settings = decodeProtoToMap(inner)
				pos = newPos
			} else {
				pos = skipField(raw, pos, wireType)
				if pos < 0 {
					return
				}
			}
		case 3: // role (enum = varint)
			val, n := readVarint(raw, pos)
			if n <= 0 {
				return
			}
			role = val
			pos += n
		default:
			pos = skipField(raw, pos, wireType)
			if pos < 0 {
				return
			}
		}
	}

	channelData := map[string]interface{}{
		"index": chIndex,
		"role":  channelRoleName(role),
	}
	if settings != nil {
		channelData["settings"] = settings
	}

	key := fmt.Sprintf("channel.%d", chIndex)
	d.mu.Lock()
	d.configData[key] = channelData
	d.mu.Unlock()
}

// channelRoleName returns a human-readable name for a Channel.Role enum value.
func channelRoleName(role uint64) string {
	switch role {
	case 0:
		return "DISABLED"
	case 1:
		return "PRIMARY"
	case 2:
		return "SECONDARY"
	default:
		return fmt.Sprintf("ROLE_%d", role)
	}
}

// ChannelConfig holds the parameters for setting a Meshtastic channel.
type ChannelConfig struct {
	Index           uint32 // 0 = primary, 1-7 = secondary
	Name            string // Channel name
	PSK             []byte // Pre-shared key: 0, 1, 16, or 32 bytes
	Role            uint32 // 0=DISABLED, 1=PRIMARY, 2=SECONDARY
	UplinkEnabled   bool
	DownlinkEnabled bool
}

// SetChannel sends a channel configuration to the connected Meshtastic radio.
// Uses AdminMessage (portnum 6) self-addressed to own node.
func (d *MeshtasticDriver) SetChannel(ctx context.Context, ch ChannelConfig) error {
	d.mu.RLock()
	if !d.connected || d.transport == nil {
		d.mu.RUnlock()
		return fmt.Errorf("not connected to Meshtastic device")
	}
	myNode := d.myNodeNum
	d.mu.RUnlock()

	if myNode == 0 {
		return fmt.Errorf("own node number not known (config handshake may not have completed)")
	}

	// Build the AdminMessage → MeshPacket → ToRadio chain
	toRadio := buildSetChannelToRadio(myNode, ch)
	if err := d.transport.SendToRadio(toRadio); err != nil {
		return fmt.Errorf("send channel config failed: %w", err)
	}

	return nil
}

// buildSetChannelToRadio builds a complete ToRadio message containing an AdminMessage
// with set_channel (field 2) to configure a Meshtastic channel.
//
// Wire structure:
//
//	ToRadio { field 1: MeshPacket {
//	  from: myNodeNum, to: myNodeNum,
//	  decoded: Data { portnum: ADMIN_APP(6), payload: AdminMessage {
//	    field 2: Channel { index, settings: ChannelSettings { psk, name, ... }, role }
//	  }},
//	  want_ack: true
//	}}
func buildSetChannelToRadio(myNodeNum uint32, ch ChannelConfig) []byte {
	// Step 1: Build ChannelSettings (innermost)
	settings := make([]byte, 0, 64)
	// ChannelSettings field 3: psk (bytes)
	if len(ch.PSK) > 0 {
		settings = append(settings, 0x1A) // field 3, length-delimited
		settings = appendVarint(settings, uint64(len(ch.PSK)))
		settings = append(settings, ch.PSK...)
	}
	// ChannelSettings field 4: name (string)
	if ch.Name != "" {
		settings = append(settings, 0x22) // field 4, length-delimited
		settings = appendVarint(settings, uint64(len(ch.Name)))
		settings = append(settings, []byte(ch.Name)...)
	}
	// ChannelSettings field 6: uplink_enabled (bool)
	if ch.UplinkEnabled {
		settings = append(settings, 0x30, 0x01) // field 6, varint 1
	}
	// ChannelSettings field 7: downlink_enabled (bool)
	if ch.DownlinkEnabled {
		settings = append(settings, 0x38, 0x01) // field 7, varint 1
	}

	// Step 2: Build Channel message
	channel := make([]byte, 0, len(settings)+16)
	// Channel field 1: index (uint32)
	channel = append(channel, 0x08) // field 1, varint
	channel = appendVarint(channel, uint64(ch.Index))
	// Channel field 2: settings (ChannelSettings)
	if len(settings) > 0 {
		channel = append(channel, 0x12) // field 2, length-delimited
		channel = appendVarint(channel, uint64(len(settings)))
		channel = append(channel, settings...)
	}
	// Channel field 3: role (enum)
	if ch.Role > 0 {
		channel = append(channel, 0x18) // field 3, varint
		channel = appendVarint(channel, uint64(ch.Role))
	}

	// Step 3: Build AdminMessage with field 2 = set_channel
	admin := make([]byte, 0, len(channel)+8)
	admin = append(admin, 0x12) // field 2, length-delimited
	admin = appendVarint(admin, uint64(len(channel)))
	admin = append(admin, channel...)

	// Step 4: Build Data submessage (portnum=ADMIN_APP, payload=AdminMessage)
	data := make([]byte, 0, len(admin)+16)
	data = append(data, 0x08) // Data field 1: portnum (varint)
	data = appendVarint(data, uint64(PortNumAdminApp))
	data = append(data, 0x12) // Data field 2: payload (bytes)
	data = appendVarint(data, uint64(len(admin)))
	data = append(data, admin...)
	// Data field 3: want_response = true
	data = append(data, 0x18, 0x01)

	// Step 5: Build MeshPacket (self-addressed admin packet)
	pkt := make([]byte, 0, len(data)+32)
	// MeshPacket field 1: from (uint32)
	pkt = append(pkt, 0x08) // field 1, varint
	pkt = appendVarint(pkt, uint64(myNodeNum))
	// MeshPacket field 2: to (uint32)
	pkt = append(pkt, 0x10) // field 2, varint
	pkt = appendVarint(pkt, uint64(myNodeNum))
	// MeshPacket field 4: decoded (Data, length-delimited)
	pkt = append(pkt, 0x22) // field 4, length-delimited
	pkt = appendVarint(pkt, uint64(len(data)))
	pkt = append(pkt, data...)
	// MeshPacket field 7: want_ack = true
	pkt = append(pkt, 0x38, 0x01) // field 7, varint 1
	// MeshPacket field 9: hop_limit = 3
	pkt = append(pkt, 0x48) // field 9, varint
	pkt = appendVarint(pkt, 3)

	// Step 6: Wrap in ToRadio
	return buildToRadioPacket(pkt)
}

// ============================================================================
// Protobuf Varint Helpers
// ============================================================================

func appendVarint(buf []byte, val uint64) []byte {
	for val >= 0x80 {
		buf = append(buf, byte(val)|0x80)
		val >>= 7
	}
	buf = append(buf, byte(val))
	return buf
}

// ============================================================================
// Port Number Constants and Names
// ============================================================================

const (
	PortNumTextMessage = 1
	PortNumPosition    = 3
	PortNumNodeInfo    = 4
	PortNumAdminApp    = 6
	PortNumSerial      = 64
	PortNumTelemetry   = 67
	PortNumPrivate     = 256
)

func portNumName(pn int) string {
	switch pn {
	case PortNumTextMessage:
		return "TEXT_MESSAGE_APP"
	case PortNumPosition:
		return "POSITION_APP"
	case PortNumNodeInfo:
		return "NODEINFO_APP"
	case PortNumAdminApp:
		return "ADMIN_APP"
	case PortNumTelemetry:
		return "TELEMETRY_APP"
	case PortNumSerial:
		return "SERIAL_APP"
	case PortNumPrivate:
		return "PRIVATE_APP"
	default:
		return fmt.Sprintf("PORTNUM_%d", pn)
	}
}

// ============================================================================
// Hardware Model Names (subset — most common Meshtastic devices)
// ============================================================================

func hwModelName(model int) string {
	names := map[int]string{
		0: "UNSET", 1: "TLORA_V2", 2: "TLORA_V1", 3: "TLORA_V2_1_1P6",
		4: "TBEAM", 5: "HELTEC_V2_0", 6: "TBEAM_V0P7", 7: "T_ECHO",
		8: "TLORA_V1_1P3", 9: "RAK4631", 10: "HELTEC_V2_1",
		11: "HELTEC_V1", 25: "RAK11200", 39: "STATION_G1",
		40: "RAK11310", 41: "SENSELORA_RP2040", 42: "SENSELORA_S3",
		43: "CANARYONE", 44: "RP2040_LORA", 47: "HELTEC_V3",
		48: "HELTEC_WSL_V3", 58: "TBEAM_S3_CORE", 59: "RAK11300",
		60: "WIO_E5", 61: "RADIOMASTER_900_BANDIT", 62: "HELTEC_CAPSULE_SENSOR_V3",
		63: "HELTEC_VISION_MASTER_T190", 64: "HELTEC_VISION_MASTER_E213",
		65: "HELTEC_VISION_MASTER_E290", 66: "HELTEC_MESH_NODE_T114",
	}
	if name, ok := names[model]; ok {
		return name
	}
	return fmt.Sprintf("HW_MODEL_%d", model)
}

// nodeIDStr formats a node number as a Meshtastic-style hex string.
func nodeIDStr(num uint32) string {
	return fmt.Sprintf("!%08x", num)
}

// ============================================================================
// Protobuf Parsers — FromRadio and sub-messages
// ============================================================================

// ProtoFromRadio represents a parsed FromRadio message.
type ProtoFromRadio struct {
	ID               uint32
	Packet           *ProtoMeshPacket
	MyInfo           *ProtoMyNodeInfo
	NodeInfo         *ProtoNodeInfo
	ConfigCompleteID uint32
	ConfigRaw        []byte // Field 5: Config message (raw protobuf)
	ModuleConfigRaw  []byte // Field 7: ModuleConfig message (raw protobuf)
	ChannelRaw       []byte // Field 8: Channel message (raw protobuf)
}

// ProtoMeshPacket represents a parsed MeshPacket.
type ProtoMeshPacket struct {
	From      uint32
	To        uint32
	Channel   uint32
	ID        uint32
	Decoded   *ProtoData
	Encrypted []byte
	RxTime    uint32
	RxSNR     float32
	HopLimit  uint32
	HopStart  uint32
}

// ProtoData represents a parsed Data message (decoded payload).
type ProtoData struct {
	PortNum int
	Payload []byte
}

// ProtoMyNodeInfo holds our local node number.
type ProtoMyNodeInfo struct {
	MyNodeNum uint32
}

// ProtoNodeInfo holds info about a mesh node.
type ProtoNodeInfo struct {
	Num           uint32
	User          *ProtoUser
	Position      *ProtoPosition
	DeviceMetrics *ProtoDeviceMetrics
	SNR           float32
	LastHeard     uint32
}

// ProtoUser holds user identity info.
type ProtoUser struct {
	ID        string
	LongName  string
	ShortName string
	HWModel   uint32
}

// ProtoPosition holds GPS position.
type ProtoPosition struct {
	LatitudeI  int32
	LongitudeI int32
	Altitude   int32
	SatsInView uint32
}

// ProtoDeviceMetrics holds battery/voltage telemetry.
type ProtoDeviceMetrics struct {
	BatteryLevel uint32
	Voltage      float32
}

// parseFromRadio parses raw protobuf bytes into a FromRadio struct.
func parseFromRadio(data []byte) (*ProtoFromRadio, error) {
	fr := &ProtoFromRadio{}
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			return fr, nil // Return what we have
		}
		pos = newPos

		switch fieldNum {
		case 1: // id (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return fr, nil
			}
			fr.ID = uint32(val)
			pos += n

		case 2: // packet (MeshPacket)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return fr, nil
			}
			fr.Packet, _ = parseMeshPacket(val)
			pos = newPos

		case 3: // my_info (MyNodeInfo)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return fr, nil
			}
			fr.MyInfo, _ = parseMyNodeInfo(val)
			pos = newPos

		case 4: // node_info (NodeInfo)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return fr, nil
			}
			fr.NodeInfo, _ = parseNodeInfo(val)
			pos = newPos

		case 6: // config_complete_id (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return fr, nil
			}
			fr.ConfigCompleteID = uint32(val)
			pos += n

		case 5: // config (Config message)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return fr, nil
			}
			fr.ConfigRaw = val
			pos = newPos

		case 7: // module_config (ModuleConfig message)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return fr, nil
			}
			fr.ModuleConfigRaw = val
			pos = newPos

		case 8: // channel (Channel message)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return fr, nil
			}
			fr.ChannelRaw = val
			pos = newPos

		default:
			// Skip unknown fields
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return fr, nil
			}
		}
	}

	return fr, nil
}

func parseMeshPacket(data []byte) (*ProtoMeshPacket, error) {
	pkt := &ProtoMeshPacket{}
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			return pkt, nil
		}
		pos = newPos

		switch fieldNum {
		case 1: // from (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return pkt, nil
			}
			pkt.From = uint32(val)
			pos += n
		case 2: // to (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return pkt, nil
			}
			pkt.To = uint32(val)
			pos += n
		case 3: // channel (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return pkt, nil
			}
			pkt.Channel = uint32(val)
			pos += n
		case 4: // decoded (Data)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return pkt, nil
			}
			pkt.Decoded, _ = parseData(val)
			pos = newPos
		case 5: // encrypted (bytes)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return pkt, nil
			}
			pkt.Encrypted = val
			pos = newPos
		case 6: // id (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return pkt, nil
			}
			pkt.ID = uint32(val)
			pos += n
		case 7: // rx_time (fixed32)
			if pos+4 > len(data) {
				return pkt, nil
			}
			pkt.RxTime = binary.LittleEndian.Uint32(data[pos : pos+4])
			pos += 4
		case 8: // rx_snr (float = fixed32)
			if pos+4 > len(data) {
				return pkt, nil
			}
			bits := binary.LittleEndian.Uint32(data[pos : pos+4])
			pkt.RxSNR = math.Float32frombits(bits)
			pos += 4
		case 9: // hop_limit (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return pkt, nil
			}
			pkt.HopLimit = uint32(val)
			pos += n
		case 12: // hop_start (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return pkt, nil
			}
			pkt.HopStart = uint32(val)
			pos += n
		default:
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return pkt, nil
			}
		}
	}

	return pkt, nil
}

func parseData(data []byte) (*ProtoData, error) {
	d := &ProtoData{}
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			return d, nil
		}
		pos = newPos

		switch fieldNum {
		case 1: // portnum (enum = varint)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return d, nil
			}
			d.PortNum = int(val)
			pos += n
		case 2: // payload (bytes)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return d, nil
			}
			d.Payload = val
			pos = newPos
		default:
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return d, nil
			}
		}
	}

	return d, nil
}

func parseMyNodeInfo(data []byte) (*ProtoMyNodeInfo, error) {
	info := &ProtoMyNodeInfo{}
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			return info, nil
		}
		pos = newPos

		switch fieldNum {
		case 1: // my_node_num (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return info, nil
			}
			info.MyNodeNum = uint32(val)
			pos += n
		default:
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return info, nil
			}
		}
	}

	return info, nil
}

func parseNodeInfo(data []byte) (*ProtoNodeInfo, error) {
	info := &ProtoNodeInfo{}
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			return info, nil
		}
		pos = newPos

		switch fieldNum {
		case 1: // num (uint32)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return info, nil
			}
			info.Num = uint32(val)
			pos += n
		case 2: // user (User)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return info, nil
			}
			info.User, _ = parseUser(val)
			pos = newPos
		case 4: // position (Position)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return info, nil
			}
			info.Position, _ = parsePosition(val)
			pos = newPos
		case 6: // snr (float = fixed32)
			if pos+4 > len(data) {
				return info, nil
			}
			bits := binary.LittleEndian.Uint32(data[pos : pos+4])
			info.SNR = math.Float32frombits(bits)
			pos += 4
		case 7: // last_heard (fixed32)
			if pos+4 > len(data) {
				return info, nil
			}
			info.LastHeard = binary.LittleEndian.Uint32(data[pos : pos+4])
			pos += 4
		case 8: // device_metrics (DeviceMetrics)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return info, nil
			}
			info.DeviceMetrics, _ = parseDeviceMetricsProto(val)
			pos = newPos
		default:
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return info, nil
			}
		}
	}

	return info, nil
}

func parseUser(data []byte) (*ProtoUser, error) {
	user := &ProtoUser{}
	pos := 0

	for pos < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, pos)
		if err != nil {
			return user, nil
		}
		pos = newPos

		switch fieldNum {
		case 1: // id (string)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return user, nil
			}
			user.ID = string(val)
			pos = newPos
		case 2: // long_name (string)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return user, nil
			}
			user.LongName = string(val)
			pos = newPos
		case 3: // short_name (string)
			val, newPos, err := readLengthDelimited(data, pos)
			if err != nil {
				return user, nil
			}
			user.ShortName = string(val)
			pos = newPos
		case 6: // hw_model (enum = varint)
			val, n := readVarint(data, pos)
			if n <= 0 {
				return user, nil
			}
			user.HWModel = uint32(val)
			pos += n
		default:
			pos = skipField(data, pos, wireType)
			if pos < 0 {
				return user, nil
			}
		}
	}

	return user, nil
}

func parsePosition(data []byte) (*ProtoPosition, error) {
	pos := &ProtoPosition{}
	offset := 0

	for offset < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, offset)
		if err != nil {
			return pos, nil
		}
		offset = newPos

		switch fieldNum {
		case 1: // latitude_i (sfixed32)
			if offset+4 > len(data) {
				return pos, nil
			}
			pos.LatitudeI = int32(binary.LittleEndian.Uint32(data[offset : offset+4]))
			offset += 4
		case 2: // longitude_i (sfixed32)
			if offset+4 > len(data) {
				return pos, nil
			}
			pos.LongitudeI = int32(binary.LittleEndian.Uint32(data[offset : offset+4]))
			offset += 4
		case 3: // altitude (int32 = varint, zigzag encoded)
			val, n := readVarint(data, offset)
			if n <= 0 {
				return pos, nil
			}
			pos.Altitude = int32(val)
			offset += n
		case 9: // sats_in_view (uint32)
			val, n := readVarint(data, offset)
			if n <= 0 {
				return pos, nil
			}
			pos.SatsInView = uint32(val)
			offset += n
		default:
			offset = skipField(data, offset, wireType)
			if offset < 0 {
				return pos, nil
			}
		}
	}

	return pos, nil
}

func parseDeviceMetrics(data []byte) (*ProtoDeviceMetrics, error) {
	// Telemetry message wraps DeviceMetrics — field 1 is device_metrics submessage
	// Try parsing as Telemetry first (outer message), then as DeviceMetrics
	dm := &ProtoDeviceMetrics{}
	offset := 0

	for offset < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, offset)
		if err != nil {
			return dm, nil
		}
		offset = newPos

		switch fieldNum {
		case 1:
			if wireType == 2 {
				// Length-delimited: this is the Telemetry wrapper, field 1 = device_metrics
				val, newPos, err := readLengthDelimited(data, offset)
				if err != nil {
					return dm, nil
				}
				offset = newPos
				return parseDeviceMetricsProto(val)
			}
			// Varint: this IS device_metrics, field 1 = battery_level
			val, n := readVarint(data, offset)
			if n <= 0 {
				return dm, nil
			}
			dm.BatteryLevel = uint32(val)
			offset += n
		case 2: // voltage (float = fixed32)
			if offset+4 > len(data) {
				return dm, nil
			}
			bits := binary.LittleEndian.Uint32(data[offset : offset+4])
			dm.Voltage = math.Float32frombits(bits)
			offset += 4
		default:
			offset = skipField(data, offset, wireType)
			if offset < 0 {
				return dm, nil
			}
		}
	}

	return dm, nil
}

func parseDeviceMetricsProto(data []byte) (*ProtoDeviceMetrics, error) {
	dm := &ProtoDeviceMetrics{}
	offset := 0

	for offset < len(data) {
		fieldNum, wireType, newPos, err := readTag(data, offset)
		if err != nil {
			return dm, nil
		}
		offset = newPos

		switch fieldNum {
		case 1: // battery_level (uint32)
			val, n := readVarint(data, offset)
			if n <= 0 {
				return dm, nil
			}
			dm.BatteryLevel = uint32(val)
			offset += n
		case 2: // voltage (float = fixed32)
			if offset+4 > len(data) {
				return dm, nil
			}
			bits := binary.LittleEndian.Uint32(data[offset : offset+4])
			dm.Voltage = math.Float32frombits(bits)
			offset += 4
		default:
			offset = skipField(data, offset, wireType)
			if offset < 0 {
				return dm, nil
			}
		}
	}

	return dm, nil
}

// ============================================================================
// Protobuf Wire Format Primitives
// ============================================================================

// readTag reads a protobuf field tag and returns (fieldNumber, wireType, newOffset, error).
func readTag(data []byte, pos int) (uint32, int, int, error) {
	val, n := readVarint(data, pos)
	if n <= 0 {
		return 0, 0, pos, fmt.Errorf("invalid tag at position %d", pos)
	}
	fieldNum := uint32(val >> 3)
	wireType := int(val & 0x07)
	return fieldNum, wireType, pos + n, nil
}

// readVarint reads a protobuf varint and returns (value, bytesConsumed).
// Returns n=0 if the varint is malformed or data is exhausted.
func readVarint(data []byte, pos int) (uint64, int) {
	var val uint64
	var shift uint
	for i := 0; i < 10; i++ {
		if pos+i >= len(data) {
			return 0, 0
		}
		b := data[pos+i]
		val |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return val, i + 1
		}
		shift += 7
	}
	return 0, 0 // Varint too long
}

// readLengthDelimited reads a length-delimited field and returns (data, newOffset, error).
func readLengthDelimited(data []byte, pos int) ([]byte, int, error) {
	length, n := readVarint(data, pos)
	if n <= 0 {
		return nil, pos, fmt.Errorf("invalid length at position %d", pos)
	}
	pos += n
	end := pos + int(length)
	if end > len(data) || length > 65536 {
		return nil, pos, fmt.Errorf("length-delimited field exceeds data bounds")
	}
	return data[pos:end], end, nil
}

// skipField advances past a field based on its wire type.
// Returns -1 if the field cannot be skipped.
func skipField(data []byte, pos int, wireType int) int {
	switch wireType {
	case 0: // Varint
		_, n := readVarint(data, pos)
		if n <= 0 {
			return -1
		}
		return pos + n
	case 1: // 64-bit
		if pos+8 > len(data) {
			return -1
		}
		return pos + 8
	case 2: // Length-delimited
		_, newPos, err := readLengthDelimited(data, pos)
		if err != nil {
			return -1
		}
		return newPos
	case 5: // 32-bit
		if pos+4 > len(data) {
			return -1
		}
		return pos + 4
	default:
		return -1
	}
}
