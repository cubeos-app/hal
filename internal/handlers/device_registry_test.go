package handlers

import (
	"sync"
	"testing"
	"time"
)

func TestDeviceRegistry_UpsertAndList(t *testing.T) {
	reg := NewDeviceRegistry()

	now := time.Now()
	entry := DeviceEntry{
		BusNum:    1,
		DevNum:    2,
		VIDPID:    "303a:1001",
		Vendor:    "Espressif",
		Product:   "ESP32-S3",
		SysPath:   "/sys/bus/usb/devices/1-1",
		Class:     ClassSerial,
		State:     StateDetected,
		DevPath:   "/dev/ttyACM0",
		FirstSeen: now,
		LastSeen:  now,
	}

	isNew := reg.Upsert("/sys/bus/usb/devices/1-1", entry)
	if !isNew {
		t.Error("expected Upsert to return true for new entry")
	}

	all := reg.ListAll()
	if len(all) != 1 {
		t.Fatalf("expected 1 device, got %d", len(all))
	}
	if all[0].VIDPID != "303a:1001" {
		t.Errorf("expected VID:PID 303a:1001, got %s", all[0].VIDPID)
	}

	// Update existing
	entry.LastSeen = now.Add(time.Minute)
	isNew = reg.Upsert("/sys/bus/usb/devices/1-1", entry)
	if isNew {
		t.Error("expected Upsert to return false for existing entry")
	}

	all = reg.ListAll()
	if len(all) != 1 {
		t.Fatalf("expected 1 device after update, got %d", len(all))
	}
}

func TestDeviceRegistry_Remove(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	reg.Upsert("/sys/bus/usb/devices/1-1", DeviceEntry{
		VIDPID:    "303a:1001",
		Class:     ClassSerial,
		State:     StateDetected,
		DevPath:   "/dev/ttyACM0",
		FirstSeen: now,
		LastSeen:  now,
	})

	removed := reg.Remove("/sys/bus/usb/devices/1-1")
	if removed == nil {
		t.Fatal("expected removed entry, got nil")
	}
	if removed.State != StateRemoved {
		t.Errorf("expected state Removed, got %s", removed.State)
	}

	all := reg.ListAll()
	if len(all) != 0 {
		t.Errorf("expected 0 devices after remove, got %d", len(all))
	}

	// Remove non-existent
	removed = reg.Remove("/sys/bus/usb/devices/999")
	if removed != nil {
		t.Error("expected nil for non-existent remove")
	}
}

func TestDeviceRegistry_ClaimAndRelease(t *testing.T) {
	reg := NewDeviceRegistry()

	// Claim unclaimed port
	ok := reg.ClaimSerialPort("/dev/ttyUSB0", RoleMeshtastic)
	if !ok {
		t.Error("expected successful claim")
	}

	// Second claim should fail
	ok = reg.ClaimSerialPort("/dev/ttyUSB0", RoleIridium)
	if ok {
		t.Error("expected claim to fail on already-claimed port")
	}

	// Check role
	role := reg.GetSerialPortRole("/dev/ttyUSB0")
	if role != RoleMeshtastic {
		t.Errorf("expected role meshtastic, got %s", role)
	}

	// Release
	reg.ReleaseSerialPort("/dev/ttyUSB0")

	// Now claim again with different role
	ok = reg.ClaimSerialPort("/dev/ttyUSB0", RoleIridium)
	if !ok {
		t.Error("expected successful claim after release")
	}

	role = reg.GetSerialPortRole("/dev/ttyUSB0")
	if role != RoleIridium {
		t.Errorf("expected role iridium after re-claim, got %s", role)
	}
}

func TestDeviceRegistry_FindByClass(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	reg.Upsert("/sys/bus/usb/devices/1-1", DeviceEntry{
		VIDPID: "303a:1001", Class: ClassSerial, State: StateDetected,
		FirstSeen: now, LastSeen: now,
	})
	reg.Upsert("/sys/bus/usb/devices/1-2", DeviceEntry{
		VIDPID: "0bda:2832", Class: ClassSDR, State: StateDetected,
		FirstSeen: now, LastSeen: now,
	})
	reg.Upsert("/sys/bus/usb/devices/1-3", DeviceEntry{
		VIDPID: "1234:5678", Class: ClassSerial, State: StateDetected,
		FirstSeen: now, LastSeen: now,
	})

	serial := reg.FindByClass(ClassSerial)
	if len(serial) != 2 {
		t.Errorf("expected 2 serial devices, got %d", len(serial))
	}

	sdr := reg.FindByClass(ClassSDR)
	if len(sdr) != 1 {
		t.Errorf("expected 1 SDR device, got %d", len(sdr))
	}

	cameras := reg.FindByClass(ClassCamera)
	if len(cameras) != 0 {
		t.Errorf("expected 0 camera devices, got %d", len(cameras))
	}
}

func TestDeviceRegistry_FindByRole(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	reg.Upsert("/sys/bus/usb/devices/1-1", DeviceEntry{
		VIDPID: "303a:1001", Class: ClassSerial, Role: RoleMeshtastic,
		State: StateConnected, DevPath: "/dev/ttyACM0",
		FirstSeen: now, LastSeen: now,
	})
	reg.Upsert("/sys/bus/usb/devices/1-2", DeviceEntry{
		VIDPID: "0403:6001", Class: ClassSerial, Role: RoleIridium,
		State: StateConnected, DevPath: "/dev/ttyUSB0",
		FirstSeen: now, LastSeen: now,
	})

	mesh := reg.FindByRole(RoleMeshtastic)
	if mesh == nil {
		t.Fatal("expected meshtastic device, got nil")
	}
	if mesh.DevPath != "/dev/ttyACM0" {
		t.Errorf("expected /dev/ttyACM0, got %s", mesh.DevPath)
	}

	gps := reg.FindByRole(RoleGPS)
	if gps != nil {
		t.Error("expected nil for GPS role, got a device")
	}
}

func TestDeviceRegistry_Reconcile(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	reg.Upsert("/sys/bus/usb/devices/1-1", DeviceEntry{
		VIDPID: "303a:1001", Class: ClassSerial, State: StateDetected,
		DevPath: "/dev/ttyACM0", FirstSeen: now, LastSeen: now,
	})
	reg.Upsert("/sys/bus/usb/devices/1-2", DeviceEntry{
		VIDPID: "0bda:2832", Class: ClassSDR, State: StateDetected,
		FirstSeen: now, LastSeen: now,
	})
	reg.Upsert("/sys/bus/usb/devices/1-3", DeviceEntry{
		VIDPID: "1234:5678", Class: ClassStorage, State: StateDetected,
		FirstSeen: now, LastSeen: now,
	})

	// Claim a serial port
	reg.ClaimSerialPort("/dev/ttyACM0", RoleMeshtastic)

	// Reconcile — only 1-2 remains
	currentPaths := map[string]bool{
		"/sys/bus/usb/devices/1-2": true,
	}
	removed := reg.Reconcile(currentPaths)
	if len(removed) != 2 {
		t.Fatalf("expected 2 removed devices, got %d", len(removed))
	}

	// Verify claim was released for the removed serial device
	role := reg.GetSerialPortRole("/dev/ttyACM0")
	if role != RoleNone {
		t.Errorf("expected claim released after reconcile, got role %s", role)
	}

	all := reg.ListAll()
	if len(all) != 1 {
		t.Errorf("expected 1 device remaining, got %d", len(all))
	}
}

func TestDeviceRegistry_FindByDevPath(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	reg.Upsert("/sys/bus/usb/devices/1-1", DeviceEntry{
		VIDPID: "303a:1001", Class: ClassSerial, DevPath: "/dev/ttyACM0",
		FirstSeen: now, LastSeen: now,
	})

	found := reg.FindByDevPath("/dev/ttyACM0")
	if found == nil {
		t.Fatal("expected device for /dev/ttyACM0")
	}
	if found.VIDPID != "303a:1001" {
		t.Errorf("expected 303a:1001, got %s", found.VIDPID)
	}

	notFound := reg.FindByDevPath("/dev/ttyUSB9")
	if notFound != nil {
		t.Error("expected nil for non-existent dev path")
	}
}

func TestDeviceRegistry_SetState(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	reg.Upsert("/sys/bus/usb/devices/1-1", DeviceEntry{
		VIDPID: "303a:1001", Class: ClassSerial, State: StateDetected,
		FirstSeen: now, LastSeen: now,
	})

	reg.SetState("/sys/bus/usb/devices/1-1", StateConnected)

	all := reg.ListAll()
	if len(all) != 1 || all[0].State != StateConnected {
		t.Errorf("expected state connected, got %s", all[0].State)
	}
}

func TestDeviceRegistry_ConcurrentAccess(t *testing.T) {
	reg := NewDeviceRegistry()
	now := time.Now()

	var wg sync.WaitGroup
	const n = 100

	// Concurrent Upserts
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			path := "/sys/bus/usb/devices/1-" + time.Now().Format("150405.000000000")
			reg.Upsert(path, DeviceEntry{
				VIDPID: "303a:1001", Class: ClassSerial, State: StateDetected,
				FirstSeen: now, LastSeen: now,
			})
		}(i)
	}
	wg.Wait()

	// Concurrent Claims/Releases
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			reg.ClaimSerialPort("/dev/ttyUSB0", RoleMeshtastic)
		}()
		go func() {
			defer wg.Done()
			reg.ReleaseSerialPort("/dev/ttyUSB0")
		}()
	}
	wg.Wait()

	// Concurrent ListAll + Reconcile
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			reg.ListAll()
		}()
		go func() {
			defer wg.Done()
			reg.Reconcile(map[string]bool{})
		}()
	}
	wg.Wait()
}

func TestIsKnownMeshtasticVIDPID(t *testing.T) {
	tests := []struct {
		vidpid string
		want   bool
	}{
		{"303a:1001", true},
		{"1a86:55d4", true},
		{"1a86:7523", true},
		{"10c4:ea60", true},
		{"239a:8029", true},
		{"1915:520f", true},
		{"0000:0000", false},
		{"1546:01a7", false}, // GPS
		{"", false},
	}
	for _, tt := range tests {
		if got := IsKnownMeshtasticVIDPID(tt.vidpid); got != tt.want {
			t.Errorf("IsKnownMeshtasticVIDPID(%q) = %v, want %v", tt.vidpid, got, tt.want)
		}
	}
}

func TestIsKnownGPSVIDPID(t *testing.T) {
	tests := []struct {
		vidpid string
		want   bool
	}{
		{"1546:01a7", true},
		{"1546:01a8", true},
		{"067b:2303", true},
		{"303a:1001", false}, // Meshtastic
		{"0000:0000", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsKnownGPSVIDPID(tt.vidpid); got != tt.want {
			t.Errorf("IsKnownGPSVIDPID(%q) = %v, want %v", tt.vidpid, got, tt.want)
		}
	}
}
