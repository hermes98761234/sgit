#!/usr/bin/env bash
# Builds the sg-server image, runs it with a self-signed cert, and runs the
# end-to-end smoke test against it from the host.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-sgit-server:smoke}
NAME=sgit-smoke-$$

docker build -t "$IMAGE" .
docker run -d --name "$NAME" -e SG_TLS_SELF_SIGNED=1 -p 127.0.0.1::7419 "$IMAGE" >/dev/null
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

status=starting
for _ in $(seq 1 30); do
  status=$(docker inspect -f '{{.State.Health.Status}}' "$NAME")
  [ "$status" = healthy ] && break
  sleep 2
done
if [ "$status" != healthy ]; then
  echo "container not healthy: $status" >&2
  docker logs "$NAME" >&2
  exit 1
fi

PORT=$(docker port "$NAME" 7419/tcp | head -1 | sed 's/.*://')
TOKEN=$(docker exec "$NAME" cat /data/admin-token)
SG_SMOKE_ADDR="127.0.0.1:${PORT}" SG_SMOKE_TOKEN="${TOKEN}" go test -tags docker -count=1 ./internal/smoke/
echo "docker smoke OK"
