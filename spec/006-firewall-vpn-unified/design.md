# Design — Firewall + VPN unified (spec/006)

Retrospective. Real handler files (2):
- `internal/handlers/firewall.go` — firewall rules + NAT + IP forwarding + save/restore/reset
- `internal/handlers/vpn.go` — WireGuard + OpenVPN + Tor

## Firewall topology

CubeOS-managed rules go in the `CUBEOS-INPUT` + `CUBEOS-FORWARD` + `CUBEOS-NAT` chains. Default iptables / nftables chains are NOT modified directly — they jump to the CubeOS chains.

`/firewall/reset` flushes only the CubeOS chains; operator rules in `INPUT` / `FORWARD` / `NAT` (outside the CubeOS jumps) survive.

## NAT enable flow

```
POST /firewall/nat/enable {uplink_iface: "eth0"} (full tier only)
  ↓
iptables -t nat -A CUBEOS-NAT -o eth0 -j MASQUERADE
echo 1 > /proc/sys/net/ipv4/ip_forward
```

NAT disable removes the MASQUERADE rule + resets `ip_forward` (only if no other rules need it).

## VPN per-tool

WireGuard: shells out to `wg-quick up <name>` / `wg-quick down <name>`.
OpenVPN: shells out to `systemctl start openvpn-client@<name>` / `systemctl stop openvpn-client@<name>`.
Tor: shells out to `systemctl start tor` / `systemctl stop tor` + uses the Tor control port for `newcircuit` (`SIGNAL NEWNYM`).

## VPN status aggregation

`GET /vpn/status` walks `wg show interfaces`, `systemctl is-active openvpn-client@*`, `systemctl is-active tor` and returns a combined `{wireguard: [...], openvpn: [...], tor: ...}` payload.

## Public IP lookup

`GET /vpn/public-ip` GETs `https://ifconfig.me` or equivalent via the active VPN if one is up. Cached for 30s to avoid hammering the lookup endpoint.

## Out of scope

- VPN config management (uploading new wg / openvpn configs is operator-driven via filesystem; HAL just up/down's them).
- Per-app VPN routing (api/ wiring policy).
- Tor pluggable transports.
