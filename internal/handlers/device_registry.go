package handlers

import (
	"sync"
	"time"
)

// ============================================================================
// Device Registry — Central USB Device Inventory
// ============================================================================

// DeviceClass classifies a USB device by its primary function.
type DeviceClass string

const (
	ClassSerial   DeviceClass = "serial"
	ClassCellular DeviceClass = "cellular"
	ClassNetwork  DeviceClass = "network"
	ClassStorage  DeviceClass = "storage"
	ClassCamera   DeviceClass = "camera"
	ClassAudio    DeviceClass = "audio"
	ClassSDR      DeviceClass = "sdr"
	ClassHID      DeviceClass = "hid"
	ClassHub      DeviceClass = "hub"
	ClassUnknown  DeviceClass = "unknown"
)

// DeviceRole sub-classifies serial devices by their protocol/function.
type DeviceRole string

const (
	RoleMeshtastic DeviceRole = "meshtastic"
	RoleIridium    DeviceRole = "iridium"
	RoleGPS        DeviceRole = "gps"
	RoleNone       DeviceRole = ""
)

// DeviceState tracks a device's lifecycle from detection through connection.
type DeviceState string

const (
	StateDetected     DeviceState = "detected"
	StateIdentifying  DeviceState = "identifying"
	StateReady        DeviceState = "ready"
	StateConnected    DeviceState = "connected"
	StateDisconnected DeviceState = "disconnected"
	StateRemoved      DeviceState = "removed"
)

// DeviceEntry holds all metadata for a single USB device.
type DeviceEntry struct {
	BusNum  int         `json:"bus"`
	DevNum  int         `json:"dev"`
	VIDPID  string      `json:"vid_pid"`
	Vendor  string      `json:"vendor,omitempty"`
	Product string      `json:"product,omitempty"`
	Serial  string      `json:"serial,omitempty"`
	SysPath string      `json:"sys_path"`
	Class   DeviceClass `json:"class"`
	Role    DeviceRole  `json:"role,omitempty"`
	State   DeviceState `json:"state"`
	DevPath string      `json:"dev_path,omitempty"`
	Error   string      `json:"error,omitempty"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// DeviceRegistry is a thread-safe registry of all USB devices.
type DeviceRegistry struct {
	mu      sync.RWMutex
	devices map[string]*DeviceEntry // keyed by sysfs path

	// Serial port claims: devPath ("/dev/ttyUSB0") → role
	claimsMu sync.Mutex
	claims   map[string]DeviceRole
}

// NewDeviceRegistry creates a new empty registry.
func NewDeviceRegistry() *DeviceRegistry {
	return &DeviceRegistry{
		devices: make(map[string]*DeviceEntry),
		claims:  make(map[string]DeviceRole),
	}
}

// Upsert adds or updates a device in the registry.
// Returns true if the device was newly added (not an update).
func (r *DeviceRegistry) Upsert(sysPath string, entry DeviceEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, exists := r.devices[sysPath]
	if exists {
		existing.LastSeen = entry.LastSeen
		existing.DevPath = entry.DevPath
		if entry.VIDPID != "" {
			existing.VIDPID = entry.VIDPID
		}
		if entry.Vendor != "" {
			existing.Vendor = entry.Vendor
		}
		if entry.Product != "" {
			existing.Product = entry.Product
		}
		if entry.Serial != "" {
			existing.Serial = entry.Serial
		}
		if entry.Class != ClassUnknown {
			existing.Class = entry.Class
		}
		if entry.BusNum != 0 {
			existing.BusNum = entry.BusNum
		}
		if entry.DevNum != 0 {
			existing.DevNum = entry.DevNum
		}
		return false
	}

	e := entry
	e.SysPath = sysPath
	r.devices[sysPath] = &e
	return true
}

// Remove removes a device from the registry and returns it (for event emission).
func (r *DeviceRegistry) Remove(sysPath string) *DeviceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, exists := r.devices[sysPath]
	if !exists {
		return nil
	}

	// Release any serial port claim
	if entry.DevPath != "" {
		r.claimsMu.Lock()
		delete(r.claims, entry.DevPath)
		r.claimsMu.Unlock()
	}

	entry.State = StateRemoved
	delete(r.devices, sysPath)
	return entry
}

// ClaimSerialPort atomically claims a serial port for a specific role.
// Returns true if the claim succeeded (port was unclaimed).
func (r *DeviceRegistry) ClaimSerialPort(devPath string, role DeviceRole) bool {
	r.claimsMu.Lock()
	defer r.claimsMu.Unlock()

	if _, claimed := r.claims[devPath]; claimed {
		return false
	}
	r.claims[devPath] = role
	return true
}

// ReleaseSerialPort releases a serial port claim.
func (r *DeviceRegistry) ReleaseSerialPort(devPath string) {
	r.claimsMu.Lock()
	defer r.claimsMu.Unlock()
	delete(r.claims, devPath)
}

// GetSerialPortRole returns the role that has claimed a serial port, or empty string.
func (r *DeviceRegistry) GetSerialPortRole(devPath string) DeviceRole {
	r.claimsMu.Lock()
	defer r.claimsMu.Unlock()
	return r.claims[devPath]
}

// FindByClass returns all devices of a given class.
func (r *DeviceRegistry) FindByClass(class DeviceClass) []*DeviceEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*DeviceEntry
	for _, entry := range r.devices {
		if entry.Class == class {
			e := *entry
			result = append(result, &e)
		}
	}
	return result
}

// FindByRole returns the first device with a given role, or nil.
func (r *DeviceRegistry) FindByRole(role DeviceRole) *DeviceEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, entry := range r.devices {
		if entry.Role == role {
			e := *entry
			return &e
		}
	}
	return nil
}

// ListAll returns a snapshot of all devices.
func (r *DeviceRegistry) ListAll() []*DeviceEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*DeviceEntry, 0, len(r.devices))
	for _, entry := range r.devices {
		e := *entry
		result = append(result, &e)
	}
	return result
}

// SetState updates the state of a device identified by sysPath.
func (r *DeviceRegistry) SetState(sysPath string, state DeviceState) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, exists := r.devices[sysPath]; exists {
		entry.State = state
	}
}

// SetRole updates the role of a device identified by sysPath.
func (r *DeviceRegistry) SetRole(sysPath string, role DeviceRole) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, exists := r.devices[sysPath]; exists {
		entry.Role = role
	}
}

// FindByDevPath returns the device entry for a given /dev path, or nil.
func (r *DeviceRegistry) FindByDevPath(devPath string) *DeviceEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, entry := range r.devices {
		if entry.DevPath == devPath {
			e := *entry
			return &e
		}
	}
	return nil
}

// Reconcile compares the current set of sysfs paths against the registry.
// Returns entries that were removed (present in registry but not in currentSysPaths).
func (r *DeviceRegistry) Reconcile(currentSysPaths map[string]bool) []*DeviceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	var removed []*DeviceEntry
	for sysPath, entry := range r.devices {
		if !currentSysPaths[sysPath] {
			entry.State = StateRemoved
			removed = append(removed, entry)

			// Release any serial port claim
			if entry.DevPath != "" {
				r.claimsMu.Lock()
				delete(r.claims, entry.DevPath)
				r.claimsMu.Unlock()
			}

			delete(r.devices, sysPath)
		}
	}
	return removed
}
