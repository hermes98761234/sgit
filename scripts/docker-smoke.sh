#!/usr/bin/env bash
# Builds the sg-server image, runs it with a self-signed cert, and runs the
# end-to-end smoke test against it from the host.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-sgit-server:smoke}
NAME=sgit-smoke-$$

docker build -t "$IMAGE" .
docker run -d --name "$NAME" -e SG_TLS_SELF_SIGNED=1 -p 127.0.0.1::7419 "$IMAGE" >/dev/null
cleanup() { docker rm -f -v "$NAME" >/dev/null 2>&1 || true; }
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
# The image ships sg for admin work inside the container.
LIST=$(docker exec "$NAME" sh -c 'sg login localhost:7419 --ca /data/tls/cert.pem --token "$(cat /data/admin-token)" >/dev/null \
  && sg repo create smoke-admin >/dev/null && sg project create smoke-admin >/dev/null \
  && sg project mount smoke-admin . smoke-admin:. >/dev/null && sg project list')
grep -q '^smoke-admin ' <<<"$LIST"
# sg-git-askpass answers git's https prompts from the environment.
[ "$(docker exec "$NAME" sg-git-askpass "Username for 'https://github.com': ")" = x-access-token ]
[ "$(docker exec -e SG_MIRROR_TOKEN=s3cret "$NAME" sg-git-askpass "Password for 'https://x-access-token@github.com': ")" = s3cret ]
# ...and refuses any host other than SG_MIRROR_HOST (default github.com).
if OUT=$(docker exec -e SG_MIRROR_TOKEN=s3cret "$NAME" sg-git-askpass "Password for 'https://x-access-token@evil.example': "); then
  echo "sg-git-askpass answered for evil.example" >&2
  exit 1
fi
if [ -n "$OUT" ]; then
  echo "sg-git-askpass printed output for evil.example" >&2
  exit 1
fi
echo "in-container sg and askpass OK"
SG_SMOKE_ADDR="127.0.0.1:${PORT}" SG_SMOKE_TOKEN="${TOKEN}" go test -tags docker -count=1 ./internal/smoke/
echo "docker smoke OK"
