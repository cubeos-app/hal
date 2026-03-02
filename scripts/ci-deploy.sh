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
echo "Pulling latest HAL image from GHCR..."
GHCR_IMAGE="ghcr.io/cubeos-app/hal"
timeout 120 docker pull "${GHCR_IMAGE}:latest" 2>&1 || {
  echo "Pull failed, using cached..."
}

# --- Retag for local registry ---
LOCAL_REG_IMAGE="localhost:5000/cubeos-app/hal:latest"
docker tag "${GHCR_IMAGE}:latest" "${LOCAL_REG_IMAGE}" 2>/dev/null || true

# --- Push to local registry (keeps registry in sync) ---
docker push "${LOCAL_REG_IMAGE}" 2>/dev/null && \
  echo "  Pushed to local registry: ${LOCAL_REG_IMAGE}" || \
  echo "  WARN: Local registry push failed (non-fatal)"

echo "Stopping existing HAL..."
docker rm -f cubeos-hal 2>/dev/null || true
docker compose down 2>/dev/null || true

echo "Starting HAL..."
DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose up -d --pull never

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
