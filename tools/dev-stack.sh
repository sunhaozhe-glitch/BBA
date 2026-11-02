#!/usr/bin/env bash
# Isolated Task Engine development stack: disposable PostgreSQL fixture plus the
# authenticated console, deterministic worker, and fixture runtime routes.
#
#   tools/dev-stack.sh start          # DB container, schema head, backend + worker
#   tools/dev-stack.sh stop           # SIGTERM drain for the server, then DB stop
#   tools/dev-stack.sh status         # ports, pids, health, schema version
#   tools/dev-stack.sh logs           # DB container logs + server log tail
#   tools/dev-stack.sh env            # fixture exports (prints secrets)
#   tools/dev-stack.sh token          # console fixture bearer token
#   tools/dev-stack.sh runtime-token  # separate runtime fixture bearer token
# Fixture-only by construction:
#  * PostgreSQL listens on 127.0.0.1:$BBA_DEV_STACK_DB_PORT (default 18432), never
#    5432; the console server listens on 127.0.0.1:$BBA_DEV_STACK_HTTP_PORT
#    (default 18431), never the deployment default 18421.
#  * Data lives in the dedicated docker volume and state directory of this
#    instance only; the volume name and container name carry the instance name.
#  * No QQ adapter, no OneBot/NapCat connection, no production DSN. The
#    deterministic backend worker runs with its fixture transport only. A
#    non-fixture BBA_TASKENGINE_DSN is rejected rather than reused.
#  * Credentials and the bearer token are generated into 0600 files under the
#    state directory and are only printed by explicit token/env commands.
#
# Do not point this at a production database and do not expose either port.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTANCE="${BBA_DEV_STACK_NAME:-default}"
DB_PORT="${BBA_DEV_STACK_DB_PORT:-18432}"
HTTP_PORT="${BBA_DEV_STACK_HTTP_PORT:-18431}"
STATE_DIR="${BBA_DEV_STACK_STATE_DIR:-$ROOT/build/dev-stack/$INSTANCE}"
PG_IMAGE="${BBA_DEV_STACK_PG_IMAGE:-postgres:16-alpine}"
GO_IMAGE="${BBA_DEV_STACK_GO_IMAGE:-golang:1.26}"

DB_USER="bba_dev"
DB_NAME="bba_dev"
WORKER_POLL="${BBA_DEV_STACK_WORKER_POLL:-2s}"
CONT_NAME="bba-dev-stack-$INSTANCE-db"
VOL_NAME="bba-dev-stack-$INSTANCE-pgdata"
SERVER_BIN="$STATE_DIR/bin/taskengine-server"
MIGRATE_BIN="$STATE_DIR/bin/taskengine-migrate"
SERVER_LOG="$STATE_DIR/server.log"
PID_FILE="$STATE_DIR/server.pid"
PASSWORD_FILE="$STATE_DIR/pg-password"
TOKEN_FILE="$STATE_DIR/console-token"
RESOLVER_FILE="$STATE_DIR/resolver.json"
RUNTIME_TOKEN_FILE="$STATE_DIR/runtime-token"
DSN_FILE="$STATE_DIR/dsn"

die() { echo "dev-stack: $*" >&2; exit 1; }
note() { echo "dev-stack: $*"; }

usage() {
	printf '%s\n' 'usage: tools/dev-stack.sh start|stop|status|logs|env|token|runtime-token|reset' >&2
	exit 2
}

# --- validation ------------------------------------------------------------

validate_config() {
  [[ "$INSTANCE" =~ ^[A-Za-z0-9_-]{1,32}$ ]] || die "instance name must match [A-Za-z0-9_-]{1,32}"
  for spec in "db:$DB_PORT:5432" "http:$HTTP_PORT:18421"; do
    IFS=: read -r label value reserved <<<"$spec"
    [[ "$value" =~ ^[0-9]+$ ]] || die "BBA_DEV_STACK_${label^^}_PORT must be numeric"
    (( value >= 1024 && value <= 65535 )) || die "$label port $value is outside 1024-65535"
    [[ "$value" != "$reserved" ]] || die "$label port must differ from the real service port $reserved"
  done
  [[ "$DB_PORT" != "$HTTP_PORT" ]] || die "db and http ports must differ"
  [[ "$WORKER_POLL" =~ ^[0-9]+(ms|s|m)$ ]] || die "BBA_DEV_STACK_WORKER_POLL must be a duration such as 2s or 500ms"
}

refuse_production_dsn() {
  # The fixture DSN is always built here and passed explicitly to the child
  # processes, but an inherited variable from a production shell must never be
  # silently reused either.
  if [[ -v BBA_TASKENGINE_DSN ]]; then
    if [[ "${BBA_TASKENGINE_DSN}" != "postgres://$DB_USER:"*"@127.0.0.1:$DB_PORT/$DB_NAME?sslmode=disable" ]]; then
      die "BBA_TASKENGINE_DSN points outside this fixture (127.0.0.1:$DB_PORT/$DB_NAME); unset it before starting."
    fi
  fi
}

require_docker() {
  command -v docker >/dev/null 2>&1 || die "docker is required"
  docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
}

require_curl() { command -v curl >/dev/null 2>&1 || die "curl is required for health checks"; }

random_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
  else
    od -An -v -tx1 -N"$1" /dev/urandom | tr -d ' \n'
  fi
}

port_busy() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
}

http_code() {
  curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$@" 2>/dev/null || true
}

container_state() {
  docker inspect -f '{{.State.Status}}' "$CONT_NAME" 2>/dev/null || true
}

volume_exists() { docker volume inspect "$VOL_NAME" >/dev/null 2>&1; }

# --- state and secrets -----------------------------------------------------

prepare_state() {
  [[ -d "$STATE_DIR" ]] || (umask 077 && mkdir -p "$STATE_DIR")
  [[ -d "$STATE_DIR/bin" ]] || (umask 077 && mkdir -p "$STATE_DIR/bin")
  chmod 700 "$STATE_DIR"

  if [[ ! -s "$PASSWORD_FILE" ]]; then
    (umask 077 && random_hex 24 > "$PASSWORD_FILE")
  fi
  chmod 600 "$PASSWORD_FILE"

  if [[ ! -s "$TOKEN_FILE" ]]; then
    (umask 077 && printf '%s' "$(random_hex 32)" > "$TOKEN_FILE")
  fi
  if [[ ! -s "$RUNTIME_TOKEN_FILE" ]]; then
    (umask 077 && printf '%s' "$(random_hex 32)" > "$RUNTIME_TOKEN_FILE")
  fi
  chmod 600 "$RUNTIME_TOKEN_FILE"
  local runtime_token runtime_digest
  runtime_token="$(cat "$RUNTIME_TOKEN_FILE")"
  [[ -n "$runtime_token" && "$runtime_token" != *[[:space:]]* ]] || die "runtime-token must be a single non-empty token"
  runtime_digest="$(printf '%s' "$runtime_token" | sha256sum | cut -d' ' -f1)"
  [[ "$runtime_digest" =~ ^[0-9a-f]{64}$ ]] || die "failed to digest the runtime fixture token"
  chmod 600 "$TOKEN_FILE"
  local token
  token="$(cat "$TOKEN_FILE")"
  [[ -n "$token" && "$token" != *[[:space:]]* ]] || die "console-token must be a single non-empty token"

  PG_PASSWORD="$(cat "$PASSWORD_FILE")"
  [[ -n "$PG_PASSWORD" ]] || die "pg-password is empty"
  DSN="postgres://$DB_USER:$PG_PASSWORD@127.0.0.1:$DB_PORT/$DB_NAME?sslmode=disable"
  (umask 077 && printf '%s\n' "$DSN" > "$DSN_FILE")
  chmod 600 "$DSN_FILE"

  # The resolver holds only the SHA-256 digest of the fixture token; the token
  # itself stays in console-token. Rewritten on every start so the digest always
  # matches the current token file.
  local digest
  digest="$(printf '%s' "$token" | sha256sum | cut -d' ' -f1)"
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || die "failed to digest the fixture token"
	(umask 077 && printf '{"credentials":[{"token_sha256":"%s","workspace_id":"dev-stack-%s","principal_id":"dev-operator","permissions":["operator.read","task.read","task.create","task.control","task.approve","task.manage_all"]},{"token_sha256":"%s","workspace_id":"dev-stack-%s","runtime_id":"dev-runtime-%s","owner_epoch":1,"account_ids":["dev-account-%s"]}]}\n' \
	  "$digest" "$INSTANCE" "$runtime_digest" "$INSTANCE" "$INSTANCE" "$INSTANCE" > "$RESOLVER_FILE")
	chmod 600 "$RESOLVER_FILE"
}

# --- server process --------------------------------------------------------

server_pid() {
  local pid
  [[ -f "$PID_FILE" ]] || return 1
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  # Only trust the pid when its command line is this instance's server binary;
  # a recycled pid must never be signalled.
  if [[ -r "/proc/$pid/cmdline" ]] && tr '\0' ' ' < "/proc/$pid/cmdline" | grep -qF -- "$STATE_DIR/bin/taskengine-server"; then
    printf '%s\n' "$pid"
    return 0
  fi
  return 1
}

# --- docker fixture --------------------------------------------------------

ensure_db() {
  local state
  state="$(container_state)"
  if [[ "$state" == "running" ]]; then
    :
  elif [[ -n "$state" ]]; then
    note "starting existing database container $CONT_NAME"
    docker start "$CONT_NAME" >/dev/null
  else
    if docker inspect "$CONT_NAME" >/dev/null 2>&1; then
      die "container name $CONT_NAME is taken; run tools/dev-stack.sh reset"
    fi
    if port_busy "$DB_PORT"; then
      die "127.0.0.1:$DB_PORT is already in use; stop the other service or choose BBA_DEV_STACK_DB_PORT"
    fi
    if ! volume_exists; then
      docker volume create --label "bba.dev-stack=$INSTANCE" --label bba.dev-stack.role=data "$VOL_NAME" >/dev/null
    fi
    note "creating database container $CONT_NAME on 127.0.0.1:$DB_PORT"
    docker run -d --name "$CONT_NAME" \
      --label "bba.dev-stack=$INSTANCE" --label bba.dev-stack.role=db \
      -e POSTGRES_USER="$DB_USER" \
      -e POSTGRES_DB="$DB_NAME" \
      -e POSTGRES_PASSWORD="$PG_PASSWORD" \
      -p "127.0.0.1:$DB_PORT:5432" \
      -v "$VOL_NAME:/var/lib/postgresql/data" \
      --restart no \
      "$PG_IMAGE" >/dev/null
  fi

  local published
  published="$(docker port "$CONT_NAME" 5432/tcp 2>/dev/null | head -n1 || true)"
  [[ "$published" == "127.0.0.1:$DB_PORT" ]] || \
    die "database container publishes $published, expected 127.0.0.1:$DB_PORT; run tools/dev-stack.sh reset"

  local attempt
  for attempt in $(seq 1 150); do
    if docker exec "$CONT_NAME" pg_isready -h 127.0.0.1 -U "$DB_USER" -d "$DB_NAME" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  docker logs --tail 40 "$CONT_NAME" >&2 || true
  die "database did not become ready within 30s"
}

build_server() {
  if [[ "${BBA_DEV_STACK_SKIP_BUILD:-0}" == "1" ]]; then
    [[ -x "$SERVER_BIN" && -x "$MIGRATE_BIN" ]] || die "BBA_DEV_STACK_SKIP_BUILD=1 but binaries are missing"
    return 0
  fi
  note "building taskengine-server and taskengine-migrate from current src (offline Go cache)"
  docker run --rm --network none --user "$(id -u):$(id -g)" \
    -e HOME=/tmp/home \
    -e GOPATH=/work/.cache/gopath \
    -e GOMODCACHE=/work/.cache/gomod \
    -e GOCACHE=/work/.cache/gobuild \
    -e GOPROXY=off -e GOSUMDB=off -e CGO_ENABLED=0 \
    -v "$ROOT:/work" -v "$STATE_DIR:/out" -w /work/backend "$GO_IMAGE" \
    /bin/sh -c 'set -e
      /usr/local/go/bin/go build -trimpath -o /out/bin/taskengine-migrate ./cmd/taskengine-migrate
      /usr/local/go/bin/go build -trimpath -o /out/bin/taskengine-server ./cmd/taskengine-server'
}

schema_version() {
  docker exec "$CONT_NAME" psql -U "$DB_USER" -d "$DB_NAME" -Atc 'SELECT max(version) FROM core.schema_version' 2>/dev/null || true
}

# The server validates the schema it finds and never migrates on startup, so a
# brand-new fixture is initialized once here through the explicit command. An
# existing fixture is left untouched: the migration head is whatever
# internal/taskengine/migrations currently defines, so a schema bump needs no
# change to this script.
ensure_schema() {
  local version
  version="$(schema_version)"
  if [[ -n "$version" ]]; then
    note "fixture database already reports schema $version; taskengine-migrate only initializes empty databases"
    return 0
  fi
  # taskengine-migrate accepts only a brand-new database and reads the DSN from
  # BBA_TASKENGINE_DSN; the value is the fixture DSN built above, not a
  # production string.
  note "initializing empty fixture database with taskengine-migrate"
  BBA_TASKENGINE_DSN="$DSN" "$MIGRATE_BIN"
  version="$(schema_version)"
  [[ -n "$version" ]] || die "migration finished but core.schema_version is still empty"
  note "fixture database initialized at schema $version"
}
seed_dev_identity() {
	local workspace="dev-stack-$INSTANCE" account="dev-account-$INSTANCE" runtime="dev-runtime-$INSTANCE" self_id="100001"
	docker exec -i "$CONT_NAME" psql -v ON_ERROR_STOP=1 -U "$DB_USER" -d "$DB_NAME" \
	  -v workspace="$workspace" -v account="$account" -v runtime="$runtime" -v self_id="$self_id" >/dev/null <<'SQL'
INSERT INTO core.workspace(id) VALUES(:'workspace') ON CONFLICT DO NOTHING;
INSERT INTO core.principal(workspace_id,id) VALUES(:'workspace','dev-operator') ON CONFLICT DO NOTHING;
INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,
  connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,
  quota_limit,heartbeat_at)
VALUES(:'workspace',:'account',:'runtime','dev-fixture-adapter',:'self_id','verified',
  'online','logged_in','active','clear',false,false,1,clock_timestamp())
ON CONFLICT(workspace_id,id) DO NOTHING;
INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority,active)
VALUES(:'workspace',:'runtime',:'account',1,false,true)
ON CONFLICT(workspace_id,runtime_id,account_id) DO NOTHING;
SQL
	local seeded
	seeded="$(docker exec "$CONT_NAME" psql -U "$DB_USER" -d "$DB_NAME" -Atc "SELECT count(*) FROM core.account WHERE workspace_id='dev-stack-$INSTANCE' AND id='dev-account-$INSTANCE' AND self_id='100001'")"
	[[ "$seeded" == "1" ]] || die "fixture account seed missing or ambiguous (count=${seeded:-unavailable})"
}

wait_for_http() {
  local attempt code
  for attempt in $(seq 1 100); do
    code="$(http_code "http://127.0.0.1:$HTTP_PORT/")"
    if [[ "$code" == "200" ]]; then
      return 0
    fi
    if [[ ! -f "$PID_FILE" ]] || ! kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
      return 1
    fi
    sleep 0.2
  done
  return 1
}

start_server() {
  (umask 077
    : > "$SERVER_LOG"
    cd "$STATE_DIR"
	    export BBA_TASKENGINE_DSN="$DSN"
    nohup "$SERVER_BIN" \
      -listen "127.0.0.1:$HTTP_PORT" \
      -origin "http://127.0.0.1:$HTTP_PORT" \
      -resolver-file "$RESOLVER_FILE" \
      -worker -worker-id "dev-worker-$INSTANCE" -worker-poll "$WORKER_POLL" -worker-recover \
	  -development-operations \
      >>"$SERVER_LOG" 2>&1 </dev/null &
    echo $! > "$PID_FILE")
  if ! wait_for_http; then
    tail -n 40 "$SERVER_LOG" >&2 || true
    die "server did not answer http://127.0.0.1:$HTTP_PORT/ within 20s; if the fixture schema is older than the migration head, run tools/dev-stack.sh reset"
  fi
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 \
    -H "Authorization: Bearer $(cat "$TOKEN_FILE")" \
    "http://127.0.0.1:$HTTP_PORT/v1/accounts" || true)"
  [[ "$code" == "200" ]] || die "authenticated GET /v1/accounts returned $code; check $SERVER_LOG and tools/dev-stack.sh token"
}

# --- commands --------------------------------------------------------------

cmd_start() {
  validate_config
  refuse_production_dsn
  require_docker
  require_curl
  prepare_state

  local pid
  if pid="$(server_pid)"; then
    [[ "$(http_code "http://127.0.0.1:$HTTP_PORT/")" == "200" ]] \
      || die "server pid $pid is running but not healthy; run tools/dev-stack.sh stop first"
    note "already running: pid $pid console http://127.0.0.1:$HTTP_PORT/"
    return 0
  fi
  rm -f "$PID_FILE"
  if port_busy "$HTTP_PORT"; then
    die "127.0.0.1:$HTTP_PORT is already in use; stop the other service or choose BBA_DEV_STACK_HTTP_PORT"
  fi

  ensure_db
  build_server
  ensure_schema
  seed_dev_identity
  note "starting taskengine-server on 127.0.0.1:$HTTP_PORT"
  start_server
  note "console   : http://127.0.0.1:$HTTP_PORT/  (bearer token: tools/dev-stack.sh token)"
  note "database  : 127.0.0.1:$DB_PORT user=$DB_USER db=$DB_NAME (fixture DSN: tools/dev-stack.sh env)"
  note "state     : $STATE_DIR"
  note "server pid: $(cat "$PID_FILE")"
}

cmd_stop() {
  local pid attempt
  if pid="$(server_pid)"; then
    note "stopping server pid $pid (SIGTERM, 15s drain)"
    kill -TERM "$pid" 2>/dev/null || true
    for attempt in $(seq 1 75); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    if kill -0 "$pid" 2>/dev/null; then
      note "server did not exit within 15s; sending SIGKILL"
      kill -KILL "$pid" 2>/dev/null || true
      for attempt in $(seq 1 25); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.2
      done
    fi
  fi
  rm -f "$PID_FILE"

  if [[ "$(container_state)" == "running" ]]; then
    note "stopping database container $CONT_NAME"
    docker stop -t 20 "$CONT_NAME" >/dev/null
  fi
  note "stack stopped; data volume $VOL_NAME and $STATE_DIR are retained"
}

cmd_status() {
  local state pid schema code
  state="$(container_state)"
  echo "instance : $INSTANCE"
  echo "state    : $STATE_DIR"
  if pid="$(server_pid)"; then
    code="$(http_code "http://127.0.0.1:$HTTP_PORT/")"
    echo "server   : running pid $pid, GET / -> ${code:-unreachable}"
  else
    echo "server   : stopped"
  fi
  if [[ "$state" == "running" ]]; then
    schema="$(docker exec "$CONT_NAME" psql -U "$DB_USER" -d "$DB_NAME" -Atc 'SELECT max(version) FROM core.schema_version' 2>/dev/null || true)"
    echo "database : running $CONT_NAME on 127.0.0.1:$DB_PORT, schema=${schema:-not initialized}"
  else
    echo "database : ${state:-absent} ($CONT_NAME)"
  fi
  echo "console  : http://127.0.0.1:$HTTP_PORT/"
}

cmd_logs() {
  if [[ -n "$(container_state)" ]]; then
    echo "--- $CONT_NAME ---"
    docker logs --tail 50 "$CONT_NAME" 2>&1 || true
  else
    echo "--- $CONT_NAME: absent ---"
  fi
  echo "--- server.log ---"
  if [[ -f "$SERVER_LOG" ]]; then
    tail -n 50 "$SERVER_LOG" || true
  else
    echo "no server log yet"
  fi
}

require_state_files() {
	for file in "$PASSWORD_FILE" "$TOKEN_FILE" "$RUNTIME_TOKEN_FILE" "$DSN_FILE" "$RESOLVER_FILE"; do
		[[ -s "$file" ]] || die "missing $file; run tools/dev-stack.sh start first"
	done
}
cmd_env() {
	require_state_files
	printf 'export BBA_TASKENGINE_DSN=%q\n' "$(cat "$DSN_FILE")"
	printf 'export BBA_DEV_STACK_CONSOLE_URL=%q\n' "http://127.0.0.1:$HTTP_PORT/"
	printf 'export BBA_DEV_STACK_TOKEN=%q\n' "$(cat "$TOKEN_FILE")"
	printf 'export BBA_DEV_STACK_RUNTIME_TOKEN=%q\n' "$(cat "$RUNTIME_TOKEN_FILE")"
}
cmd_token() {
	[[ -s "$TOKEN_FILE" ]] || die "missing $TOKEN_FILE; run tools/dev-stack.sh start first"
	cat "$TOKEN_FILE"
	echo
}

cmd_runtime_token() {
	[[ -s "$RUNTIME_TOKEN_FILE" ]] || die "missing $RUNTIME_TOKEN_FILE; run tools/dev-stack.sh start first"
	cat "$RUNTIME_TOKEN_FILE"
	echo
}

cmd_reset() {
  cmd_stop
  if docker inspect "$CONT_NAME" >/dev/null 2>&1; then
    note "removing database container $CONT_NAME"
    docker rm -f "$CONT_NAME" >/dev/null
  fi
  if volume_exists; then
    note "removing data volume $VOL_NAME"
    docker volume rm "$VOL_NAME" >/dev/null
  fi
  case "$STATE_DIR" in
    "$ROOT"/build/dev-stack/*)
      note "removing state directory $STATE_DIR"
      rm -rf -- "$STATE_DIR"
      ;;
    *)
      note "state directory $STATE_DIR is outside $ROOT/build/dev-stack; delete it manually"
      ;;
  esac
  note "reset complete"
}

case "${1:-}" in
  start) cmd_start ;;
  stop) cmd_stop ;;
  status) cmd_status ;;
  logs) cmd_logs ;;
  env) cmd_env ;;
  token) cmd_token ;;
  runtime-token) cmd_runtime_token ;;
  reset) cmd_reset ;;
  *) usage ;;
esac
