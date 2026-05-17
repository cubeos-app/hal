# 3. `CUBEOS_TIER` full/container gating

Date: 2026-05-18 (codifying shipped design)

## Status

Accepted

## Context

HAL runs on two host kinds:

- **Full Pi** (Raspberry Pi 4/5 native host) — can write netplan, recreate compose stacks, manage NAT, swap WiFi AP modes.
- **Containerised host** (x86_64 LXC inside Proxmox, OR container-only test environments) — can't `nsenter`, can't write `/etc/netplan/*.yaml`, can't enable NAT (depends on parent's iptables config).

Letting container-tier callers hit netplan / NAT / compose recreate endpoints causes silent half-failures (HTTP 200 returned but the host is unchanged) — worse than explicit "not supported here."

## Decision

`CUBEOS_TIER` env var set at container start ∈ `{"full", "container"}` (default `full`). `requireFullTier(handler)` wraps destructive endpoints in `internal/handlers/routes.go`:

```go
r.Post("/network/netplan", h.requireFullTier(h.WriteNetplan))
r.Post("/firewall/nat/enable", h.requireFullTier(h.EnableNAT))
r.Post("/system/service/{name}/recreate", h.requireFullTier(h.RecreateComposeService))
r.Post("/network/ap/revert", h.requireFullTier(h.RevertToAP))
// + more
```

Container-tier callers get HTTP 403 with body explaining the tier mismatch.

## Consequences

**Positive:**
- Container-tier HAL refuses operations it can't actually perform.
- Operator deploying to LXC doesn't have to filter their automation by hand.
- The wrap is at routes.go (one place to audit).

**Negative:**
- Two code paths to think about. Mitigated by `requireFullTier` being a single helper that explicitly names the gate.

**Enforced by:** Component Article C-III + the `requireFullTier` wrapper in `internal/handlers/handlers.go`.
