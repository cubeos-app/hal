Feature: Firewall + VPN unified (spec/006 — RETROSPECTIVE)

  # Covers: REQ-500, REQ-501, REQ-502, REQ-503, REQ-504, REQ-505, REQ-506, REQ-507, REQ-508, REQ-509, REQ-510, REQ-511, REQ-512, REQ-513, REQ-514, REQ-515, REQ-516, REQ-517, REQ-518, REQ-519

  Background:
    Given HAL is running with X-HAL-Key=core-key
    And CUBEOS_TIER=full

  # Firewall rules
  Scenario: Add + list + delete rule round-trip
    When POST /firewall/rule {chain:"CUBEOS-INPUT", proto:"tcp", dport:8080, action:"ACCEPT"} is called
    Then GET /firewall/rules includes the new rule
    When DELETE /firewall/rule {selector...} is called
    Then GET /firewall/rules no longer includes the rule

  # NAT
  Scenario: NAT enable adds MASQUERADE
    When POST /firewall/nat/enable {uplink_iface: "eth0"} is called
    Then `iptables -t nat -A CUBEOS-NAT -o eth0 -j MASQUERADE` is invoked
    And `/proc/sys/net/ipv4/ip_forward` is "1"

  Scenario: NAT enable is full-tier-only
    Given CUBEOS_TIER=container
    When POST /firewall/nat/enable is called
    Then HTTP 403 returned with tier-mismatch body

  # Save / restore
  Scenario: Save captures current state
    When POST /firewall/save is called
    Then iptables state is written to a snapshot file

  Scenario: Reset flushes only CubeOS chains
    Given operator rules exist in INPUT (outside CUBEOS-INPUT)
    When POST /firewall/reset is called
    Then CUBEOS-INPUT is flushed
    And operator rules in INPUT survive

  # VPN — WireGuard
  Scenario: wg-quick up invoked on /vpn/wireguard/up
    When POST /vpn/wireguard/up/wg0 is called
    Then `wg-quick up wg0` is invoked

  # VPN — OpenVPN
  Scenario: systemctl start invoked on /vpn/openvpn/up
    When POST /vpn/openvpn/up/myclient is called
    Then `systemctl start openvpn-client@myclient` is invoked

  # VPN — Tor
  Scenario: Tor missing returns 503
    Given Tor is not installed
    When GET /vpn/tor/status is called
    Then HTTP 503 is returned with body explaining missing Tor

  Scenario: newcircuit sends NEWNYM via Tor control port
    Given Tor is running with control port enabled
    When POST /vpn/tor/newcircuit is called
    Then a `SIGNAL NEWNYM` is sent on the Tor control port

  # Aggregate
  Scenario: /vpn/status aggregates all 3
    When GET /vpn/status is called
    Then the response includes wireguard, openvpn, tor keys

  Scenario: /vpn/public-ip resolves via lookup
    When GET /vpn/public-ip is called
    Then the response includes ipv4 + lookup_source
    And the value is cached for 30 seconds
