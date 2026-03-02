#!/usr/bin/env bash
# =============================================================================
# CubeOS HAL — Pi-side deploy script (executed via SSH from GPU VM)
# =============================================================================
# Usage: GHCR_TOKEN=... GHCR_USER=... bash /tmp/ci-deploy-hal.sh
# =============================================================================
set -euo pipefail

COMPOSE_FILE="/cubeos/coreapps/cubeos-hal/appconfig/docker-compose.yml"

# --- Source env files for compose variable substitution ---
if [ -f /cubeos/config/defaults.env ]; then
  set -a
  source /cubeos/config/defaults.env
  set +a
fi
if [ -f /cubeos/config/secrets.env ]; then
  set -a
  source /cubeos/config/secrets.env
  set +a
fi

echo "=== HAL Deploy ==="

# --- Pre-flight ---
if [ ! -f "$COMPOSE_FILE" ]; then
  echo "ERROR: HAL compose file not found at $COMPOSE_FILE"
  exit 1
fi

# --- GHCR login ---
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USER" --password-stdin

# --- Deploy ---
cd /cubeos/coreapps/cubeos-hal/appconfig

# --- Pull from GHCR ---
GHCR_IMAGE="ghcr.io/cubeos-app/hal"
PULL_TAG="${CI_COMMIT_SHORT_SHA:-latest}"
echo "Pulling HAL image from GHCR (${PULL_TAG})..."
timeout 120 docker pull "${GHCR_IMAGE}:${PULL_TAG}" 2>&1 || {
  echo "Pull failed, using cached..."
}

# --- Retag for local registry ---
LOCAL_REG_IMAGE="localhost:5000/cubeos-app/hal:latest"
docker tag "${GHCR_IMAGE}:${PULL_TAG}" "${GHCR_IMAGE}:latest" 2>/dev/null || true
docker tag "${GHCR_IMAGE}:${PULL_TAG}" "${LOCAL_REG_IMAGE}" 2>/dev/null || true

# --- Push to local registry (keeps registry in sync) ---
docker push "${LOCAL_REG_IMAGE}" 2>/dev/null && \
  echo "  Pushed to local registry: ${LOCAL_REG_IMAGE}" || \
  echo "  WARN: Local registry push failed (non-fatal)"

# --- Check if running container already has this image ---
RUNNING_CID=$(docker ps --filter "name=cubeos-hal" --format '{{.ID}}' | head -1 || true)
if [ -n "${RUNNING_CID}" ]; then
  RUNNING_SHA=$(docker inspect "${RUNNING_CID}" --format '{{.Image}}' 2>/dev/null || true)
  TARGET_SHA=$(docker image inspect "${LOCAL_REG_IMAGE}" --format '{{.Id}}' 2>/dev/null || true)
  if [ -n "${RUNNING_SHA}" ] && [ -n "${TARGET_SHA}" ] && [ "${RUNNING_SHA}" = "${TARGET_SHA}" ]; then
    echo "Already running target image (digest match) — skipping recreate"
    echo "Swagger UI: http://cubeos.cube:6005/hal/docs"
    echo "Deploy complete (no change)"
    exit 0
  else
    echo "Image changed: running=${RUNNING_SHA:7:12} target=${TARGET_SHA:7:12}"
  fi
fi

# --- Save WiFi state before deploy ---
WIFI_WAS_UP=false
WIFI_IP=""
if ip link show wlan0 2>/dev/null | grep -q "state UP"; then
  WIFI_WAS_UP=true
  WIFI_IP=$(ip -4 addr show wlan0 2>/dev/null | grep -oP 'inet \K[0-9.]+' || true)
  echo "WiFi client active: wlan0=${WIFI_IP:-unknown}"
fi

# --- Spawn WiFi recovery watchdog BEFORE recreate ---
# This runs as a detached background process so it survives if SSH dies
# (HAL container recreate can disrupt WiFi, killing the SSH session)
if [ "$WIFI_WAS_UP" = "true" ]; then
  (
    sleep 15  # wait for container recreate to happen
    # Always run netplan apply — container recreate disconnects wpa_supplicant
    # even if the interface still shows UP with a stale IP
    echo "Running netplan apply to reconnect WiFi..."
    netplan apply 2>/dev/null || true
    sleep 10
    for attempt in 1 2 3; do
      WIFI_LINK=$(iw dev wlan0 link 2>/dev/null | head -1)
      WIFI_NOW=$(ip -4 addr show wlan0 2>/dev/null | grep -oP 'inet \K[0-9.]+' || true)
      if echo "$WIFI_LINK" | grep -q "Connected"; then
        echo "WiFi connected (attempt $attempt): ${WIFI_NOW:-no IP yet}"
        break
      fi
      echo "WiFi not connected (attempt $attempt) — retrying..."
      netplan apply 2>/dev/null || true
      sleep 10
    done
    # Restore source-based routing for dual-interface same-subnet
    sysctl -qw net.ipv4.conf.all.arp_filter=1 2>/dev/null || true
    sysctl -qw net.ipv4.conf.all.arp_announce=2 2>/dev/null || true
    WIFI_NOW=$(ip -4 addr show wlan0 2>/dev/null | grep -oP 'inet \K[0-9.]+' || true)
    if [ -n "$WIFI_NOW" ]; then
      WLAN_GW=$(ip route show dev wlan0 2>/dev/null | grep default | awk '{print $3}')
      if [ -n "$WLAN_GW" ]; then
        ip rule add from "$WIFI_NOW" table 100 2>/dev/null || true
        ip route replace default via "$WLAN_GW" dev wlan0 table 100 2>/dev/null || true
      fi
    fi
    echo "WiFi watchdog done: wlan0=${WIFI_NOW:-FAILED}"
  ) </dev/null >/tmp/hal-wifi-recovery.log 2>&1 &
  disown
  echo "WiFi recovery watchdog spawned (PID: $!)"
fi

# --- Deploy HAL (atomic recreate — avoids full teardown) ---
echo "Deploying HAL (force-recreate)..."
DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose up -d --force-recreate --pull never

# --- Health check ---
sleep 5
for i in $(seq 1 10); do
  if curl -sf http://127.0.0.1:6005/health >/dev/null 2>&1; then
    echo "HAL healthy"
    break
  fi
  [ "$i" -eq 10 ] && echo "HAL may still be starting..."
  sleep 3
done

echo "Swagger UI: http://cubeos.cube:6005/hal/docs"
echo "Deploy complete"
