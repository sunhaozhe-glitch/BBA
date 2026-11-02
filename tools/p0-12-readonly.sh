#!/usr/bin/env bash
# P0-12 single-account read-only run.
#
# One disposable PostgreSQL fixture (--network none, unix socket) plus a runner
# that can only dial the approved loopback OneBot endpoint in napcatd state.
# Real QQ reads are limited to get_login_info, get_status and get_group_list.
# No send, join/application, login, plugin write, service lifecycle change or
# production database is reachable from this script.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${P012_EVIDENCE_DIR:-/tmp/bba-p0-12-evidence-$(date -u +%Y%m%dT%H%M%SZ)}"
STATE_FILE="${P012_STATE_FILE:-/home/sun/.local/state/bba-napcatd/state.json}"
mkdir -m 700 "$EVIDENCE"

CODE_FILES=(
  backend/internal/taskengine/migrations/0001_core.sql
  backend/internal/taskengine/base.go
  backend/internal/taskengine/migrate.go
  backend/internal/taskengine/task.go
  backend/internal/taskengine/task_http.go
  backend/internal/taskengine/scheduler.go
  backend/internal/taskengine/bridge.go
  backend/internal/taskengine/observations.go
  backend/internal/taskengine/heartbeat.go
  backend/internal/taskengine/heartbeat_http.go
  backend/internal/taskengine/schema_test.go
  backend/internal/taskengine/task_test.go
  backend/internal/taskengine/scheduler_test.go
  backend/internal/taskengine/bridge_test.go
  backend/internal/taskengine/observations_test.go
  backend/internal/taskengine/fixture_test.go
  backend/internal/taskengine/remediation_test.go
  backend/internal/taskengine/reaudit_test.go
  backend/internal/taskengine/third_review_test.go
  backend/internal/taskengine/heartbeat_test.go
  backend/internal/taskengine/fourth_review_test.go
  backend/internal/taskengine/fourth_pause_test.go
  backend/cmd/kernel-smoke/main.go
  backend/cmd/p0-12-readonly/main.go
  backend/cmd/p0-12-readonly/main_test.go
  tools/kernel.sh
  tools/p0-12-readonly.sh
)
CODE_SHA="$(cd "$ROOT" && sha256sum "${CODE_FILES[@]}" | sha256sum | cut -d' ' -f1)"

FIXTURE_NAME="bba-p012-$(date +%s)-$$"
SOCKET_VOLUME="$FIXTURE_NAME-socket"
cleanup() {
  docker rm -f "$FIXTURE_NAME" >/dev/null 2>&1 || true
  docker volume rm "$SOCKET_VOLUME" >/dev/null 2>&1 || true
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

{
  echo "utc_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host=$(uname -srm)"
  echo "go_image=$(docker image inspect golang:1.26 --format '{{.Id}}')"
  echo "postgres_image=$(docker image inspect postgres:16-bookworm --format '{{.Id}}')"
  echo "code_sha=$CODE_SHA"
  echo "onebot_endpoint_source=$STATE_FILE"
  echo "evidence_dir=$EVIDENCE"
} > "$EVIDENCE/environment.txt"

set +e
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
  -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
  -e BBA_FIXTURE_ONLY=1 -e 'BBA_FIXTURE_DSN=postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable' \
  -e P012_EVIDENCE_DIR=/evidence \
  -e P012_STATE_FILE=/config/state.json \
  -e P012_CODE_SHA="$CODE_SHA" \
  -e P012_ENVIRONMENT="golang:1.26 container, network host, loopback OneBot only" \
  -v "$ROOT:/work" -v "$SOCKET_VOLUME:/fixture" \
  -v "$STATE_FILE:/config/state.json:ro" \
  -v "$EVIDENCE:/evidence" \
  -w /work/backend golang:1.26 go run ./cmd/p0-12-readonly 2>&1 | tee "$EVIDENCE/stdout.log"
RUN_EXIT=${PIPESTATUS[0]}
set -e

docker exec "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -Atc \
  "SELECT row_to_json(t) FROM (SELECT 'account' AS kind,version,connection_state,login_state,control_state,self_id,heartbeat_at FROM core.account) t
   UNION ALL
   SELECT row_to_json(t) FROM (SELECT 'heartbeat' AS kind,owner_epoch,sequence,connection_state,login_state,heartbeat_at,disposition,request_id,digest,received_at FROM core.runtime_heartbeat ORDER BY received_at) t
   UNION ALL
   SELECT row_to_json(t) FROM (SELECT 'schema' AS kind, max(version) AS version FROM core.schema_version) t" \
  > "$EVIDENCE/db-psql.jsonl"
echo "P0-12 run exit=$RUN_EXIT evidence=$EVIDENCE code_sha=$CODE_SHA"
exit "$RUN_EXIT"
