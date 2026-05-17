# Design — Network applier (spec/003)

Retrospective. Real handler files (6) under `internal/handlers/`:
- `network.go` — interfaces + DHCP + static IP + netplan
- `network_capabilities.go` — DHCP / proxy / Ethernet-DHCP capability detection
- `network_ports.go` — `GET /network/ports/listening`
- `network_wifi_saved.go` — saved networks under `/network/wifi/saved/*`
- `network_wifi_status.go` — `GET /network/wifi/status`
- `wifi_ap.go` — WiFi AP setup + revert + whitelist/blacklist

## WiFi AP whitelist/blacklist

Persisted at `/cubeos/config/wifi-ap-{whitelist,blacklist}.json`. Format:

```json
{
  "adapters": [
    {"chipset": "mt7612u", "vendor": "0bda", "product": "8812", "tested_at": 1747396800, "result": "pass"},
    {"chipset": "rtl8188cu", "vendor": "0bda", "product": "8176", "tested_at": 1747396900, "result": "fail", "reason": "hostapd refused: driver-not-supported"}
  ]
}
```

The retest endpoint (`POST /hardware/wifi-ap/retest/<iface>`) re-runs the iw+hostapd test and moves the adapter between lists.

## DHCP / static / netplan path

`POST /network/dhcp/request` → `dhclient -v <iface>`.

`POST /network/ip/static` → `ip addr add <ip/cidr> dev <iface>` + `ip link set <iface> up`.

`POST /network/netplan` → writes `/etc/netplan/99-cubeos.yaml` + `netplan generate && netplan apply`. Full tier only.

## Capability detection

`GET /network/capabilities/dhcp` checks: iptables FORWARD rules permit DHCP relay, NAT not blocking, etc.

`GET /network/capabilities/proxy` checks: HTTP_PROXY env propagation, NPM container alive (if applicable).

`GET /network/ethernet/dhcp-capability` checks: dhcpcd / systemd-networkd Ethernet stanzas.

## Out of scope

- DHCP server (Pi-hole owns DHCP serving — not HAL).
- Cellular network mgmt (separate /cellular/* domain).
- VLAN tagging (operator can use netplan to express).
