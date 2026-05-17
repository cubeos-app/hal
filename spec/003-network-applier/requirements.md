# Requirements — Network applier (spec/003 — RETROSPECTIVE)

Source: `internal/handlers/routes.go` `/network/*` block + 6 handler files (network.go, network_capabilities.go, network_ports.go, network_wifi_saved.go, network_wifi_status.go, wifi_ap.go).

> ID convention: 200-block.

## Interface enumeration

REQ-200: The system shall expose `GET /network/interfaces` returning every Linux network interface with type, state, addresses.
REQ-201: The system shall expose `GET /network/interface/{name}` returning details for one interface.
REQ-202: The system shall expose `GET /network/interface/{name}/traffic` returning rx/tx byte counters.

## WiFi AP

REQ-203: The system shall expose `POST /network/ap/setup` taking SSID + passphrase + interface; writes hostapd config + starts hostapd.
REQ-204: The system shall expose `POST /network/ap/revert` (full tier only) tearing down hostapd + restoring prior config.
REQ-205: The system shall maintain a WiFi AP whitelist at `/cubeos/config/wifi-ap-whitelist.json` and a blacklist at `/cubeos/config/wifi-ap-blacklist.json`.
REQ-206: While testing a new USB WiFi adapter for AP capability, the system shall run `iw phy` + brief hostapd start/stop; pass = whitelist; fail = blacklist with reason.
REQ-207: The system shall expose `GET /hardware/wifi-ap/whitelist` and `GET /hardware/wifi-ap/blacklist`.

## DHCP + static IP

REQ-208: The system shall expose `POST /network/dhcp/request` triggering a DHCP request on the named interface.
REQ-209: The system shall expose `POST /network/ip/static` setting a static IPv4 address.
REQ-210: The system shall expose `POST /network/netplan` (full tier only) writing a netplan YAML config and applying via `netplan apply`.

## Capability detection (per CubeOS access-profile gating)

REQ-211: The system shall expose `GET /network/capabilities/dhcp` reporting whether the host can serve DHCP (kernel/iptables/host config check).
REQ-212: The system shall expose `GET /network/capabilities/proxy` reporting whether the host can serve as HTTP proxy.
REQ-213: The system shall expose `GET /network/ethernet/dhcp-capability` reporting Ethernet-DHCP capability specifically.

## Listening ports introspection

REQ-214: The system shall expose `GET /network/ports/listening` returning every host listening port with PID + process name (via ss / netstat-equivalent).
REQ-215: While returning listening ports, the system shall include UDP + TCP across IPv4 + IPv6.

## Saved networks

REQ-216: The system shall expose endpoints for saved WiFi network management (under /network/wifi/saved/*) backed by `/cubeos/config/wifi-networks.json`.
REQ-217: The system shall expose `GET /network/wifi/status` returning current WiFi association state.

## Tests

REQ-218: The system shall test the WiFi AP whitelist/blacklist persistence in colocated `_test.go` files.
REQ-219: The system shall test the netplan write + apply behind a `requireFullTier` gate (skip-in-container-tier test).
