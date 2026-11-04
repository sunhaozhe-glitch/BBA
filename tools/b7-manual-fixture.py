#!/usr/bin/env python3
"""B7 manual acceptance fixture: temporary PostgreSQL 16 + the real compiled
taskengine-server with the embedded product console, seeded with dual-view
data so a human can open the page and operate it over real HTTP.

usage:
  tools/b7-manual-fixture.py start --output evals/results/<new-dir>
  tools/b7-manual-fixture.py stop  --output evals/results/<dir>
  tools/b7-manual-fixture.py status --output evals/results/<dir>

Safety model (mirrors tools/dev-stack.sh and tools/kernel.sh):
  * --output must be a brand-new directory under evals/results/.
  * Inherited BBA_TASKENGINE_DSN / DATABASE_URL / production connection
    configuration is refused, never silently reused.
  * Random loopback-only HTTP and DB ports; 5432 and the 184xx service ports
    are refused so a running deployment is never touched.
  * Random operator/reader/runtime tokens live only in 0600 files inside the
    output directory; stdout prints URL and token file paths, not tokens.
  * Only containers/containers labelled as this instance are cleaned up.
  * The service stays up; Ctrl-C (or the stop command) drains it and removes
    exactly the resources this launcher created, then reads back the cleanup.
"""
import argparse
import hashlib
import json
import os
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PG_IMAGE = "postgres:16-alpine"
GO_IMAGE = "golang:1.26"
LABEL_KEY = "bba.b7-manual-fixture"
CONTAINER_PREFIX = "bba-b7fix-"
# Ports owned by live/other services; the fixture must never claim or reuse them.
REFUSED_PORTS = {5432, 18421, 18431, 18432, 18451, 18452}
INHERITED_DSN_VARS = ("BBA_TASKENGINE_DSN", "DATABASE_URL")
SEED_SQL = ROOT / "tools" / "b7-manual-fixture-seed.sql"


def die(msg):
    print(f"b7-manual-fixture: {msg}", file=sys.stderr)
    sys.exit(1)


# --- guards (pure, unit-tested in tests/product/test_b7_manual_fixture_launcher.py) ---

def guard_output_dir(output, root=ROOT):
    """The output directory must be a NEW directory under <root>/evals/results."""
    out = Path(output)
    if not out.is_absolute():
        out = root / out
    results = (root / "evals" / "results").resolve()
    resolved = out.resolve()
    if resolved.parent != results:
        raise ValueError(f"--output must be a direct child of {results}")
    if out.exists():
        raise ValueError(f"--output must not exist yet: {out}")
    return out


def guard_inherited_dsn(env):
    """No inherited BBA_TASKENGINE_DSN/DATABASE_URL may leak into the fixture."""
    for var in INHERITED_DSN_VARS:
        if env.get(var):
            raise ValueError(f"{var} is set in the environment; unset it before starting the fixture")


def guard_port(port):
    port = int(port)
    if port in REFUSED_PORTS:
        raise ValueError(f"port {port} belongs to a live or default service; the fixture uses random loopback ports")
    if port < 1024 or port > 65535:
        raise ValueError(f"port {port} is outside the allowed range")
    return port


def guard_loopback(host):
    if host != "127.0.0.1":
        raise ValueError(f"listener must be loopback, got {host}")
    return host


def guard_token_file_mode(path):
    mode = Path(path).stat().st_mode & 0o777
    if mode != 0o600:
        raise ValueError(f"token file {path} must be mode 0600, got {oct(mode)}")


def guard_resolver_file(path):
    """The resolver file holds only SHA-256 digests; plaintext tokens never land in it."""
    data = json.loads(Path(path).read_text())
    for cred in data["credentials"]:
        if not re.fullmatch(r"[0-9a-f]{64}", cred["token_sha256"]):
            raise ValueError("resolver token_sha256 must be a lowercase SHA-256 hex digest")
        for key in ("token", "plaintext_token"):
            if key in cred:
                raise ValueError(f"resolver file must not carry a plaintext field: {key}")


def guard_owned_container(name, labels):
    if not name.startswith(CONTAINER_PREFIX):
        raise ValueError(f"refusing to touch container {name}: not owned by this fixture")
    if labels.get(LABEL_KEY) is None:
        raise ValueError(f"refusing to touch container {name}: missing {LABEL_KEY} label")


def guard_owned_pid(pid, expected_cmdline_fragment):
    try:
        cmdline = Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\0", b" ").decode(errors="replace")
    except OSError:
        return False
    return expected_cmdline_fragment in cmdline


def server_args(port, resolver_file):
    """Assembled server flags: no -worker, no -development-operations, no live services."""
    guard_port(port)
    return ["-listen", f"127.0.0.1:{port}", "-origin", f"http://127.0.0.1:{port}", "-resolver-file", str(resolver_file)]


def pick_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    return guard_port(port)


def sha256_hex(text):
    return hashlib.sha256(text.encode()).hexdigest()


def rand_token():
    return secrets.token_hex(32)


def sh(cmd, check=True, capture=True, env=None, stdin_file=None):
    stdin = open(stdin_file, "rb") if stdin_file else None
    try:
        result = subprocess.run(cmd, check=check, capture_output=capture, text=True, env=env, stdin=stdin)
    except subprocess.CalledProcessError as exc:
        print(f"b7-manual-fixture: command failed: {cmd}\n  stdout: {exc.stdout}\n  stderr: {exc.stderr}", file=sys.stderr)
        raise
    finally:
        if stdin:
            stdin.close()
    return result


def write_private(path, text):
    path = Path(path)
    path.write_text(text if text.endswith("\n") else text + "\n")
    path.chmod(0o600)
    guard_token_file_mode(path)


# --- fixture lifecycle -----------------------------------------------------

def state_path(out):
    return Path(out) / "state.json"


def load_state(out):
    state = json.loads(Path(state_path(out)).read_text())
    return state


def read_tokens(out):
    tokens = {}
    for role in ("operator", "reader", "runtime"):
        path = Path(out) / "tokens" / f"{role}-token"
        guard_token_file_mode(path)
        tokens[role] = path.read_text().strip()
    return tokens


def cmd_start(output, env=None):
    env = dict(os.environ if env is None else env)
    out = guard_output_dir(output, ROOT)
    guard_inherited_dsn(env)
    if not SEED_SQL.exists():
        die(f"missing {SEED_SQL}")
    out.mkdir(parents=True)
    out.chmod(0o755)
    instance = secrets.token_hex(4)
    http_port = pick_free_port()
    db_port = pick_free_port()
    container = f"{CONTAINER_PREFIX}{instance}"
    db_user = f"b7fix_{instance}"
    db_name = f"b7fix_{instance}_db"
    db_password = rand_token()
    instance_id = instance

    tokens = {role: rand_token() for role in ("operator", "reader", "runtime")}
    tokens_dir = out / "tokens"
    tokens_dir.mkdir()
    tokens_dir.chmod(0o700)
    for role, token in tokens.items():
        write_private(tokens_dir / f"{role}-token", token)

    resolver = {
        "credentials": [
            {"token_sha256": sha256_hex(tokens["operator"]), "workspace_id": "b7fix-ws",
             "principal_id": "op-b7fix",
             "permissions": ["conversation.read", "conversation.write", "notification.read"]},
            {"token_sha256": sha256_hex(tokens["reader"]), "workspace_id": "b7fix-ws",
             "principal_id": "op-b7fix-reader", "permissions": ["conversation.read", "notification.read"]},
            {"token_sha256": sha256_hex(tokens["runtime"]), "workspace_id": "b7fix-ws",
             "runtime_id": "rt-b7fix-2", "owner_epoch": 1,
             "account_ids": ["acct-qq-2"]},
        ]
    }
    resolver_file = out / "resolver.json"
    resolver_file.write_text(json.dumps(resolver, indent=1) + "\n")
    resolver_file.chmod(0o600)
    guard_resolver_file(resolver_file)

    # container: label ownership; random loopback publish only
    sh(["docker", "run", "-d", "--name", container,
        "--label", f"{LABEL_KEY}={instance_id}",
        "-e", f"POSTGRES_USER={db_user}", "-e", f"POSTGRES_DB={db_name}",
        "-e", f"POSTGRES_PASSWORD={db_password}",
        "-p", f"127.0.0.1:{db_port}:5432", "--restart", "no", PG_IMAGE])
    dsn = f"postgres://{db_user}:{db_password}@127.0.0.1:{db_port}/{db_name}?sslmode=disable"
    write_private(out / "dsn.txt", dsn)

    for _ in range(150):
        ready = sh(["docker", "exec", container, "pg_isready", "-h", "127.0.0.1", "-U", db_user, "-d", db_name], check=False)
        if ready.returncode == 0:
            break
        time.sleep(0.2)
    else:
        cleanup(out, state_path(out).with_name("state.json"))
        die("database did not become ready")

    # offline Go build from current sources (same cache pattern as dev-stack)
    sh(["docker", "run", "--rm", "--network", "none", "--user", f"{os.getuid()}:{os.getgid()}",
        "-e", "HOME=/tmp/home", "-e", "GOPATH=/work/.cache/gopath", "-e", "GOMODCACHE=/work/.cache/gomod",
        "-e", "GOCACHE=/work/.cache/gobuild", "-e", "GOPROXY=off", "-e", "GOSUMDB=off", "-e", "CGO_ENABLED=0",
        "-v", f"{ROOT}:/work", "-v", f"{out}:/out", "-w", "/work/backend", GO_IMAGE,
        "/bin/sh", "-c", "set -e; /usr/local/go/bin/go build -trimpath -o /out/bin/taskengine-migrate ./cmd/taskengine-migrate;"
        " /usr/local/go/bin/go build -trimpath -o /out/bin/taskengine-server ./cmd/taskengine-server"])

    # the server never migrates on startup; taskengine-migrate initializes only empty databases
    build_env = {k: v for k, v in env.items() if k not in INHERITED_DSN_VARS}
    build_env["BBA_TASKENGINE_DSN"] = dsn
    sh([str(out / "bin" / "taskengine-migrate")], env=build_env)

    sh(["docker", "exec", "-i", container, "psql", "-v", "ON_ERROR_STOP=1", "-U", db_user, "-d", db_name, "-f", "-"],
       stdin_file=str(SEED_SQL))
    counts = sh(["docker", "exec", container, "psql", "-U", db_user, "-d", db_name, "-Atc",
                 "SELECT (SELECT count(*) FROM core.operator_conversation)||'/'||"
                 "(SELECT count(*) FROM core.operator_message)||'/'||"
                 "(SELECT count(*) FROM core.qq_draft)||'/'||"
                 "(SELECT count(*) FROM core.operator_notification)"]).stdout.strip()
    if counts != "3/6/2/2":
        die(f"seed readback mismatch: {counts} (want 3/6/2/2)")

    log_file = out / "server.log"
    args = [str(out / "bin" / "taskengine-server")] + server_args(http_port, resolver_file)
    server_proc = subprocess.Popen(args, env=build_env,
                                   stdout=open(log_file, "a"), stderr=subprocess.STDOUT,
                                   start_new_session=True)
    state = {"instance": instance_id, "container": container, "http_port": http_port, "db_port": db_port,
             "server_pid": server_proc.pid, "output": str(out),
             "binary": str(out / "bin" / "taskengine-server"),
             "started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    state_path(out).write_text(json.dumps(state, indent=1) + "\n")

    config = {
        "origin": f"http://127.0.0.1:{http_port}",
        "product_url": f"http://127.0.0.1:{http_port}/product/chat",
        "workspace_id": "b7fix-ws",
        "accounts": ["acct-qq-1", "acct-qq-2"],
        "token_files": {role: str(tokens_dir / f"{role}-token") for role in tokens},
        "resolver_file": str(resolver_file),
        "dsn_file": str(out / "dsn.txt"),
        "db_container": container,
        "seed_sql": str(SEED_SQL),
        "server_log": str(log_file),
        "state_file": str(state_path(out)),
        "notes": "fixture-only service; real QQ is not connected and nothing is ever sent",
    }
    (out / "fixture-config.json").write_text(json.dumps(config, indent=1) + "\n")

    # health: the same origin a human would open, plus an authenticated real API read
    for _ in range(100):
        code = sh(["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "3",
                   config["product_url"]], check=False).stdout.strip()
        if code == "200":
            break
        time.sleep(0.2)
    else:
        die("server did not answer the product page; see server.log")
    code = sh(["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "3",
               "-H", f"Authorization: Bearer {tokens['operator']}",
               f"{config['origin']}/v1/operator/conversations"], check=False).stdout.strip()
    if code != "200":
        die(f"authenticated GET /v1/operator/conversations returned {code}")

    print(f"b7-manual-fixture: open {config['product_url']} (login token: {tokens_dir}/operator-token)")
    print(f"b7-manual-fixture: stop with Ctrl-C or tools/b7-manual-fixture.py stop --output {out}")

    # keep the service up; clean only what this launcher created
    stop_flag = {"signal": None}

    def on_sig(signum, _frame):
        stop_flag["signal"] = signum

    signal.signal(signal.SIGINT, on_sig)
    signal.signal(signal.SIGTERM, on_sig)
    while stop_flag["signal"] is None and server_proc.poll() is None:
        time.sleep(0.5)
    if stop_flag["signal"] is None and server_proc.poll() is not None:
        die(f"server exited early (code {server_proc.returncode}); see {log_file}")
    cleanup(out, state_path(out))
    return 0


def cleanup(out, state_file):
    state = json.loads(Path(state_file).read_text())
    guard_owned_container(state["container"], {LABEL_KEY: state["instance"]})
    pid = state["server_pid"]
    if pid and guard_owned_pid(pid, state["binary"]):
        try:
            os.kill(pid, signal.SIGTERM)
            for _ in range(75):
                if not os.path.exists(f"/proc/{pid}"):
                    break
                time.sleep(0.2)
            else:
                os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    sh(["docker", "stop", "-t", "20", state["container"]], check=False)
    sh(["docker", "rm", state["container"]], check=False)
    # read back: the container must be gone and the ports closed
    gone = sh(["docker", "inspect", state["container"]], check=False).returncode != 0
    closed = True
    for port in (state["http_port"], state["db_port"]):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.settimeout(1)
            if sock.connect_ex(("127.0.0.1", port)) == 0:
                closed = False
    report = {"container_gone": gone, "ports_closed": closed,
              "cleaned_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    Path(out).joinpath("cleanup.json").write_text(json.dumps(report, indent=1) + "\n")
    if not (gone and closed):
        die(f"cleanup verification failed: {report}")
    print(f"b7-manual-fixture: cleanup verified (container gone={gone}, ports closed={closed})")


def cmd_stop(output):
    out = Path(output)
    if not out.is_absolute():
        out = ROOT / out
    state = load_state(out)
    cleanup(out, state_path(out))
    return 0


def cmd_status(output):
    out = Path(output)
    if not out.is_absolute():
        out = ROOT / out
    state = load_state(out)
    alive = guard_owned_pid(state["server_pid"], state["binary"])
    print(json.dumps({"server_running": alive, "pid": state["server_pid"],
                      "origin": f"http://127.0.0.1:{state['http_port']}",
                      "container": state["container"]}, indent=1))


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["start", "stop", "status"])
    parser.add_argument("--output", required=True)
    args = parser.parse_args(argv)
    if args.command == "start":
        return cmd_start(args.output)
    if args.command == "stop":
        return cmd_stop(args.output)
    return cmd_status(args.output)


if __name__ == "__main__":
    sys.exit(main())
