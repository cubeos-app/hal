// Package config provides configuration loading for the CubeOS HAL service.
package config

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

// Role represents a caller's access level for HAL endpoints.
type Role string

const (
	// RoleCore grants full access to all HAL endpoints (for cubeos-api).
	RoleCore Role = "core"
	// RoleMeshSat grants access to communication and basic system endpoints (for MeshSat).
	RoleMeshSat Role = "meshsat"
	// RoleReadOnly grants GET-only access to system, power status, and sensors (for monitoring).
	RoleReadOnly Role = "readonly"
)

// ACLConfig holds the API key to role mappings.
type ACLConfig struct {
	// Keys maps API keys to their assigned role.
	Keys map[string]Role `json:"keys"`
	// Permissive is true when no ACL config was found — all requests are allowed.
	Permissive bool `json:"-"`
}

// aclFileFormat is the JSON structure for the ACL config file.
type aclFileFormat struct {
	Keys map[string]string `json:"keys"`
}

// LoadACLConfig loads ACL configuration from environment or file.
// Priority: HAL_ACL_KEYS env var (JSON) > HAL_ACL_KEYS_FILE file path > permissive mode.
func LoadACLConfig() *ACLConfig {
	// Try env var first (inline JSON)
	if raw := os.Getenv("HAL_ACL_KEYS"); raw != "" {
		cfg, err := parseACLJSON([]byte(raw))
		if err != nil {
			log.Printf("ACL: failed to parse HAL_ACL_KEYS env var: %v — running in PERMISSIVE mode", err)
			return permissiveConfig()
		}
		if len(cfg.Keys) == 0 {
			log.Printf("ACL: HAL_ACL_KEYS contains no keys — running in PERMISSIVE mode")
			return permissiveConfig()
		}
		log.Printf("ACL: loaded %d key(s) from HAL_ACL_KEYS env var", len(cfg.Keys))
		return cfg
	}

	// Try file path
	filePath := os.Getenv("HAL_ACL_KEYS_FILE")
	if filePath == "" {
		filePath = "/data/acl.json"
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("ACL: no config found (no env var, %s not found) — running in PERMISSIVE mode", filePath)
		} else {
			log.Printf("ACL: failed to read %s: %v — running in PERMISSIVE mode", filePath, err)
		}
		return permissiveConfig()
	}

	cfg, err := parseACLJSON(data)
	if err != nil {
		log.Printf("ACL: failed to parse %s: %v — running in PERMISSIVE mode", filePath, err)
		return permissiveConfig()
	}
	if len(cfg.Keys) == 0 {
		log.Printf("ACL: %s contains no keys — running in PERMISSIVE mode", filePath)
		return permissiveConfig()
	}
	log.Printf("ACL: loaded %d key(s) from %s", len(cfg.Keys), filePath)
	return cfg
}

// parseACLJSON parses the JSON ACL config format: {"keys": {"key1": "core", ...}}
func parseACLJSON(data []byte) (*ACLConfig, error) {
	var raw aclFileFormat
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	cfg := &ACLConfig{
		Keys: make(map[string]Role, len(raw.Keys)),
	}
	for key, role := range raw.Keys {
		cfg.Keys[key] = Role(strings.ToLower(role))
	}
	return cfg, nil
}

// permissiveConfig returns an ACL config that allows all requests.
func permissiveConfig() *ACLConfig {
	return &ACLConfig{
		Permissive: true,
	}
}

// LookupRole returns the role for the given API key, or empty string if not found.
func (c *ACLConfig) LookupRole(key string) (Role, bool) {
	if c == nil || c.Keys == nil {
		return "", false
	}
	role, ok := c.Keys[key]
	return role, ok
}
