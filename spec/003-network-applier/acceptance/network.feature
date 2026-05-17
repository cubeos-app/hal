Feature: Network applier (spec/003 — RETROSPECTIVE)

  # Covers: REQ-200, REQ-201, REQ-202, REQ-203, REQ-204, REQ-205, REQ-206, REQ-207, REQ-208, REQ-209, REQ-210, REQ-211, REQ-212, REQ-213, REQ-214, REQ-215, REQ-216, REQ-217, REQ-218, REQ-219

  Background:
    Given HAL is running with X-HAL-Key=core-key
    And CUBEOS_TIER=full

  # Interfaces
  Scenario: List interfaces returns all NICs
    When GET /network/interfaces is called
    Then HTTP 200 is returned
    And the response includes every NIC reported by Linux

  # WiFi AP
  Scenario: AP setup writes hostapd config and starts service
    When POST /network/ap/setup with {ssid, passphrase, interface} is called
    Then /etc/hostapd/hostapd.conf is written
    And hostapd service is started

  Scenario: AP revert tears down hostapd
    Given AP is active
    When POST /network/ap/revert is called (full tier)
    Then hostapd is stopped + config restored

  Scenario: AP capability test persists to whitelist
    Given a fresh USB WiFi adapter passes iw + hostapd test
    When the test runs
    Then /cubeos/config/wifi-ap-whitelist.json includes the adapter
    And /cubeos/config/wifi-ap-blacklist.json does NOT include it

  Scenario: AP capability test on failure persists to blacklist with reason
    Given a USB WiFi adapter fails hostapd start
    When the test runs
    Then /cubeos/config/wifi-ap-blacklist.json includes the adapter with reason

  # DHCP / static / netplan
  Scenario: DHCP request invokes dhclient
    When POST /network/dhcp/request {iface: "eth0"} is called
    Then dhclient is invoked on eth0
    And HTTP 200 is returned

  Scenario: netplan write is full-tier-only
    Given CUBEOS_TIER=container
    When POST /network/netplan is called
    Then HTTP 403 is returned with tier-mismatch body

  Scenario: netplan write applies config in full tier
    Given CUBEOS_TIER=full
    When POST /network/netplan {yaml: ...} is called
    Then /etc/netplan/99-cubeos.yaml is written
    And `netplan generate && netplan apply` runs

  # Capabilities
  Scenario: /network/capabilities/dhcp reports DHCP-serve viability
    When GET /network/capabilities/dhcp is called
    Then the response contains a boolean indicator + diagnostic notes

  # Listening ports
  Scenario: /network/ports/listening enumerates all listeners
    When GET /network/ports/listening is called
    Then the response includes TCP4 + TCP6 + UDP4 + UDP6 entries with PID + process

  # Saved networks
  Scenario: /network/wifi/status reports current WiFi association
    When GET /network/wifi/status is called
    Then the response includes ssid + signal_dbm + state
