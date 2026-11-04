#!/usr/bin/env bash
## Isolated PostgreSQL fixture through schema 26. No host ports or production DSN accepted.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:-test}"
if [[ $# -gt 0 ]]; then shift; fi
case "$MODE" in test|test-all|smoke|schema) ;; *) echo 'usage: tools/kernel.sh test [go test flags] | test-all | smoke | schema' >&2; exit 2;; esac
FIXTURE_NAME="bba-p004-011-$(date +%s)-$$"
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
 -e PGHOST=/fixture \
 -e POSTGRES_HOST_AUTH_METHOD=trust \
 postgres:16-bookworm postgres -c listen_addresses='' -c unix_socket_directories='/var/run/postgresql,/fixture' -c unix_socket_permissions=0777 >/dev/null
# pg_isready ignores the database name and can answer from the temporary
# initdb server, so wait for the real database to accept a query instead.
for attempt in $(seq 1 300); do
 if docker exec "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -Atc 'SELECT 1' >/dev/null 2>&1; then break; fi
 if [[ "$attempt" == 300 ]]; then docker logs "$FIXTURE_NAME"; exit 1; fi
 sleep 0.1
done
if [[ "$MODE" == schema ]]; then
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0001_core.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0002_chat_send_permits.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0003_model_requests.sql"
    docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0004_approved_join_text.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0005_graph_projection.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0006_plan_proposals.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0007_group_summary.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0008_plan_proposal_basis.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0009_worker_cursor.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0010_proposal_work.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0011_artex_sync.sql"
	docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/0012_login_sessions.sql"
	for migration in 0013_content_rules.sql 0014_work_slots.sql 0015_work_settings.sql 0016_qq_conversations.sql 0017_operator_notification_scope.sql; do
	 docker exec -i "$FIXTURE_NAME" psql -h /fixture -U bba_fixture -d bba_fixture_admin -v ON_ERROR_STOP=1 < "$ROOT/backend/internal/taskengine/migrations/$migration"
	done
	exit
fi
GO_COMMAND=(go test -race -count=1 ./internal/taskengine)
if [[ "$MODE" == smoke ]]; then
 GO_COMMAND=(go run ./cmd/kernel-smoke)
elif [[ "$MODE" == test-all ]]; then
 # Extra flags go before the package list so the default race run can be
 # relaxed, e.g. `test-all -race=false`.
 GO_COMMAND=(go test -race -count=1)
 GO_COMMAND+=("$@")
 GO_COMMAND+=(./...)
else
 GO_COMMAND+=("$@")
fi
docker run --rm --network none --user "$(id -u):$(id -g)" \
 -e HOME=/scratch/home -e TMPDIR=/scratch -e GOTMPDIR=/scratch \
 -v "${TMPDIR:?TMPDIR required}:/scratch" \
 -e GOPATH=/work/.cache/gopath -e GOMODCACHE=/work/.cache/gomod \
 -e GOCACHE=/work/.cache/gobuild -e GOPROXY=off -e GOSUMDB=off -e GOFLAGS=-mod=readonly \
 -e BBA_FIXTURE_ONLY=1 -e 'BBA_FIXTURE_DSN=postgres://bba_fixture@/bba_fixture_admin?host=/fixture&sslmode=disable' \
 -e CONTRACT_ROOT=/work/contracts \
 -v "$ROOT:/work" -v "$SOCKET_VOLUME:/fixture" -w /work/backend \
 golang:1.26 "${GO_COMMAND[@]}"
