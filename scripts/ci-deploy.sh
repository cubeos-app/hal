#!/usr/bin/env bash
# =============================================================================
# CubeOS HAL — Pi-side deploy script (executed via SSH from GPU VM)
# =============================================================================
# Usage: GHCR_TOKEN=... GHCR_USER=... bash /tmp/ci-deploy-hal.sh
# =============================================================================
set -euo pipefail

COMPOSE_FILE="/cubeos/coreapps/cubeos-hal/appconfig/docker-compose.yml"

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

echo "Pulling latest HAL image..."
timeout 120 docker compose pull 2>&1 || echo "Pull failed, using cached..."

echo "Stopping existing HAL..."
docker rm -f cubeos-hal 2>/dev/null || true
docker compose down 2>/dev/null || true

echo "Starting HAL..."
docker compose up -d --pull always

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
