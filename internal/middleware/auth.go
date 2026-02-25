// Package middleware provides HTTP middleware for the CubeOS HAL service.
package middleware

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"cubeos-hal/internal/config"
)

const (
	// HeaderHALKey is the HTTP header used for HAL API key authentication.
	HeaderHALKey = "X-HAL-Key"
)

// rolePermissions defines which endpoint path prefixes and methods each role can access.
// This is hardcoded as a security boundary — not configurable at runtime.
var rolePermissions = map[config.Role][]permission{
	config.RoleCore: {
		// Full access to all endpoints
		{prefix: "/", methods: allMethods},
	},
	config.RoleMeshSat: {
		// Communication hardware
		{prefix: "/gps/", methods: allMethods},
		{prefix: "/meshtastic/", methods: allMethods},
		{prefix: "/iridium/", methods: allMethods},
		// Basic system and network info (read-only)
		{prefix: "/network/wifi/status/", methods: readOnly},
		{prefix: "/network/status", methods: readOnly},
		{prefix: "/system/info", methods: readOnly},
		{prefix: "/system/temperature", methods: readOnly},
		{prefix: "/system/uptime", methods: readOnly},
		// Power status (read-only)
		{prefix: "/power/status", methods: readOnly},
		{prefix: "/power/battery", methods: readOnly},
		// Sensors (read-only)
		{prefix: "/sensors/", methods: readOnly},
	},
	config.RoleReadOnly: {
		// System info (GET only)
		{prefix: "/system/", methods: readOnly},
		// Power status (GET only)
		{prefix: "/power/status", methods: readOnly},
		{prefix: "/power/battery", methods: readOnly},
		{prefix: "/power/ups", methods: readOnly},
		{prefix: "/power/monitor/status", methods: readOnly},
		// Sensors (GET only)
		{prefix: "/sensors/", methods: readOnly},
	},
}

// permission defines access to a path prefix with allowed HTTP methods.
type permission struct {
	prefix  string
	methods map[string]bool
}

var allMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
	"HEAD": true, "OPTIONS": true,
}

var readOnly = map[string]bool{
	"GET": true, "HEAD": true, "OPTIONS": true,
}

// ACLAuth returns middleware that enforces API key authentication and role-based access control.
// The acl parameter controls behavior:
//   - If acl.Permissive is true, all requests are allowed (backward compatibility).
//   - Otherwise, requests must include a valid X-HAL-Key header mapped to a role
//     with permission for the requested path and method.
//
// Exempt paths (always allowed without a key):
//   - /health (Docker healthcheck)
//   - /hal/health (redundant health check inside /hal route group)
//   - /hal/docs/* (development documentation)
func ACLAuth(acl *config.ACLConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path

			// Always allow health checks and docs (unauthenticated)
			if isExemptPath(path) {
				next.ServeHTTP(w, r)
				return
			}

			// Permissive mode: no ACL configured, allow everything
			if acl == nil || acl.Permissive {
				next.ServeHTTP(w, r)
				return
			}

			// Extract API key from header
			key := r.Header.Get(HeaderHALKey)
			if key == "" {
				writeAuthError(w, http.StatusUnauthorized, "missing X-HAL-Key header")
				return
			}

			// Look up the role for this key
			role, ok := acl.LookupRole(key)
			if !ok {
				writeAuthError(w, http.StatusUnauthorized, "invalid API key")
				return
			}

			// Check if the role has permission for this path + method
			if !isAllowed(role, path, r.Method) {
				log.Printf("ACL: role %q denied %s %s", role, r.Method, path)
				writeAuthError(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// isExemptPath returns true for paths that bypass ACL authentication.
func isExemptPath(path string) bool {
	// Exact health check paths
	if path == "/health" || path == "/hal/health" {
		return true
	}
	// Documentation paths
	if strings.HasPrefix(path, "/hal/docs") {
		return true
	}
	return false
}

// isAllowed checks whether the given role can access the path with the given method.
// The path is expected to be the full URL path (e.g., /hal/system/info).
// We strip the /hal prefix for matching against role permissions.
func isAllowed(role config.Role, path, method string) bool {
	perms, ok := rolePermissions[role]
	if !ok {
		return false
	}

	// Strip /hal prefix for matching — role permissions are defined without it
	halPath := path
	if strings.HasPrefix(halPath, "/hal") {
		halPath = strings.TrimPrefix(halPath, "/hal")
	}
	if halPath == "" {
		halPath = "/"
	}

	for _, p := range perms {
		if strings.HasPrefix(halPath, p.prefix) || halPath == strings.TrimSuffix(p.prefix, "/") {
			if p.methods[method] {
				return true
			}
		}
	}
	return false
}

// writeAuthError writes a JSON error response for authentication/authorization failures.
func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": message,
		"code":  status,
	})
}
