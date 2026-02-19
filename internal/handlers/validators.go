package handlers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"net/http"
)

// --- Regex patterns (compiled once) ---

var (
	reServiceName    = regexp.MustCompile(`^[a-zA-Z0-9@._:-]+$`)
	reInterfaceName  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$`)
	reSSID           = regexp.MustCompile(`^[^\x00]{1,32}$`)
	reVPNName        = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,15}$`)
	reShareName      = regexp.MustCompile(`^[a-zA-Z0-9_.$-]+$`)
	reDeviceName     = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	reBlockDevice    = regexp.MustCompile(`^/dev/[a-zA-Z0-9]+$`)
	re1WireDeviceID  = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{12}$`)
	reI2CAddress     = regexp.MustCompile(`^0x[0-9a-fA-F]{2}$`)
	reI2CRegister    = regexp.MustCompile(`^0x[0-9a-fA-F]{2}$`)
	reGPIOChipName   = regexp.MustCompile(`^gpiochip[0-9]+$`)
	reSerialPort     = regexp.MustCompile(`^/dev/(tty[a-zA-Z0-9]+|serial[a-zA-Z0-9/]+)$`)
	reAPN            = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,100}$`)
	reMeshtasticNode = regexp.MustCompile(`^![0-9a-fA-F]{1,8}$`)
	reMixerControl   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9 _-]{0,63}$`)
	reHostname       = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
)

// --- Service allowlist ---

// ErrServiceNotInAllowlist is returned when a service name is valid but not
// in the allowed list. HAL service handlers return HTTP 501 for this error
// (to distinguish from 400 for invalid names), which the API proxies through
// to the dashboard as "not available on this hardware". (B70 fix)
var ErrServiceNotInAllowlist = errors.New("service not in allowlist")

// serviceAllowlist contains the systemd services that can be managed via HAL.
// Add services here as needed — anything not on the list is rejected.
var serviceAllowlist = map[string]bool{
	"pihole-FTL":        true,
	"hostapd":           true,
	"dnsmasq":           true,
	"NetworkManager":    true,
	"systemd-resolved":  true,
	"systemd-timesyncd": true,
	"wpa_supplicant":    true,
	"bluetooth":         true,
	"gpsd":              true,
	"ModemManager":      true,
	"tor":               true,
	"docker":            true,
	"ssh":               true,
	"avahi-daemon":      true,
	"cron":              true,
	"systemd-journald":  true,
	"wg-quick@wg0":      true,
	"wg-quick@wg1":      true,
	"openvpn@client":    true,
	"openvpn@server":    true,
}

// --- Validator functions ---

// validateServiceName checks that the service name is safe and in the allowlist.
func validateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name is required")
	}
	if len(name) > 256 {
		return fmt.Errorf("service name too long (max 256)")
	}
	if !reServiceName.MatchString(name) {
		return fmt.Errorf("invalid service name")
	}
	if !serviceAllowlist[name] {
		return ErrServiceNotInAllowlist
	}
	return nil
}

// validateInterfaceName validates a Linux network interface name.
func validateInterfaceName(name string) error {
	if name == "" {
		return fmt.Errorf("interface name is required")
	}
	if !reInterfaceName.MatchString(name) {
		return fmt.Errorf("invalid interface name")
	}
	return nil
}

// validateMACAddress validates a MAC address string.
func validateMACAddress(mac string) error {
	if mac == "" {
		return fmt.Errorf("MAC address is required")
	}
	if _, err := net.ParseMAC(mac); err != nil {
		return fmt.Errorf("invalid MAC address")
	}
	return nil
}

// validateSSID validates a WiFi SSID.
func validateSSID(ssid string) error {
	if ssid == "" {
		return fmt.Errorf("SSID is required")
	}
	if len(ssid) > 32 {
		return fmt.Errorf("SSID too long (max 32)")
	}
	if strings.ContainsRune(ssid, 0) {
		return fmt.Errorf("SSID contains null byte")
	}
	return nil
}

// validateWiFiPassword validates a WiFi password (WPA2 personal).
func validateWiFiPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 63 {
		return fmt.Errorf("WiFi password must be 8-63 characters")
	}
	if strings.ContainsRune(pw, 0) {
		return fmt.Errorf("password contains null byte")
	}
	return nil
}

// validateCIDROrIP validates an IP address or CIDR notation string.
func validateCIDROrIP(s string) error {
	if s == "" {
		return fmt.Errorf("IP/CIDR is required")
	}
	if net.ParseIP(s) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(s); err == nil {
		return nil
	}
	return fmt.Errorf("invalid IP address or CIDR")
}

// validateFirewallChain validates an iptables chain name against an allowlist.
func validateFirewallChain(chain string) error {
	allowed := map[string]bool{"INPUT": true, "FORWARD": true, "OUTPUT": true}
	if !allowed[chain] {
		return fmt.Errorf("invalid firewall chain (must be INPUT, FORWARD, or OUTPUT)")
	}
	return nil
}

// validateFirewallAction validates an iptables action/target.
func validateFirewallAction(action string) error {
	allowed := map[string]bool{"ACCEPT": true, "DROP": true, "REJECT": true, "LOG": true}
	if !allowed[action] {
		return fmt.Errorf("invalid firewall action (must be ACCEPT, DROP, REJECT, or LOG)")
	}
	return nil
}

// validateFirewallProtocol validates an iptables protocol.
func validateFirewallProtocol(proto string) error {
	allowed := map[string]bool{"tcp": true, "udp": true, "icmp": true, "all": true}
	if !allowed[strings.ToLower(proto)] {
		return fmt.Errorf("invalid protocol (must be tcp, udp, icmp, or all)")
	}
	return nil
}

// validateRuleNumber validates an iptables rule number (positive integer).
func validateRuleNumber(num string) error {
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 {
		return fmt.Errorf("invalid rule number (must be positive integer)")
	}
	return nil
}

// validatePort validates a port number string (1-65535).
func validatePort(port string) error {
	if port == "" {
		return fmt.Errorf("port is required")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port number (must be 1-65535)")
	}
	return nil
}

// isValidVPNName checks if a VPN config name is safe.
func isValidVPNName(name string) bool {
	return reVPNName.MatchString(name)
}

// validateHostnameOrIP validates a hostname or IP address.
func validateHostnameOrIP(host string) error {
	if host == "" {
		return fmt.Errorf("hostname or IP is required")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if len(host) > 253 {
		return fmt.Errorf("hostname too long")
	}
	if !reHostname.MatchString(host) {
		return fmt.Errorf("invalid hostname")
	}
	return nil
}

// validateShareName validates an SMB/NFS share name.
func validateShareName(share string) error {
	if share == "" {
		return fmt.Errorf("share name is required")
	}
	if len(share) > 255 {
		return fmt.Errorf("share name too long")
	}
	if !reShareName.MatchString(share) {
		return fmt.Errorf("invalid share name")
	}
	return nil
}

// validateMountpoint validates that a mountpoint is under /mnt/ or /media/ only.
func validateMountpoint(path string) error {
	if path == "" {
		return fmt.Errorf("mountpoint is required")
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, "/mnt/") && !strings.HasPrefix(clean, "/media/") {
		return fmt.Errorf("mountpoint must be under /mnt/ or /media/")
	}
	// Block if clean path equals just "/mnt" or "/media" (no subdirectory)
	if clean == "/mnt" || clean == "/media" {
		return fmt.Errorf("mountpoint must be a subdirectory of /mnt/ or /media/")
	}
	return nil
}

// validateExportPath validates an NFS export path.
func validateExportPath(path string) error {
	if path == "" {
		return fmt.Errorf("export path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("export path must start with /")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("export path must not contain ..")
	}
	return nil
}

// validateSMBVersion validates an SMB protocol version.
func validateSMBVersion(ver string) error {
	allowed := map[string]bool{"": true, "1.0": true, "2.0": true, "2.1": true, "3.0": true, "3.1.1": true}
	if !allowed[ver] {
		return fmt.Errorf("invalid SMB version")
	}
	return nil
}

// validateSMBCredentialField checks that an SMB credential field (username, password, domain)
// does not contain characters that could inject mount options (commas, newlines, null bytes).
func validateSMBCredentialField(field, name string) error {
	if strings.ContainsAny(field, ",\n\r\x00") {
		return fmt.Errorf("%s contains disallowed characters", name)
	}
	if len(field) > 256 {
		return fmt.Errorf("%s too long (max 256)", name)
	}
	return nil
}

// validateNFSVersion validates an NFS protocol version string.
func validateNFSVersion(ver string) error {
	allowed := map[string]bool{"": true, "3": true, "4": true, "4.1": true, "4.2": true}
	if !allowed[ver] {
		return fmt.Errorf("invalid NFS version")
	}
	return nil
}

// validateNFSOptions validates NFS mount options against an allowlist.
func validateNFSOptions(opts string) error {
	if opts == "" {
		return nil
	}
	allowed := map[string]bool{
		"rw": true, "ro": true, "sync": true, "async": true,
		"noatime": true, "atime": true, "nodiratime": true,
		"hard": true, "soft": true, "intr": true, "nointr": true,
		"tcp": true, "udp": true, "nolock": true, "lock": true,
		"nfsvers=3": true, "nfsvers=4": true, "nfsvers=4.1": true, "nfsvers=4.2": true,
		"vers=3": true, "vers=4": true, "vers=4.1": true, "vers=4.2": true,
		"nosuid": true, "nodev": true, "noexec": true,
	}
	for _, opt := range strings.Split(opts, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		if !allowed[opt] {
			// Allow timeo=N and retrans=N patterns
			if strings.HasPrefix(opt, "timeo=") || strings.HasPrefix(opt, "retrans=") ||
				strings.HasPrefix(opt, "rsize=") || strings.HasPrefix(opt, "wsize=") {
				parts := strings.SplitN(opt, "=", 2)
				if len(parts) == 2 {
					if _, err := strconv.Atoi(parts[1]); err == nil {
						continue
					}
				}
			}
			return fmt.Errorf("disallowed NFS option: %s", opt)
		}
	}
	return nil
}

// validateDeviceName validates a storage device name (e.g., "sda", "nvme0n1").
func validateDeviceName(name string) error {
	if name == "" {
		return fmt.Errorf("device name is required")
	}
	if len(name) > 32 {
		return fmt.Errorf("device name too long")
	}
	if !reDeviceName.MatchString(name) {
		return fmt.Errorf("invalid device name")
	}
	return nil
}

// validateBlockDevice validates a block device path (e.g., "/dev/sda1").
func validateBlockDevice(path string) error {
	if path == "" {
		return fmt.Errorf("block device path is required")
	}
	if !reBlockDevice.MatchString(path) {
		return fmt.Errorf("invalid block device path (expected /dev/[name])")
	}
	return nil
}

// validateMountOptions validates filesystem mount options against an allowlist.
func validateMountOptions(opts string) error {
	if opts == "" {
		return nil
	}
	allowed := map[string]bool{
		"rw": true, "ro": true, "noatime": true, "sync": true,
		"nosuid": true, "nodev": true, "noexec": true, "relatime": true,
	}
	for _, opt := range strings.Split(opts, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		if !allowed[opt] {
			return fmt.Errorf("disallowed mount option: %s", opt)
		}
	}
	return nil
}

// validateLogLevel validates a syslog-style log level.
func validateLogLevel(level string) error {
	allowed := map[string]bool{
		"emerg": true, "alert": true, "crit": true, "err": true,
		"warn": true, "warning": true, "notice": true, "info": true, "debug": true,
	}
	if !allowed[level] {
		return fmt.Errorf("invalid log level")
	}
	return nil
}

// validateUnitName validates a systemd unit name for journalctl -u filtering.
// Unlike validateServiceName, this does not check the allowlist — it only ensures
// the name is syntactically safe (no shell metacharacters or path traversal).
func validateUnitName(name string) error {
	if name == "" {
		return fmt.Errorf("unit name is required")
	}
	if len(name) > 256 {
		return fmt.Errorf("unit name too long (max 256)")
	}
	if !reServiceName.MatchString(name) {
		return fmt.Errorf("invalid unit name")
	}
	return nil
}

// validateJournalSince validates a journalctl --since parameter.
// Accepts date/time formats like "2024-01-01", "1 hour ago", "today", "yesterday".
// Rejects shell metacharacters, path separators, and other dangerous characters.
var reJournalSince = regexp.MustCompile(`^[a-zA-Z0-9 :._+-]+$`)

func validateJournalSince(since string) error {
	if since == "" {
		return fmt.Errorf("since value is required")
	}
	if len(since) > 64 {
		return fmt.Errorf("since value too long (max 64)")
	}
	if !reJournalSince.MatchString(since) {
		return fmt.Errorf("invalid since value")
	}
	return nil
}

// validateJournalPriority validates a journalctl -p priority level (0-7).
func validateJournalPriority(priority string) error {
	n, err := strconv.Atoi(priority)
	if err != nil || n < 0 || n > 7 {
		return fmt.Errorf("invalid priority (must be 0-7)")
	}
	return nil
}

// validateHardwareLogCategory validates a hardware log category.
func validateHardwareLogCategory(category string) error {
	allowed := map[string]bool{
		"all": true, "i2c": true, "gpio": true, "usb": true,
		"pcie": true, "mmc": true, "net": true, "power": true, "thermal": true,
	}
	if !allowed[category] {
		return fmt.Errorf("invalid hardware log category")
	}
	return nil
}

// validate1WireDeviceID validates a 1-Wire device identifier (e.g., "28-0123456789ab").
func validate1WireDeviceID(id string) error {
	if id == "" {
		return fmt.Errorf("1-Wire device ID is required")
	}
	if !re1WireDeviceID.MatchString(id) {
		return fmt.Errorf("invalid 1-Wire device ID")
	}
	return nil
}

// validateI2CAddress validates an I2C device address (e.g., "0x48").
func validateI2CAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("I2C address is required")
	}
	if !reI2CAddress.MatchString(addr) {
		return fmt.Errorf("invalid I2C address format (expected 0xNN)")
	}
	// Check range 0x03-0x77
	val, err := strconv.ParseUint(strings.TrimPrefix(addr, "0x"), 16, 8)
	if err != nil {
		return fmt.Errorf("invalid I2C address")
	}
	if val < 0x03 || val > 0x77 {
		return fmt.Errorf("I2C address out of range (0x03-0x77)")
	}
	return nil
}

// validateI2CBus validates an I2C bus number.
func validateI2CBus(bus int) error {
	if bus < 0 || bus > 20 {
		return fmt.Errorf("I2C bus out of range (0-20)")
	}
	return nil
}

// validateI2CRegister validates an I2C register address (e.g., "0xFF").
func validateI2CRegister(reg string) error {
	if reg == "" {
		return fmt.Errorf("I2C register is required")
	}
	if !reI2CRegister.MatchString(reg) {
		return fmt.Errorf("invalid I2C register format (expected 0xNN)")
	}
	return nil
}

// validateI2CMode validates an I2C access mode.
func validateI2CMode(mode string) error {
	allowed := map[string]bool{"b": true, "w": true, "i": true, "c": true, "s": true}
	if !allowed[mode] {
		return fmt.Errorf("invalid I2C mode (must be one of: b, w, i, c, s)")
	}
	return nil
}

// validateGPIOChipName validates a GPIO chip name (e.g., "gpiochip0").
func validateGPIOChipName(chip string) error {
	if chip == "" {
		return fmt.Errorf("GPIO chip name is required")
	}
	if !reGPIOChipName.MatchString(chip) {
		return fmt.Errorf("invalid GPIO chip name")
	}
	return nil
}

// validateUSBBusDevice validates USB bus and device numbers.
func validateUSBBusDevice(bus, device int) error {
	if bus < 1 || bus > 128 {
		return fmt.Errorf("USB bus out of range (1-128)")
	}
	if device < 1 || device > 128 {
		return fmt.Errorf("USB device out of range (1-128)")
	}
	return nil
}

// validateSerialPort validates a serial port path, preventing path traversal.
func validateSerialPort(port string) error {
	if port == "" {
		return fmt.Errorf("serial port is required")
	}
	// Clean the path first to resolve any ..
	clean := filepath.Clean(port)
	if clean != port {
		return fmt.Errorf("invalid serial port path (traversal detected)")
	}
	if !reSerialPort.MatchString(clean) {
		return fmt.Errorf("invalid serial port (must be /dev/tty* or /dev/serial*)")
	}
	return nil
}

// validateAPN validates a cellular APN name.
func validateAPN(apn string) error {
	if apn == "" {
		return fmt.Errorf("APN is required")
	}
	if !reAPN.MatchString(apn) {
		return fmt.Errorf("invalid APN format")
	}
	return nil
}

// validateMeshtasticText validates a Meshtastic message.
func validateMeshtasticText(text string) error {
	if text == "" {
		return fmt.Errorf("message text is required")
	}
	if len(text) > 228 {
		return fmt.Errorf("message too long (max 228 chars)")
	}
	if strings.HasPrefix(text, "-") {
		return fmt.Errorf("message must not start with -")
	}
	return nil
}

// validateMeshtasticNodeID validates a Meshtastic node ID (e.g., "!a1b2c3d4").
func validateMeshtasticNodeID(id string) error {
	if id == "" {
		return nil // empty is valid (broadcast)
	}
	if !reMeshtasticNode.MatchString(id) {
		return fmt.Errorf("invalid Meshtastic node ID (expected !HEXHEX)")
	}
	return nil
}

// validateIridiumMessage validates an Iridium SBD message.
func validateIridiumMessage(msg string) error {
	if msg == "" {
		return fmt.Errorf("message is required")
	}
	if len(msg) > 340 {
		return fmt.Errorf("message too long (max 340 chars for SBD)")
	}
	if strings.ContainsAny(msg, "\r\n\x00") {
		return fmt.Errorf("message must not contain CR, LF, or null bytes")
	}
	return nil
}

// validateMixerControl validates an ALSA mixer control name.
func validateMixerControl(control string) error {
	if control == "" {
		return fmt.Errorf("mixer control name is required")
	}
	if !reMixerControl.MatchString(control) {
		return fmt.Errorf("invalid mixer control name")
	}
	return nil
}

// validateCameraIndex validates a camera device index.
func validateCameraIndex(idx int) error {
	if idx < 0 || idx > 15 {
		return fmt.Errorf("camera index out of range (0-15)")
	}
	return nil
}

// validateResolution validates image/video resolution dimensions.
func validateResolution(width, height int) error {
	if width < 1 || width > 4096 {
		return fmt.Errorf("width out of range (1-4096)")
	}
	if height < 1 || height > 4096 {
		return fmt.Errorf("height out of range (1-4096)")
	}
	return nil
}

// validateImageQuality validates JPEG quality (1-100).
func validateImageQuality(quality int) error {
	if quality < 1 || quality > 100 {
		return fmt.Errorf("image quality out of range (1-100)")
	}
	return nil
}

// validateRotation validates image rotation angle.
func validateRotation(rotation int) error {
	valid := map[int]bool{0: true, 90: true, 180: true, 270: true}
	if !valid[rotation] {
		return fmt.Errorf("invalid rotation (must be 0, 90, 180, or 270)")
	}
	return nil
}

// validateAudioCard validates an ALSA audio card number.
func validateAudioCard(card int) error {
	if card < 0 || card > 31 {
		return fmt.Errorf("audio card out of range (0-31)")
	}
	return nil
}

// validateDuration validates a duration in seconds.
func validateDuration(seconds int) error {
	if seconds < 1 || seconds > 10 {
		return fmt.Errorf("duration out of range (1-10 seconds)")
	}
	return nil
}

// validateCapturePath validates a camera capture image path.
func validateCapturePath(path string) error {
	if path == "" {
		return fmt.Errorf("capture path is required")
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, "/tmp/") {
		return fmt.Errorf("capture path must be under /tmp/")
	}
	base := filepath.Base(clean)
	if !strings.HasPrefix(base, "capture_") || !strings.HasSuffix(base, ".jpg") {
		return fmt.Errorf("capture filename must match capture_*.jpg")
	}
	return nil
}

// --- Body size limiter ---

// limitBody wraps the request body with http.MaxBytesReader to prevent oversized payloads.
// Default max is 1MB. Returns the modified request (use: r = limitBody(r, 1<<20)).
func limitBody(r *http.Request, maxBytes int64) *http.Request {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	return r
}

// --- Exec with timeout ---

// defaultExecTimeout is the default timeout for shell commands.
const defaultExecTimeout = 30 * time.Second

// ExecResult contains the output of a command execution.
type ExecResult struct {
	Output string
	Err    error
}

// execWithTimeout runs a command with a context deadline.
// If the provided context has no deadline, a 30-second timeout is applied.
// Returns combined stdout+stderr and any error.
func execWithTimeout(ctx context.Context, name string, args ...string) (string, error) {
	// Apply default timeout if context doesn't already have a deadline
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultExecTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("command timed out")
	}
	return string(out), err
}

// --- Error sanitization ---

// sanitizeExecError returns a safe error message without leaking system internals.
// Use this instead of returning raw exec.CombinedOutput() errors to the client.
func sanitizeExecError(operation string, err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		return fmt.Sprintf("%s: command timed out", operation)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("%s failed (exit code %d)", operation, exitErr.ExitCode())
	}
	return fmt.Sprintf("%s failed", operation)
}
