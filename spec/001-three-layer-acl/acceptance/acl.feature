Feature: HAL three-layer ACL (spec/001 — RETROSPECTIVE)

  # Covers: REQ-001, REQ-002, REQ-003, REQ-004, REQ-005, REQ-006, REQ-007, REQ-008, REQ-009, REQ-010, REQ-011, REQ-012, REQ-013, REQ-014, REQ-015, REQ-016, REQ-017, REQ-018

  Background:
    Given HAL is running with ACL config loaded
    And the ACL has keys: core-key=core, meshsat-key=meshsat, readonly-key=readonly

  # Layer 1 — X-HAL-Key
  Scenario: Missing X-HAL-Key returns 401
    When GET /system/info is called with no X-HAL-Key
    Then HTTP 401 is returned

  Scenario: Unknown X-HAL-Key returns 401
    When GET /system/info is called with X-HAL-Key=wrong
    Then HTTP 401 is returned

  Scenario: /health bypasses X-HAL-Key
    When GET /health is called with no X-HAL-Key
    Then HTTP 200 is returned

  Scenario: /docs/openapi.yaml bypasses X-HAL-Key
    When GET /docs/openapi.yaml is called with no X-HAL-Key
    Then HTTP 200 is returned

  # Layer 2 — Role-based per-prefix
  Scenario: core key can POST anywhere
    When POST /firewall/nat/enable is called with X-HAL-Key=core-key
    Then the handler runs (not blocked by ACL)

  Scenario: meshsat key allowed on /iridium/*
    When POST /iridium/send is called with X-HAL-Key=meshsat-key
    Then HTTP is NOT 403 (ACL allows)

  Scenario: meshsat key BLOCKED on /firewall/*
    When POST /firewall/nat/enable is called with X-HAL-Key=meshsat-key
    Then HTTP 403 is returned

  Scenario: readonly key allowed on GET /system/info
    When GET /system/info is called with X-HAL-Key=readonly-key
    Then HTTP is NOT 403

  Scenario: readonly key BLOCKED on POST /system/reboot
    When POST /system/reboot is called with X-HAL-Key=readonly-key
    Then HTTP 403 is returned

  # Layer 3 — Tier gating
  Scenario: requireFullTier returns 403 on container tier
    Given CUBEOS_TIER=container
    When POST /network/netplan is called with X-HAL-Key=core-key
    Then HTTP 403 is returned
    And the body explains the tier mismatch

  Scenario: requireFullTier allows on full tier
    Given CUBEOS_TIER=full
    When POST /network/netplan is called with X-HAL-Key=core-key
    Then HTTP is NOT 403 (handler runs)

  # Permissive mode posture
  Scenario: Permissive mode logged at startup
    Given HAL_ACL_KEYS and HAL_ACL_KEYS_FILE are both unset
    When HAL starts
    Then the log includes "HAL ACL: PERMISSIVE MODE — no auth!"
    And every 1000th request logs the reminder

  Scenario: Permissive mode allows any key
    Given HAL runs in permissive mode
    When GET /system/info is called with X-HAL-Key=anything
    Then HTTP 200 is returned

  # Auth-test colocation
  Scenario: auth_test.go covers all paths
    When `go test ./internal/middleware/...` runs
    Then tests cover: missing key, unknown key, /health bypass, role-allowed, role-denied, tier-gated
