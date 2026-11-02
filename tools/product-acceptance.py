#!/usr/bin/env python3
"""Isolated product acceptance, evidence verification, and fail-closed release gate."""
from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import shlex
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location("bba_evidence_hashes", ROOT / "tools/round4-evidence-verify.py")
HASHES = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(HASHES)
_FIXTURE_SPEC = importlib.util.spec_from_file_location("bba_product_fixture", ROOT / "tools/product_fixture.py")
FIXTURE = importlib.util.module_from_spec(_FIXTURE_SPEC)
_FIXTURE_SPEC.loader.exec_module(FIXTURE)

REQUIREMENTS = {
    "KERNEL-01": ("go-race", "proposal-chain"),
    "ACCOUNT-01": ("cross-module", "login-protocol"),
    "ACCOUNT-02": ("login-protocol", "browser"),
    "TASK-01": ("go-race", "proposal-chain"),
    "TASK-02": ("proposal-chain", "browser"),
    "TASK-03": ("go-race", "restart-restore"),
    "PLAN-01": ("proposal-chain",),
    "PLAN-02": ("proposal-chain", "browser"),
    "PLAN-03": ("proposal-chain",),
    "PLAN-04": ("proposal-chain", "restart-restore"),
    "PLAN-05": ("proposal-chain", "graph-chain"),
    "GRAPH-01": ("graph-protocol",),
    "GRAPH-02": ("graph-chain", "browser"),
    "GRAPH-03": ("graph-protocol", "graph-chain"),
    "GRAPH-04": ("graph-chain",),
    "ANALYSIS-01": ("cross-module",),
    "ANALYSIS-02": ("cross-module",),
    "CHARACTER-01": ("cross-module",),
    "CHARACTER-02": ("cross-module",),
    "CHARACTER-03": ("node22", "cross-module"),
    "MODEL-01": ("model-protocol", "browser"),
    "MODEL-02": ("quality-holdout",),
    "UI-01": ("browser",),
    "UI-02": ("browser",),
    "UI-03": ("browser",),
    "OPS-01": ("resources", "restart-restore"),
    "OPS-02": ("evidence-negative",),
    "OPS-03": ("release-blocked",),
    "OPS-04": ("restart-restore", "release-blocked"),
    "AUDIT-01": ("source-baseline", "evidence-negative"),
}
REQUIRED_STEPS = frozenset(("schema", "go-race", "contract", "node22", "proposal-chain", "graph-protocol", "graph-chain", "login-protocol", "model-protocol", "cross-module", "browser", "restart-restore", "evidence-negative"))
RELEASE_PRECONDITIONS = (
    "qq_controlled_actions", "account_login_identity", "artex_dual_graph_live",
    "model_holdout_review", "production_backup_restore", "migration_rehearsal",
    "rollback_rehearsal", "sole_writer_ownership", "runtime_configuration", "monitoring_alerts",
)


def utc():
    return datetime.now(timezone.utc).isoformat()


def file_hash(path):
    return HASHES.sha256_file(Path(path))


def problem(code, detail):
    return {"code": code, "detail": str(detail)}


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x", encoding="utf-8") as handle:
        json.dump(value, handle, ensure_ascii=False, indent=2)
        handle.write("\n")


def read_json(path, problems, label):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        problems.append(problem(label, exc))
        return None


def safe_evidence(package, relative):
    if not isinstance(relative, str) or not relative or Path(relative).is_absolute():
        return None
    path = package / relative
    try:
        path.resolve(strict=True).relative_to(package.resolve())
        if path.is_symlink() or not path.is_file():
            return None
    except (ValueError, OSError):
        return None
    return path


def write_package_manifest(package):
    files, symlinks = HASHES.walk_package(package)
    if symlinks:
        raise ValueError("package symlinks are forbidden")
    with (package / "manifest.sha256").open("x", encoding="utf-8") as handle:
        for relative in files:
            if relative != "manifest.sha256":
                handle.write(f"{file_hash(package / relative)}  ./{relative}\n")


def scenario_problems(scenario, package, proof):
    problems = []
    if not isinstance(proof, dict) or proof.get("state") != "PASS" or not proof.get("assertions") or not all(value is True for value in proof.get("assertions", {}).values()):
        problems.append(problem("scenario-proof-invalid", scenario))
    try:
        actual = FIXTURE.require_checks(scenario, package)
        if isinstance(proof, dict) and proof.get("assertions") != actual:
            problems.append(problem("scenario-assertions-mismatch", scenario))
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as exc:
        problems.append(problem("scenario-business-evidence-invalid", f"{scenario}: {exc}"))
    return problems


def verify_package(project_root, package):
    project_root, package = Path(project_root).resolve(), Path(package).resolve()
    problems = []
    manifest = package / "manifest.sha256"
    if not manifest.is_file():
        problems.append(problem("package-manifest-missing", manifest))
    else:
        entries, parsed = HASHES.parse_manifest(manifest)
        checked = HASHES.Problems()
        checked.extend(parsed)
        HASHES.check_entries(package, entries, checked, "package", forbid=frozenset(("manifest.sha256",)))
        files, symlinks = HASHES.walk_package(package)
        for name in sorted(set(files) - {row[0] for row in entries} - {"manifest.sha256"}):
            checked.add("package-unlisted", name)
        for name in symlinks:
            checked.add("package-symlink", name)
        problems.extend(problem(code, detail) for code, detail in checked.items)
    source = package / "source-files.sha256"
    if not source.is_file():
        problems.append(problem("source-manifest-missing", source))
    else:
        checked = HASHES.Problems()
        HASHES.verify_source(project_root, source, checked, check_scope=True)
        problems.extend(problem(code, detail) for code, detail in checked.items)
    acceptance = read_json(package / "acceptance.json", problems, "acceptance-invalid") or {}
    if acceptance.get("mode") != "fixture" or acceptance.get("state") != "ENGINEERING_ACCEPTED":
        problems.append(problem("engineering-not-accepted", acceptance.get("state")))
    steps = read_json(package / "steps.json", problems, "steps-missing") or []
    if not isinstance(steps, list):
        steps = []
    indexed_steps = {}
    for row in steps:
        if not isinstance(row, dict) or not isinstance(row.get("label"), str):
            problems.append(problem("step-invalid", row))
            continue
        if row["label"] in indexed_steps:
            problems.append(problem("step-duplicate", row["label"]))
        indexed_steps[row["label"]] = row
        if row.get("exit_code") != 0 or not row.get("argv") or not row.get("start_utc") or not row.get("end_utc"):
            problems.append(problem("step-failed-or-unrecorded", row["label"]))
        for stream in ("stdout", "stderr"):
            if safe_evidence(package, row.get(stream)) is None:
                problems.append(problem("step-stream-missing", f"{row['label']} {stream}"))
    for name in sorted(REQUIRED_STEPS - indexed_steps.keys()):
        problems.append(problem("required-step-missing", name))
    rows = acceptance.get("requirements", [])
    if not isinstance(rows, list):
        rows = []
    indexed = {}
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get("id"), str):
            problems.append(problem("requirement-invalid", row))
            continue
        requirement = row["id"]
        if requirement in indexed:
            problems.append(problem("requirement-duplicate", requirement))
        indexed[requirement] = row
        evidence = row.get("evidence", [])
        if row.get("status") != "PASS" or not evidence:
            problems.append(problem("requirement-not-proven", requirement))
        for name in evidence:
            if safe_evidence(package, name) is None:
                problems.append(problem("requirement-evidence-missing", f"{requirement}: {name}"))
        required_scenarios = REQUIREMENTS.get(requirement, ())
        scenarios = row.get("scenarios", [])
        if not set(required_scenarios).issubset(scenarios):
            problems.append(problem("requirement-scenarios-missing", requirement))
        for scenario in required_scenarios:
            proof = read_json(package / "scenarios" / f"{scenario}.json", problems, "scenario-proof-missing")
            problems.extend(scenario_problems(scenario, package, proof))
    if set(indexed) != set(REQUIREMENTS):
        problems.append(problem("requirements-incomplete", sorted(set(REQUIREMENTS) - indexed.keys())))
    resources = read_json(package / "resources.json", problems, "resources-unproven") or {}
    if resources.get("owned_resources_removed") is not True or resources.get("preexisting_preserved") is not True:
        problems.append(problem("resources-not-clean", resources))
    findings, _stats = HASHES.scan_package(package) if package.is_dir() else ([], {})
    for name, line, category in findings:
        problems.append(problem("credential-like-evidence", f"{name}:{line} {category}"))
    return {"ok": not problems, "state": "ENGINEERING_ACCEPTED" if not problems else "ENGINEERING_REJECTED", "problems": problems}


def release_problems(package):
    package = Path(package)
    problems = []
    evidence = read_json(package / "release/evidence.json", problems, "release-evidence-missing") or {}
    records = evidence.get("prerequisites", {})
    for name in RELEASE_PRECONDITIONS:
        row = records.get(name, {}) if isinstance(records, dict) else {}
        required = ("accepted_at", "reviewer", "scope", "evidence", "sha256")
        proof = safe_evidence(package, row.get("evidence"))
        if row.get("status") != "ACCEPTED" or any(not row.get(field) for field in required) or proof is None:
            problems.append(problem("release-prerequisite-missing", name))
            continue
        if file_hash(proof) != row["sha256"]:
            problems.append(problem("release-prerequisite-hash", name))
        scope = row["scope"]
        if not isinstance(scope, dict) or not scope.get("environment") or scope.get("environment") == "fixture":
            problems.append(problem("release-fixture-not-live", name))
        if name in ("qq_controlled_actions", "account_login_identity", "artex_dual_graph_live"):
            authorization = row.get("authorization", {})
            if not all(authorization.get(key) for key in ("reference", "accounts", "actions", "window", "stop_conditions")):
                problems.append(problem("release-authorization-missing", name))
    return problems


class Recorder:
    def __init__(self, package):
        self.package = Path(package)
        self.steps = []

    def run(self, label, argv, env=None, cwd=ROOT, stdin=None):
        output = self.package / "logs" / f"{label}.stdout.log"
        error = self.package / "logs" / f"{label}.stderr.log"
        output.parent.mkdir(parents=True, exist_ok=True)
        row = {"label": label, "argv": [str(arg) for arg in argv], "cwd": str(cwd), "start_utc": utc(),
               "stdout": str(output.relative_to(self.package)), "stderr": str(error.relative_to(self.package))}
        with output.open("xb") as stdout, error.open("xb") as stderr:
            completed = subprocess.run(argv, cwd=cwd, env={**os.environ, **(env or {})}, input=stdin, stdout=stdout, stderr=stderr)
        row.update(end_utc=utc(), exit_code=completed.returncode)
        self.steps.append(row)
        write_json(self.package / "logs" / f"{label}.command.json", row)
        print(f"{label}: exit={completed.returncode}", flush=True)
        return completed.returncode


def run_fixture(output):
    return FIXTURE.run_fixture(sys.modules[__name__], output)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    fixture_parser = sub.add_parser("fixture")
    fixture_parser.add_argument("--output", required=True)
    for command in ("verify", "release-check"):
        part = sub.add_parser(command)
        part.add_argument("--package", required=True)
    args = parser.parse_args()
    if args.command == "fixture":
        return run_fixture(Path(args.output).resolve())
    result = verify_package(ROOT, Path(args.package))
    if args.command == "release-check":
        result["problems"].extend(release_problems(Path(args.package)))
        result["ok"] = not result["problems"]
        result["state"] = "RELEASE_ACCEPTED" if result["ok"] else "RELEASE_BLOCKED"
        result["result"] = "ACCEPTED" if result["ok"] else "BLOCKED"
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result["ok"] else (3 if args.command == "release-check" else 1)


if __name__ == "__main__":
    raise SystemExit(main())
