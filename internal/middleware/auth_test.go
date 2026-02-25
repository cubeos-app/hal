package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"cubeos-hal/internal/config"
)

// okHandler is a simple handler that returns 200 OK for testing.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
})

func TestACLAuth_PermissiveMode(t *testing.T) {
	acl := &config.ACLConfig{Permissive: true}
	handler := ACLAuth(acl)(okHandler)

	tests := []struct {
		name   string
		path   string
		method string
		key    string
		want   int
	}{
		{"no key allowed", "/hal/system/info", "GET", "", 200},
		{"any key allowed", "/hal/system/reboot", "POST", "random-key", 200},
		{"health no key", "/health", "GET", "", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.key != "" {
				req.Header.Set(HeaderHALKey, tt.key)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("got status %d, want %d", rr.Code, tt.want)
			}
		})
	}
}

func TestACLAuth_NilConfig(t *testing.T) {
	handler := ACLAuth(nil)(okHandler)

	req := httptest.NewRequest("GET", "/hal/system/info", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Errorf("nil ACL config should be permissive, got %d", rr.Code)
	}
}

func TestACLAuth_MissingKey(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"valid-key": config.RoleCore,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	req := httptest.NewRequest("GET", "/hal/system/info", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("missing key should return 401, got %d", rr.Code)
	}
}

func TestACLAuth_InvalidKey(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"valid-key": config.RoleCore,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	req := httptest.NewRequest("GET", "/hal/system/info", nil)
	req.Header.Set(HeaderHALKey, "wrong-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invalid key should return 401, got %d", rr.Code)
	}
}

func TestACLAuth_CoreRole_FullAccess(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"core-key": config.RoleCore,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	tests := []struct {
		name   string
		path   string
		method string
		want   int
	}{
		{"system info GET", "/hal/system/info", "GET", 200},
		{"system reboot POST", "/hal/system/reboot", "POST", 200},
		{"network interfaces GET", "/hal/network/interfaces", "GET", 200},
		{"firewall rule POST", "/hal/firewall/rule", "POST", 200},
		{"gps position GET", "/hal/gps/position", "GET", 200},
		{"power battery GET", "/hal/power/battery", "GET", 200},
		{"gpio pin POST", "/hal/gpio/pin", "POST", 200},
		{"storage devices GET", "/hal/storage/devices", "GET", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set(HeaderHALKey, "core-key")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("core role %s %s: got %d, want %d", tt.method, tt.path, rr.Code, tt.want)
			}
		})
	}
}

func TestACLAuth_MeshSatRole(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"meshsat-key": config.RoleMeshSat,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	tests := []struct {
		name   string
		path   string
		method string
		want   int
	}{
		// Allowed
		{"gps devices GET", "/hal/gps/devices", "GET", 200},
		{"gps position GET", "/hal/gps/position", "GET", 200},
		{"meshtastic status GET", "/hal/meshtastic/status", "GET", 200},
		{"meshtastic send POST", "/hal/meshtastic/messages/send", "POST", 200},
		{"iridium send POST", "/hal/iridium/send", "POST", 200},
		{"iridium status GET", "/hal/iridium/status", "GET", 200},
		{"system info GET", "/hal/system/info", "GET", 200},
		{"system temperature GET", "/hal/system/temperature", "GET", 200},
		{"wifi status GET", "/hal/network/wifi/status/wlan0", "GET", 200},
		{"power status GET", "/hal/power/status", "GET", 200},
		{"sensors all GET", "/hal/sensors/all", "GET", 200},
		// Denied
		{"system reboot POST", "/hal/system/reboot", "POST", 403},
		{"network interfaces GET", "/hal/network/interfaces", "GET", 403},
		{"firewall rules GET", "/hal/firewall/rules", "GET", 403},
		{"gpio pin POST", "/hal/gpio/pin", "POST", 403},
		{"storage devices GET", "/hal/storage/devices", "GET", 403},
		{"vpn status GET", "/hal/vpn/status", "GET", 403},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set(HeaderHALKey, "meshsat-key")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("meshsat role %s %s: got %d, want %d", tt.method, tt.path, rr.Code, tt.want)
			}
		})
	}
}

func TestACLAuth_ReadOnlyRole(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"ro-key": config.RoleReadOnly,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	tests := []struct {
		name   string
		path   string
		method string
		want   int
	}{
		// Allowed (GET only)
		{"system info GET", "/hal/system/info", "GET", 200},
		{"system cpu GET", "/hal/system/cpu", "GET", 200},
		{"system temperature GET", "/hal/system/temperature", "GET", 200},
		{"power status GET", "/hal/power/status", "GET", 200},
		{"power battery GET", "/hal/power/battery", "GET", 200},
		{"sensors all GET", "/hal/sensors/all", "GET", 200},
		{"sensors 1wire GET", "/hal/sensors/1wire/devices", "GET", 200},
		// Denied (POST on read-only endpoints)
		{"system reboot POST", "/hal/system/reboot", "POST", 403},
		{"system hostname POST", "/hal/system/hostname", "POST", 403},
		// Denied (endpoints not in readonly scope)
		{"network interfaces GET", "/hal/network/interfaces", "GET", 403},
		{"gps position GET", "/hal/gps/position", "GET", 403},
		{"gpio pin POST", "/hal/gpio/pin", "POST", 403},
		{"storage devices GET", "/hal/storage/devices", "GET", 403},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set(HeaderHALKey, "ro-key")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("readonly role %s %s: got %d, want %d", tt.method, tt.path, rr.Code, tt.want)
			}
		})
	}
}

func TestACLAuth_HealthBypass(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"core-key": config.RoleCore,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	tests := []struct {
		name string
		path string
		want int
	}{
		{"root health", "/health", 200},
		{"hal health", "/hal/health", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			// No API key — should still pass
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("health bypass %s: got %d, want %d", tt.path, rr.Code, tt.want)
			}
		})
	}
}

func TestACLAuth_DocsBypass(t *testing.T) {
	acl := &config.ACLConfig{
		Keys: map[string]config.Role{
			"core-key": config.RoleCore,
		},
	}
	handler := ACLAuth(acl)(okHandler)

	tests := []struct {
		name string
		path string
		want int
	}{
		{"docs root", "/hal/docs", 200},
		{"docs slash", "/hal/docs/", 200},
		{"docs openapi", "/hal/docs/openapi.yaml", 200},
		{"docs swagger asset", "/hal/docs/swagger-ui/index.html", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			// No API key — should still pass
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("docs bypass %s: got %d, want %d", tt.path, rr.Code, tt.want)
			}
		})
	}
}

func TestIsExemptPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/health", true},
		{"/hal/health", true},
		{"/hal/docs", true},
		{"/hal/docs/", true},
		{"/hal/docs/openapi.yaml", true},
		{"/hal/docs/swagger-ui/something.js", true},
		{"/hal/system/info", false},
		{"/hal/network/interfaces", false},
		{"/hal/gps/position", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isExemptPath(tt.path)
			if got != tt.want {
				t.Errorf("isExemptPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		name   string
		role   config.Role
		path   string
		method string
		want   bool
	}{
		{"core GET anything", config.RoleCore, "/hal/anything", "GET", true},
		{"core POST anything", config.RoleCore, "/hal/anything", "POST", true},
		{"meshsat GET gps", config.RoleMeshSat, "/hal/gps/position", "GET", true},
		{"meshsat POST meshtastic", config.RoleMeshSat, "/hal/meshtastic/messages/send", "POST", true},
		{"meshsat GET storage denied", config.RoleMeshSat, "/hal/storage/devices", "GET", false},
		{"readonly GET system", config.RoleReadOnly, "/hal/system/info", "GET", true},
		{"readonly POST system denied", config.RoleReadOnly, "/hal/system/reboot", "POST", false},
		{"unknown role denied", config.Role("unknown"), "/hal/system/info", "GET", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAllowed(tt.role, tt.path, tt.method)
			if got != tt.want {
				t.Errorf("isAllowed(%q, %q, %q) = %v, want %v", tt.role, tt.path, tt.method, got, tt.want)
			}
		})
	}
}
