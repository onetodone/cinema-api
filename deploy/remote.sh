#!/usr/bin/env bash
# Runs on the VPS after the new binaries are uploaded: migrate, restart, wait until both services are ready.
set -euo pipefail

APP_DIR=/opt/go-cinema-api
cd "$APP_DIR"

# Export settings from .env so migrate sees DATABASE_URL and we can read HTTP_ADDR.
set -a
source .env
set +a

API_READY_URL="http://127.0.0.1:${HTTP_ADDR##*:}/readyz"
WORKER_READY_URL="${WORKER_READY_URL:-http://127.0.0.1:9091/readyz}"

# Migrations run before the restart, so they must stay compatible with the binary still running.
./bin/migrate up

# restart waits for the old processes to stop, so a ready answer below comes from the new binaries.
sudo /usr/bin/systemctl restart go-cinema-api go-cinema-worker

for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "$API_READY_URL" && curl -fsS -o /dev/null "$WORKER_READY_URL"; then
    echo "deploy ok"
    exit 0
  fi
  sleep 1
done

echo "services did not become ready within 30s" >&2
journalctl -u go-cinema-api -u go-cinema-worker -n 80 --no-pager >&2 || true
exit 1