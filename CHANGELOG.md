# Changelog — hal

All notable changes to this project. Format based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) + [Conventional Commits](https://www.conventionalcommits.org/).

Generated from git history + tags by `scripts/sdd-generate-changelog.py` on 2026-05-18.


## [v0.2.0-beta.05] - 2026-03-01

### Added

- Bluetooth/WiFi coexistence management (50c64877)
- USB WiFi AP preference with two-stage test and whitelist/blacklist (dad2cf78)

### Fixed

- replace hardcoded x86_64 arch and NPM URLs with runtime detection (17d169e9)
- use detected uplink interface for DHCP nmap probe (d2974345)


## [v0.2.0-beta.04] - 2026-02-28

### Added

- phase 2 — DHCP + proxy capability detection endpoints (d51e7011)

### Fixed

- eliminate goroutine leak in serial drain/flush (68458df4)
- stop claiming FTDI serial devices as GPS receivers (adb0b1a4)
- container detection via CUBEOS_TIER, ethernet detection for LXC veth (795b15e3)
- device detection LXC/VM/physical + network mode AP-aware (b5e63816)


## [v0.2.0-beta.03] - 2026-02-27

_No commits within window._


## [v0.2.0-beta.02] - 2026-02-27

### Added

- per-caller ACL middleware and key-based authentication [Phase 8.4] (673ec620)
- HAL interface detection + CUBEOS_TIER support (Phase 6c) (97cc4ac7)
- Phase 6b — HAL station mode endpoints for wifi_client switching (3946a5cf)
- Phase 6a — rename network mode constants to v2 names (T6a-06) (2226b15b)

### Changed

- add Apache 2.0 LICENSE (50c6a17e)


## [v0.2.0-beta.01] - 2026-02-24

### Changed

- registry-first batch 4: CI deploy retags + pushes to local registry (919cdd35)


## [v0.2.0-alpha.01] - 2026-02-22

### Changed

- alpha.26 batch 3: B2 targeted NAT rule deletion, EnableNAT idempotency (e50a099f)

### Fixed

- B126 use netplan apply instead of generate+reload (d38d44eb)
- B126 ensure wpa_supplicant before wpa_cli commands (3076cbbf)


## [v0.1.0-alpha.25] - 2026-02-21

### Changed

- use ip -4 to exclude IPv6 neighbor entries from ARP filter (47f55f0c)
- filter stale DHCP clients using ARP table in AP clients fallback (751c922b)
- MASQUERADE all Docker bridges (docker0, gwbridge, br-*) (5fcb3d95)
- use ip command instead of docker CLI for gwbridge detection (8891b901)
- EnableNAT adds docker_gwbridge MASQUERADE for container internet (c406e63c)

### Fixed

- B112 netplan generate before networkctl reload, B96b bring tethering iface UP (bf03f4cc)


## [v0.1.0-alpha.24] - 2026-02-20

### Changed

- GPS scanner excludes Meshtastic radios via VID:PID filtering (d1214104)
- alpha.24 batch 3: B68 GPS sysfs symlink fix, B96 Android tethering DHCP fallback (589be9f5)


## [v0.1.0-alpha.23] - 2026-02-19

### Added

- migrate deploy to SSH from GPU VM (no Pi runner needed) (2ae4813f)
- initial HAL source (migrated from coreapps) (b487c843)

### Fixed

- B68 GPS sysfs walk, B84 enx* Android tethering (5f5a1421)
- unignore cmd/cubeos-hal/ directory (934b961d)

