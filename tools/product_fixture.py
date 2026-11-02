#!/usr/bin/env python3
"""Owned offline product stack and semantic evidence checks for product-acceptance."""
from __future__ import annotations

import hashlib
import json
import os
import secrets
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
import io
import tarfile


def load(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def fetch(origin, path, token, method="GET", body=None, key=None):
    headers = {"Authorization": "Bearer " + token}
    if body is not None:
        headers["Content-Type"] = "application/json"
    if key:
        headers["Idempotency-Key"] = key
    request = urllib.request.Request(origin + path, data=json.dumps(body).encode() if body is not None else None,
                                     headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)


def go_passes(package):
    """Only structured Go runtime results can satisfy permanent failure scenarios."""
    records = []
    for line in (package / "logs/go-race.stdout.log").read_text().splitlines():
        try:
            value = json.loads(line)
        except ValueError:
            continue
        if value.get("Action") == "pass" and value.get("Test"):
            records.append(value["Test"])
    return set(records)


GO_CASES = {
    "go-race": {"TestProposalWorkerDispatchAndTrustedObservationLink", "TestLoginCancelFencesLatePollAndReplays"},
    "graph-protocol": {"TestGoalsRejectsMalformedWire", "TestWriteUpstreamErrorIsUnknownAndNotRetried"},
    "login-protocol": {"TestLoginSessionFixtureLifecycleProjectsVerifiedIdentity", "TestLoginCancelFencesLatePollAndReplays",
                       "TestLoginIdentityVerificationGatesProjection", "TestLoginIdempotentReplaysNeverAdvanceOrRecallProvider"},
    "model-protocol": {"TestModelConfigurationRejectsAmbiguityAndExposedSecrets", "TestModelConcurrencyWaitHonorsCallerDeadline"},
}


def proof_checks(name, package):
    if name in GO_CASES:
        passed = go_passes(package)
        checks = {test: test in passed for test in sorted(GO_CASES[name])}
        if name == "model-protocol":
            smoke = load(package / "model-http.json")
            checks.update(smoke.get("checks", {}))
        return checks
    if name == "proposal-chain":
        result = load(package / "product-scenario.json")
        scene = result.get("scenario", result)
        trace = scene.get("trace", {})
        return {
            "actual_scenario": result.get("ok") is True and bool(scene.get("checks")) and all(c.get("ok") is True for c in scene["checks"]),
            "trusted_causal_chain": scene.get("work_item_state") == "SUCCEEDED" and scene.get("attempt_outcome") == "SUCCEEDED" and bool(scene.get("observation_event_id")) and bool(scene.get("execution_id")),
            "immutable_proposal": bool(trace.get("proposal_id")) and bool(trace.get("materialization_id")) and bool(trace.get("application_id")) and bool(trace.get("step_id")),
            "unknown_no_resend": scene.get("unknown", {}).get("work_state") == "UNKNOWN" and scene.get("unknown", {}).get("adapter_calls_after_retick") == 0 and scene.get("unknown", {}).get("executions_total") == 2,
            "no_send_at_materialize": scene.get("idempotency", {}).get("executions_before_worker") == 0 and scene.get("idempotency", {}).get("permits_before_worker") == 0 and scene.get("idempotency", {}).get("claims_before_worker") == 0,
            "approval_ref_bound": bool(scene.get("trace", {}).get("approval_ref")) and bool(scene.get("trace", {}).get("proposal_digest")),
        }
    if name == "graph-chain":
        result = load(package / "product-scenario.json")
        scene = result.get("scenario", result)
        graph = scene.get("graph", {})
        cross = load(package / "cross-module.json")
        actual = cross.get("graph", {})
        return {
            "real_collector_source": graph.get("summary_source") == "collector" and len(graph.get("summary_manifest", "")) == 64,
            "summary_identity": bool(actual.get("summary_id")) and graph.get("summary_id") == actual.get("summary_id") and graph.get("summary_version") == actual.get("summary_version"),
            "window_identity": bool(actual.get("window_id")) and graph.get("summary_window_id") == actual.get("window_id"),
            "dual_graph_consumer": graph.get("provider") == "artex" and graph.get("external_nodes", 0) > 0 and graph.get("anchors", 0) > 0 and graph.get("goals_created") == 1,
            "result_updates_goal": graph.get("goals_updated", 0) >= 1 and graph.get("checkpoint_offset_end", 0) > graph.get("checkpoint_offset_start", 0),
            "snapshot_evidence": bool(graph.get("snapshot_id")) and len(graph.get("snapshot_digest", "")) == 64 and graph.get("evidence_refs", 0) > 0,
            "trusted_work_result": graph.get("work_state") == "SUCCEEDED" and bool(graph.get("observation_event_id")) and graph.get("external_goals") == 1,
        }
    if name == "cross-module":
        value = load(package / "cross-module.json")
        names = {c.get("name") for c in value.get("checks", []) if c.get("ok") is True}
        required = {"collector_windows", "summary_publish", "group_context_live", "character_prompt", "permit_gate", "unknown_no_retry", "duplicate_window", "expired_summary", "restart", "quality_frozen", "graph_summary"}
        return {"real_modules": value.get("schema_id") == "CrossModuleEvidence.v1" and value.get("ok") is True,
                "scope_isolation": value.get("accounts") == ["cross-a", "cross-b"] and value.get("groups") == ["21001", "21002"],
                "all_boundaries": required.issubset(names),
                "fixture_only": value.get("fixture_only") is True and value.get("live_qq") is False and value.get("live_model") is False,
                "checks_pass": bool(value.get("checks")) and all(c.get("ok") is True for c in value["checks"])}
    if name == "browser":
        value = load(package / "browser/report.json")
        screenshots = value.get("screenshots", [])
        return {"actual_browser": value.get("ok") is True and value.get("state") == "PASS",
                "all_interactions": bool(value.get("checks")) and all(c.get("ok") is True for c in value["checks"]),
                "visual_evidence": len(screenshots) >= 8 and all((package / "browser" / s["file"]).is_file() for s in screenshots),
                "real_summary_report": value.get("report_context_observed", {}).get("source") == "collector.summary.v1",
                "real_server_reads": any(r.get("source") == "server" and r.get("status") == 200 for r in value.get("requests", []))}
    if name == "restart-restore":
        value = load(package / "restore/report.json")
        drill = load(package / "restore/drill-summary.json")
        restored = drill.get("restore", {})
        restart = drill.get("restart", {})
        return {"actual_restore": value.get("state") == "PASS" and value.get("driver", {}).get("exit_code") == 0,
                "schema_and_data": restored.get("restored_matches_source") is True,
                "no_resend": restored.get("adapter_calls_after_restore") == 0 and restart.get("adapter_calls_after_restore") == 0,
                "unknown_retained": restart.get("recovered_dispatching") == 1 and restart.get("reconciled_timeouts") == 3,
                "no_claimable_uncertainty": restored.get("work_items_claimable") == 0 and restored.get("expired_work_item_claims") == 0,
                "owned_cleanup": value.get("resources", {}).get("owned_resources_removed") is True}
    if name == "quality-holdout":
        value = load(package / "cross-module.json")["quality"]
        return {"explicit_fixture_limit": value.get("real_model_evaluated") is False and value.get("status") == "frozen_fixture_only_not_real_quality",
                "frozen_inputs": all(len(value.get(key, "")) == 64 for key in ("holdout_sha256", "rubric_sha256", "provenance_sha256"))}
    if name == "resources":
        value = load(package / "resources.json")
        return {"owned_removed": value.get("owned_resources_removed") is True,
                "preexisting_preserved": value.get("preexisting_preserved") is True,
                "isolated": value.get("production_dsn_used") is False}
    if name == "release-blocked":
        value = load(package / "release-blocked.json")
        return {"explicit_block": value.get("state") == "RELEASE_BLOCKED" and value.get("exit_code") == 3 and bool(value.get("problems"))}
    if name == "source-baseline":
        value = load(package / "source-baseline.json")
        delta = value.get("source_delta", {})
        return {"history_unchanged": value.get("history_unchanged") is True,
                "source_stable_during_fixture": value.get("source_stable_during_fixture") is True,
                "baseline_present": (package / "baseline/source-before.tar.gz").is_file(),
                "source_delta_verified": bool(delta.get("added") or delta.get("changed") or delta.get("deleted")) and value.get("baseline_delta_sha256") == hashlib.sha256(json.dumps(delta, sort_keys=True).encode()).hexdigest()}
    if name == "evidence-negative":
        value = load(package / "evidence-negative.json")
        cases = value.get("cases", [])
        return {"all_tampering_rejected": value.get("ok") is True and len(cases) >= 3 and all(c.get("detected") is True for c in cases),
                "original_unchanged": value.get("original_unchanged") is True,
                "business_proof_rechecked": any(c.get("name") == "business-proof-deleted-and-rehashed" and c.get("detected") is True for c in cases)}
    if name in ("schema", "contract", "node22"):
        stdout = (package / f"logs/{name}.stdout.log").read_text()
        if name == "schema": return {"actual_migrations": "INSERT 0 1" in stdout}
        if name == "contract": return {"generated_contracts": "generation + type smoke OK" in stdout}
        return {"both_packages_built_and_exercised": "verify finished" in stdout and "# fail 0" in stdout and "character-runtime" in stdout and "analysis-collector" in stdout}
    raise ValueError("unknown scenario " + name)


def require_checks(name, package):
    checks = proof_checks(name, package)
    if not checks or not all(value is True for value in checks.values()):
        raise RuntimeError(f"{name} business evidence failed: {checks}")
    return checks


def run_fixture(api, output):
    root = api.ROOT
    if output.exists():
        raise ValueError("fixture output must be a new directory")
    for key in ("BBA_TASKENGINE_DSN", "BBA_DATABASE_URL", "BBA_PRODUCTION_DSN", "DATABASE_URL", "PGPASSWORD"):
        if os.environ.get(key):
            raise ValueError("fixture refuses inherited live connection configuration: " + key)
    output.mkdir(parents=True)
    recorder = api.Recorder(output)
    started = api.utc()
    problems = []
    services = []
    run_id = "bba-product-" + uuid.uuid4().hex[:16]
    before = set(subprocess.check_output(["docker", "ps", "-aq"], text=True).split())
    created = False
    secret_dir = tempfile.TemporaryDirectory(prefix=run_id + "-")
    secret_path = Path(secret_dir.name)
    source_start = {name: api.file_hash(root / name) for name in api.HASHES.source_scope(root)}
    baseline = root / "evals/results/bba-product-closeout-20261030T133540Z/baseline"
    baseline_source = {}
    with tarfile.open(baseline / "source-before.tar.gz", "r:gz") as archive:
        for member in archive.getmembers():
            if member.isfile() and api.HASHES.is_source_name(Path(member.name).name):
                stream = archive.extractfile(member)
                if stream is not None:
                    baseline_source[member.name.removeprefix("./")] = hashlib.sha256(stream.read()).hexdigest()
    if not baseline_source:
        raise RuntimeError("development-before source snapshot yielded no source files")
    shutil.copytree(baseline, output / "baseline")

    def command(label, argv, **kwargs):
        if recorder.run(label, argv, **kwargs) != 0:
            raise RuntimeError(label + " failed; raw streams retained")

    def service(label, argv, env):
        stdout = (output / "logs" / (label + ".stdout.log")).open("xb")
        stderr = (output / "logs" / (label + ".stderr.log")).open("xb")
        row = {"label": label, "argv": list(map(str, argv)), "start_utc": api.utc(), "stdout": f"logs/{label}.stdout.log", "stderr": f"logs/{label}.stderr.log"}
        process = subprocess.Popen(argv, cwd=root, env={**os.environ, **env}, stdout=stdout, stderr=stderr, start_new_session=True)
        services.append((process, stdout, stderr, row))
        return process

    def wait_http(origin, path, token, process):
        end = time.monotonic() + 20
        while time.monotonic() < end:
            if process.poll() is not None:
                raise RuntimeError("fixture service exited before readiness")
            try:
                status, _ = fetch(origin, path, token)
                if status == 200: return
            except (OSError, ValueError): pass
            time.sleep(0.1)
        raise RuntimeError("fixture HTTP readiness timed out")

    def prove(name, evidence):
        checks = require_checks(name, output)
        api.write_json(output / "scenarios" / (name + ".json"), {"state": "PASS", "assertions": checks, "evidence": evidence})

    try:
        command("schema", ["bash", "tools/kernel.sh", "schema"])
        command("go-race", ["bash", "tools/kernel.sh", "test-all", "-json"])
        prove("go-race", ["logs/go-race.stdout.log"])
        command("contract", ["bash", "tools/contract.sh", "generate"])
        command("node22", ["bash", "tools/node22-deps.sh", "verify"], env={"BBA_NODE_OFFLINE": "1"})
        for name in ("schema", "contract", "node22", "graph-protocol", "login-protocol"):
            prove(name, [f"logs/{name if name in ('schema','contract','node22') else 'go-race'}.stdout.log"])
        # Protocol-specific postchecks independently parse structured runtime results.
        for name in ("graph-protocol", "login-protocol"):
            command(name, ["python3", "tools/product_fixture.py", "proof", name, str(output)])
        build = root / "build/product"
        build.mkdir(parents=True, exist_ok=True)
        for binary in ("taskengine-server", "taskengine-migrate", "product-fixture"):
            command("build-" + binary, ["docker", "run", "--rm", "--network", "none", "--user", f"{os.getuid()}:{os.getgid()}",
                "-e", "GOPATH=/work/.cache/gopath", "-e", "GOMODCACHE=/work/.cache/gomod", "-e", "GOCACHE=/work/.cache/gobuild",
                "-e", "GOPROXY=off", "-e", "GOSUMDB=off", "-e", "GOFLAGS=-mod=readonly", "-v", f"{root}:/work", "-w", "/work/backend",
                "golang:1.26", "go", "build", "-o", "/work/build/product/" + binary, "./cmd/" + binary])
        pgport, httpport = free_port(), free_port()
        command("postgres-create", ["docker", "run", "-d", "--rm", "--name", run_id, "--label", "bba.product.run=" + run_id,
            "-p", f"127.0.0.1:{pgport}:5432", "--tmpfs", "/var/lib/postgresql/data:rw", "-e", "POSTGRES_USER=bba_fixture",
            "-e", "POSTGRES_DB=bba_fixture_admin", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:16-bookworm"])
        created = True
        for _ in range(200):
            ready = subprocess.run(["docker", "exec", run_id, "psql", "-U", "bba_fixture", "-d", "bba_fixture_admin", "-Atc", "SELECT 1"], capture_output=True)
            if ready.returncode == 0: break
            time.sleep(0.1)
        else: raise RuntimeError("isolated postgres not ready")
        dsn = f"postgres://bba_fixture@127.0.0.1:{pgport}/bba_fixture_admin?sslmode=disable"
        db_env = {"BBA_TASKENGINE_DSN": dsn, "BBA_PRODUCT_FIXTURE": "1"}
        command("database-init", [str(build / "taskengine-migrate")], env=db_env)
        command("seed", [str(build / "product-fixture"), "-fixture", "-mode", "seed", "-output", str(output / "seed.json")], env=db_env)
        seed = load(output / "seed.json")
        seed = seed.get("seed", seed)
        operator, reader, token_a, token_b = [secrets.token_hex(32) for _ in range(4)]
        def credential(token, principal, permissions, **extra):
            return {"token_sha256": hashlib.sha256(token.encode()).hexdigest(), "workspace_id": "ws_fixture", "principal_id": principal, "permissions": permissions, **extra}
        credentials = {"credentials": [
            credential(operator, "product-operator", ["operator.read", "task.read", "task.manage_all", "task.control", "task.create", "task.approve", "model.request"]),
            credential(reader, seed["principal_id"], ["task.read"]),
            credential(token_a, "", [], runtime_id="cross-runtime-a", owner_epoch=1, account_ids=["cross-a"]),
            credential(token_b, "", [], runtime_id="cross-runtime-b", owner_epoch=1, account_ids=["cross-b"])]}
        resolver = secret_path / "resolver.json"
        resolver.write_text(json.dumps(credentials)); resolver.chmod(0o600)
        origin = f"http://127.0.0.1:{httpport}"
        server = service("server", [str(build / "taskengine-server"), "-listen", f"127.0.0.1:{httpport}", "-origin", origin,
            "-resolver-file", str(resolver), "-development-operations", "-proposal-execution", "-login-fixture", "-model-fixture"], db_env)
        wait_http(origin, "/v1/operator/runtime", operator, server)
        task_id = seed["browser_task_id"]
        status, task = fetch(origin, "/v1/tasks/" + task_id, operator)
        if status != 200: raise RuntimeError("model task read failed")
        task = task["data"]
        model_request = {"request_id": "fixture_model_request", "operation_kind": "complex_plan",
            "task_ref": {"id": task_id, "version": task["version"], "scope_revision": task["scope_revision"]},
            "route_policy": "l2_prefer_online", "privacy_class": "redacted_external_allowed", "required_capabilities": ["structured_output"],
            "schema_id": "PlanProposal.v1", "prompt_version": "planning.v1", "input_ref": task_id, "max_provider_attempts": 1,
            "deadline_at": datetime.fromtimestamp(time.time()+60, timezone.utc).isoformat().replace("+00:00", "Z")}
        first, response = fetch(origin, "/v1/model-requests", operator, "POST", model_request, "fixture-model-1")
        second, replay = fetch(origin, "/v1/model-requests", operator, "POST", model_request, "fixture-model-1")
        denied, _ = fetch(origin, "/v1/model-requests", reader, "POST", model_request, "reader-model")
        model_result = response.get("data", {}).get("result", {})
        model_proposal = model_result.get("PlanProposal", model_result.get("plan_proposal", model_result)) if isinstance(model_result, dict) else {}
        model_checks = {"http_model_completed": first == 202, "same_key_replay": second == 202 and response.get("data") == replay.get("data"),
                        "read_actor_denied": denied == 403, "model_grants_no_work": model_proposal.get("proposal_kind") == "request_clarification" and model_proposal.get("steps") == []}
        api.write_json(output / "model-http.json", {"checks": model_checks, "requests": {"first_status": first, "replay_status": second, "denied_status": denied}, "response": response, "replay": replay})
        command("model-protocol", ["python3", "tools/product_fixture.py", "proof", "model-protocol", str(output)])
        prove("model-protocol", ["model-http.json", "logs/go-race.stdout.log"])
        cross_env = {"BBA_CROSS_FIXTURE": "1", "BBA_CROSS_BACKEND_URL": origin, "BBA_CROSS_WORKSPACE_ID": "ws_fixture",
            "BBA_CROSS_ACCOUNT_A_ID": "cross-a", "BBA_CROSS_ACCOUNT_A_SELF_ID": "11001", "BBA_CROSS_ACCOUNT_A_RUNTIME_ID": "cross-runtime-a", "BBA_CROSS_ACCOUNT_A_TOKEN": token_a,
            "BBA_CROSS_ACCOUNT_B_ID": "cross-b", "BBA_CROSS_ACCOUNT_B_SELF_ID": "11002", "BBA_CROSS_ACCOUNT_B_RUNTIME_ID": "cross-runtime-b", "BBA_CROSS_ACCOUNT_B_TOKEN": token_b,
            "BBA_CROSS_GROUP_A": "21001", "BBA_CROSS_GROUP_B": "21002", "BBA_CROSS_GRAPH_GROUP": "24001",
            "BBA_CROSS_STATE_DIR": str(secret_path / "cross-state"), "BBA_CROSS_RESULT": str(output / "cross-module.json"), "BBA_NODE_STAGE": str(root / "build/node22-stage")}
        # The cached Node22 image runs host-network only for this owned loopback fixture.
        node_prefix = ["docker", "run", "--rm", "--network", "host", "--user", f"{os.getuid()}:{os.getgid()}", "-v", f"{root}:{root}", "-v", f"{secret_path}:{secret_path}", "-w", str(root)]
        for key in cross_env: node_prefix += ["-e", key]
        command("cross-module", node_prefix + ["node:22", "node", "tools/product-cross-module.cjs"], env=cross_env)
        prove("cross-module", ["cross-module.json"])
        prove("quality-holdout", ["cross-module.json"])
        command("proposal-chain", [str(build / "product-fixture"), "-fixture", "-mode", "scenario", "-require-collector-summary", "-output", str(output / "product-scenario.json")], env=db_env)
        prove("proposal-chain", ["product-scenario.json"])
        command("graph-chain", ["python3", "tools/product_fixture.py", "proof", "graph-chain", str(output)])
        prove("graph-chain", ["product-scenario.json", "cross-module.json"])
        browser_env = {"BBA_BROWSER_URL": origin, "BBA_BROWSER_TOKEN": operator, "BBA_BROWSER_READ_TOKEN": reader,
            "BBA_BROWSER_TASK_ID": seed["browser_task_id"], "BBA_BROWSER_APPROVAL_REF": seed["browser_approval_ref"],
            "BBA_BROWSER_ACCOUNT_ID": "cross-a", "BBA_BROWSER_GROUP_ID": "21001", "BBA_BROWSER_OUTPUT": str(output / "browser")}
        node = shutil.which("node") or "/home/sun/.hermes/tools/node-26.7.0-linux-x64/bin/node"
        command("browser", [node, "tools/product-browser.cjs"], env=browser_env)
        prove("browser", ["browser/report.json"])
        command("restart-restore", ["bash", "tools/product-restore.sh", "--output", str(output / "restore")])
        prove("restart-restore", ["restore/report.json", "restore/drill-summary.json"])
        command("evidence-negative", ["python3", "-m", "unittest", "discover", "-s", "tests/product", "-p", "test_*.py", "-v"])
    except Exception as error:
        problems.append(str(error))
    finally:
        for process, stdout, stderr, row in reversed(services):
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try: process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL); process.wait()
            stdout.close(); stderr.close()
            row.update(end_utc=api.utc(), exit_code=process.returncode)
            api.write_json(output / "logs" / (row["label"] + ".service.json"), row)
        if created or run_id:
            owned = subprocess.run(["docker", "ps", "-aq", "--filter", "label=bba.product.run=" + run_id], capture_output=True, text=True)
            owned_ids = owned.stdout.split() if owned.returncode == 0 else []
            for container_id in owned_ids:
                subprocess.run(["docker", "rm", "-f", container_id], capture_output=True, text=True, check=False)
        after = set(subprocess.check_output(["docker", "ps", "-aq"], text=True).split())
        leftovers = subprocess.check_output(["docker", "ps", "-aq", "--filter", "label=bba.product.run=" + run_id], text=True).split()
        secret_dir.cleanup()
        api.write_json(output / "resources.json", {"run_id": run_id, "owned_resources_removed": not leftovers,
            "preexisting_preserved": before.issubset(after), "production_dsn_used": False, "leftovers": leftovers})
    source_end = {name: api.file_hash(root / name) for name in api.HASHES.source_scope(root)}
    with (output / "source-files.sha256").open("x") as handle:
        for name, sha in sorted(source_end.items()): handle.write(f"{sha}  ./{name}\n")
    old_inventory = load(baseline / "history-inventory.json")
    current_inventory = {}
    for rel in old_inventory:
        base = root / "evals/results" / rel
        current_inventory[rel] = {str(p.relative_to(base)): api.file_hash(p) for p in base.rglob("*") if p.is_file()}
    unchanged = current_inventory == old_inventory
    source_delta = {"added": sorted(set(source_end) - set(baseline_source)),
                    "changed": sorted(name for name in set(source_end) & set(baseline_source) if source_end[name] != baseline_source[name]),
                    "deleted": sorted(set(baseline_source) - set(source_end))}
    source_delta_hash = hashlib.sha256(json.dumps(source_delta, sort_keys=True).encode()).hexdigest()
    api.write_json(output / "source-baseline.json", {"history_unchanged": unchanged,
        "source_stable_during_fixture": source_start == source_end, "source_delta": source_delta,
        "baseline_delta_sha256": source_delta_hash,
        "baseline_file_count": len(baseline_source), "current_file_count": len(source_end)})
    api.write_json(output / "release-blocked.json", {"state": "RELEASE_BLOCKED", "exit_code": 3, "problems": api.release_problems(output)})
    for name, evidence in (("resources", ["resources.json"]), ("release-blocked", ["release-blocked.json"]), ("source-baseline", ["source-baseline.json"])):
        try: prove(name, evidence)
        except Exception as error: problems.append(str(error))
    api.write_json(output / "steps.json", recorder.steps)
    try:
        negative_step = next((row for row in recorder.steps if row["label"] == "evidence-negative"), {})
        negative_log = (output / negative_step["stderr"]).read_text(encoding="utf-8") if negative_step.get("stderr") else ""
        def unittest_passed(test_name):
            return any(line.startswith(test_name + " (") and line.rstrip().endswith("... ok") for line in negative_log.splitlines())
        negative_tests = [
            {"name": "business-proof-deleted-and-rehashed", "detected": unittest_passed("test_rehashed_missing_business_proof_is_rejected")},
            {"name": "business-assertion-forged-and-rehashed", "detected": unittest_passed("test_rehashed_business_assertion_forgery_is_rejected")},
            {"name": "browser-screenshot-deleted-and-rehashed", "detected": unittest_passed("test_rehashed_missing_browser_screenshot_is_rejected")}
        ]
        if negative_step.get("exit_code") != 0:
            raise RuntimeError("permanent evidence-negative tests failed")
        negative_evidence = {"ok": all(test["detected"] for test in negative_tests),
            "original_unchanged": source_start == source_end and unchanged,
            "cases": [{"name": test["name"], "detected": test["detected"]} for test in negative_tests]}
        api.write_json(output / "evidence-negative.json", negative_evidence)
        prove("evidence-negative", ["evidence-negative.json"])
    except Exception as error:
        problems.append(str(error))
    rows = [{"id": name, "status": "PASS" if not problems else "FAIL", "scenarios": list(scenarios),
             "evidence": [f"scenarios/{scenario}.json" for scenario in scenarios]} for name, scenarios in api.REQUIREMENTS.items()]
    api.write_json(output / "acceptance.json", {"mode": "fixture", "state": "ENGINEERING_REJECTED" if problems else "ENGINEERING_ACCEPTED",
        "start_utc": started, "end_utc": api.utc(), "requirements": rows, "problems": problems, "release_state": "RELEASE_BLOCKED"})
    api.write_package_manifest(output)
    verdict = api.verify_package(root, output)
    print(json.dumps(verdict, ensure_ascii=False, indent=2))
    return 0 if verdict["ok"] else 1


def main():
    import sys
    if len(sys.argv) != 4 or sys.argv[1] != "proof":
        raise SystemExit("usage: product_fixture.py proof SCENARIO PACKAGE")
    checks = require_checks(sys.argv[2], Path(sys.argv[3]))
    print(json.dumps({"scenario": sys.argv[2], "checks": checks}, ensure_ascii=False))


if __name__ == "__main__":
    main()
