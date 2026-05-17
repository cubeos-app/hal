# Requirements — Firewall + VPN unified (spec/006 — RETROSPECTIVE)

Source: `internal/handlers/firewall.go` + `internal/handlers/vpn.go` + routes.go `/firewall/*` and `/vpn/*` blocks (CGC-verified).

> ID convention: 500-block.

## Firewall rules CRUD

REQ-500: The system shall expose `GET /firewall/rules` returning all CubeOS-managed firewall rules.
REQ-501: The system shall expose `POST /firewall/rule` accepting a rule definition and adding it.
REQ-502: The system shall expose `DELETE /firewall/rule` accepting a rule selector and removing it.
REQ-503: The system shall expose `GET /firewall/status` returning iptables-restore-style status output.

## NAT + IP forwarding

REQ-504: The system shall expose `GET /firewall/nat/status` reporting NAT enabled/disabled per uplink interface.
REQ-505: The system shall expose `POST /firewall/nat/enable` (full tier only) enabling MASQUERADE on the specified uplink.
REQ-506: The system shall expose `POST /firewall/nat/disable` (full tier only) removing MASQUERADE rules.
REQ-507: The system shall expose `GET /firewall/forwarding` returning `/proc/sys/net/ipv4/ip_forward` value.
REQ-508: The system shall expose `POST /firewall/forward/enable` and `POST /firewall/forward/disable`.

## Backup + restore + reset

REQ-509: The system shall expose `POST /firewall/save` capturing current iptables state.
REQ-510: The system shall expose `POST /firewall/restore` reapplying a saved state.
REQ-511: The system shall expose `POST /firewall/reset` flushing all CubeOS-managed rules (does NOT touch operator rules in non-CubeOS chains).

## VPN — WireGuard

REQ-512: The system shall expose `POST /vpn/wireguard/up/{name}` bringing up the named wg interface.
REQ-513: The system shall expose `POST /vpn/wireguard/down/{name}` bringing down the named wg interface.

## VPN — OpenVPN

REQ-514: The system shall expose `POST /vpn/openvpn/up/{name}` starting the named openvpn config.
REQ-515: The system shall expose `POST /vpn/openvpn/down/{name}` stopping it.

## VPN — Tor

REQ-516: The system shall expose `GET /vpn/tor/status`, `GET /vpn/tor/config`, `POST /vpn/tor/start`, `POST /vpn/tor/stop`, `POST /vpn/tor/newcircuit`.
REQ-517: When Tor is not installed, the Tor endpoints shall return HTTP 503 with body explaining missing Tor.

## Generic VPN status

REQ-518: The system shall expose `GET /vpn/status` aggregating WireGuard + OpenVPN + Tor active states.
REQ-519: The system shall expose `GET /vpn/public-ip` returning the device's externally-visible IP via a public lookup.
