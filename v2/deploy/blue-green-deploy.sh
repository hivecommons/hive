#!/bin/bash
# Blue-green deploy for Hive v2.
# Builds the new image, starts it alongside the old container,
# waits for the healthcheck to pass, then swaps by renaming containers.
# Nginx resolves "hive" via Docker DNS — no config file changes needed.
#
# Usage: ./blue-green-deploy.sh [--skip-build]

set -euo pipefail

COMPOSE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$COMPOSE_DIR"

HEALTH_URL="http://127.0.0.1:3001/api/health"
HEALTH_INTERVAL_S=5
HEALTH_TIMEOUT_S=180

SKIP_BUILD=false
if [ "${1:-}" = "--skip-build" ]; then
    SKIP_BUILD=true
fi

log() { echo "[deploy] $(date '+%H:%M:%S') $*"; }

MIN_DISK_MB=500

# ── 0. Pre-flight checks ──────────────────────────────────────────
avail_mb=$(df -m "$COMPOSE_DIR" | awk 'NR==2 {print $4}')
if [ "$avail_mb" -lt "$MIN_DISK_MB" ]; then
    log "WARN: Only ${avail_mb}MB free (minimum ${MIN_DISK_MB}MB). Pruning unused images..."
    docker image prune -a -f --filter "until=24h" >/dev/null 2>&1 || true
    docker builder prune -a -f >/dev/null 2>&1 || true
    avail_mb=$(df -m "$COMPOSE_DIR" | awk 'NR==2 {print $4}')
    if [ "$avail_mb" -lt "$MIN_DISK_MB" ]; then
        log "ERROR: Still only ${avail_mb}MB free after prune. Aborting."
        exit 1
    fi
    log "Freed space, now ${avail_mb}MB available."
fi

# ── 1. Build new image ──────────────────────────────────────────────
if [ "$SKIP_BUILD" = false ]; then
    log "Pruning build cache to ensure fresh Go compilation..."
    docker builder prune -f >/dev/null 2>&1 || true
    log "Building new image (no cache)..."
    docker compose build --no-cache hive
fi

# ── 2. Determine active slot ────────────────────────────────────────
GATEWAY_RUNNING=$(docker ps -q -f name=hive-gateway 2>/dev/null || true)

if [ -z "$GATEWAY_RUNNING" ]; then
    log "No gateway found — first-time setup, starting everything..."
    docker compose up -d
    log "Waiting for hive to become healthy..."
    elapsed=0
    while [ "$elapsed" -lt "$HEALTH_TIMEOUT_S" ]; do
        if curl -sf "$HEALTH_URL" >/dev/null 2>/dev/null; then
            log "Hive is healthy. Deploy complete."
            exit 0
        fi
        sleep "$HEALTH_INTERVAL_S"
        elapsed=$((elapsed + HEALTH_INTERVAL_S))
        log "Waiting... (${elapsed}s / ${HEALTH_TIMEOUT_S}s)"
    done
    log "ERROR: Hive failed to become healthy within ${HEALTH_TIMEOUT_S}s"
    exit 1
fi

# ── 3. Start new container alongside old one ────────────────────────
log "Starting new container as hive-next..."

IMAGE=$(docker compose config --images | grep hive | head -1)

ENV_ARGS=""
for var in HIVE_GITHUB_TOKEN HIVE_DASHBOARD_TOKEN NTFY_SERVER NTFY_TOPIC; do
    val="${!var:-}"
    ENV_ARGS="$ENV_ARGS -e $var=$val"
done

DATA_VOL=$(docker inspect hive --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' 2>/dev/null || echo "v2_hive-data")
HIVE_YAML=$(docker inspect hive --format '{{range .Mounts}}{{if eq .Destination "/etc/hive/hive.yaml"}}{{.Source}}{{end}}{{end}}' 2>/dev/null)
SECRETS_DIR=$(docker inspect hive --format '{{range .Mounts}}{{if eq .Destination "/secrets"}}{{.Source}}{{end}}{{end}}' 2>/dev/null)

docker rm -f hive-next 2>/dev/null || true

NETWORK=$(docker inspect hive-gateway --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' | head -1)

docker run -d \
    --name hive-next \
    --init \
    --network "$NETWORK" \
    -v "${HIVE_YAML}:/etc/hive/hive.yaml:ro" \
    -v "${DATA_VOL}:/data" \
    -v "${SECRETS_DIR}:/secrets:ro" \
    $ENV_ARGS \
    --health-cmd "curl -sf http://127.0.0.1:3001/api/health" \
    --health-interval 10s \
    --health-timeout 5s \
    --health-retries 5 \
    --health-start-period 180s \
    "$IMAGE"

# ── 4. Wait for hive-next to become healthy ─────────────────────────
# Probe the endpoint directly via docker exec instead of waiting for
# Docker's health status (which delays behind health-start-period).
log "Waiting for hive-next to become healthy..."
elapsed=0
while [ "$elapsed" -lt "$HEALTH_TIMEOUT_S" ]; do
    if docker exec hive-next curl -sf http://127.0.0.1:3001/api/health >/dev/null 2>&1; then
        log "hive-next is healthy!"
        break
    fi
    RUNNING=$(docker inspect hive-next --format '{{.State.Running}}' 2>/dev/null || echo "false")
    if [ "$RUNNING" != "true" ]; then
        log "ERROR: hive-next stopped unexpectedly."
        docker logs hive-next --tail 20
        docker rm -f hive-next 2>/dev/null || true
        exit 1
    fi
    sleep "$HEALTH_INTERVAL_S"
    elapsed=$((elapsed + HEALTH_INTERVAL_S))
    log "Waiting... (${elapsed}s / ${HEALTH_TIMEOUT_S}s)"
done

if [ "$elapsed" -ge "$HEALTH_TIMEOUT_S" ]; then
    log "ERROR: hive-next did not become healthy within ${HEALTH_TIMEOUT_S}s. Aborting."
    docker logs hive-next --tail 20
    docker rm -f hive-next 2>/dev/null || true
    exit 1
fi

# ── 5. Swap: stop old, rename new, reload nginx ─────────────────────
# Nginx upstream points at "hive" by name. Docker DNS resolves container
# names, so after rename + reload, traffic goes to the new container.
log "Stopping old hive container..."
docker stop hive 2>/dev/null || true
docker rm hive 2>/dev/null || true

log "Renaming hive-next → hive..."
docker rename hive-next hive

# Reload nginx so it re-resolves "hive" via Docker DNS
docker exec hive-gateway nginx -s reload

log "Waiting for gateway to confirm new backend..."
elapsed=0
while [ "$elapsed" -lt 60 ]; do
    if curl -sf "$HEALTH_URL" >/dev/null 2>/dev/null; then
        log "Deploy complete. New version is live."
        exit 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
done

log "WARN: Gateway health check didn't pass within 60s, but swap completed."

# ── 6. Post-deploy cleanup ─────────────────────────────────────────
log "Pruning dangling images..."
docker image prune -f >/dev/null 2>&1 || true
