#!/usr/bin/env bash
set -euo pipefail
if [[ -z "${BBA_P017_LIVE_API_KEY:-}" ]]; then
  echo 'BBA_P017_LIVE_API_KEY must be supplied through the environment' >&2
  exit 2
fi
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export BBA_P017_LIVE_API_KEY
export BBA_P017_LIVE_BASE_URL BBA_P017_LIVE_MODEL_ID BBA_P017_LIVE_PROVIDER_ID BBA_P017_APPROVED_PRIVATE_HTTP_BASE_URL
: "${BBA_P017_LIVE_BASE_URL:?explicit model base URL required}"
: "${BBA_P017_LIVE_MODEL_ID:?explicit model ID required}"
: "${BBA_P017_LIVE_PROVIDER_ID:?explicit provider ID required}"
FIXTURE_NAME="bba-p017-live-$$"
SOCKET_VOLUME="$FIXTURE_NAME-socket"
cleanup() {
  docker rm -f "$FIXTURE_NAME" >/dev/null 2>&1 || true
  docker volume rm "$SOCKET_VOLUME" >/dev/null 2>&1 || true
  unset BBA_P017_LIVE_API_KEY
}
trap cleanup EXIT INT TERM
docker volume create "$SOCKET_VOLUME" >/dev/null
docker run --rm --network none -v "$SOCKET_VOLUME:/fixture" postgres:16-bookworm chmod 0777 /fixture
docker run -d --rm --network none --name "$FIXTURE_NAME" \
  --tmpfs /var/lib/postgresql/data:rw \
  -v "$SOCKET_VOLUME:/fixture" \
  -e POSTGRES_USER=bba_fixture -e POSTGRES_DB=bba_fixture_admin \
  -e PGHOST=/fixture -e POSTGRES_HOST_AUTH_METHOD=trust \
  postgres:16-bookworm postgres -c listen_addresses='' -c unix_socket_directories='/var/run/postgresql,/fixture' -c unix_socket_permissions=0777 >/dev/null
for attempt in $(seq 1 300); do
  if docker exec "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -Atc 'SELECT 1' >/dev/null 2>&1; then break; fi
  if [[ "$attempt" == 300 ]]; then docker logs "$FIXTURE_NAME"; exit 1; fi
  sleep 0.1
done
docker run --rm --network bridge --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
  -e BBA_FIXTURE_ONLY=1 \
  -e 'BBA_FIXTURE_DSN=postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable' \
  -e BBA_P017_LIVE_API_KEY \
  -e BBA_P017_LIVE_BASE_URL -e BBA_P017_LIVE_MODEL_ID -e BBA_P017_LIVE_PROVIDER_ID \
  -e BBA_P017_APPROVED_PRIVATE_HTTP_BASE_URL \
  -v "$ROOT:/work" -v "$SOCKET_VOLUME:/fixture" -w /work/backend \
  golang:1.26 go test -race -count=1 ./internal/taskengine -v
