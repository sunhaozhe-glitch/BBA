#!/usr/bin/env python3
"""Evidence-gated rollback planner. Application is restricted to temporary copies.
No deletion operation is implemented: added/unknown/unchanged files are retained.
Not a transactional rollback engine; stop concurrent writers before copy testing.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import stat
import sys
import tempfile


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe_file(base, relative):
    if not isinstance(relative, str) or not relative or "\\" in relative:
        raise ValueError("invalid relative path")
    parts = relative.split("/")
    if any(x in ("", ".", "..") for x in parts) or PurePosixPath(relative).is_absolute():
        raise ValueError("unsafe path: " + relative)
    p = base
    for part in parts:
        p = p / part
        if p.is_symlink():
            raise ValueError("symlink refused: " + str(p))
    if not p.is_file() or p.stat().st_nlink != 1:
        raise ValueError("missing/nonregular/hardlinked file: " + str(p))
    return p


def load_manifest(path):
    m = json.loads(path.read_text())
    if not isinstance(m, dict) or m.get("schema_version") != 1:
        raise ValueError("legacy/unrecognized manifest refused")
    if not isinstance(m.get("entries"), list) or not isinstance(m.get("sources"), dict):
        raise ValueError("missing entries or sources")
    sources = {}
    for name in ("original_before", "original_delivery", "historical_audit", "review_classification"):
        s = m["sources"][name]
        p = Path(s["file"])
        if p.is_symlink() or digest(p.read_bytes()) != s["sha256"]:
            raise ValueError("evidence hash mismatch: " + name)
        sources[name] = p
    b = json.loads(sources["original_before"].read_text())
    d = json.loads(sources["original_delivery"].read_text())
    before = {x["path"]: x["sha256"] for x in b}
    delivery = {x["path"]: x["sha256"] for x in d}
    if len(before) != len(b) or len(delivery) != len(d):
        raise ValueError("duplicate source path")
    historical = {}
    for line in sources["historical_audit"].read_text().splitlines():
        h, p = line.split(None, 1)
        historical[p.removeprefix("./")] = h
    paths = [e["path"] for e in m["entries"]]
    if len(paths) != len(set(paths)) or set(paths) != set(delivery):
        raise ValueError("manifest must cover exact original delivery path set")
    for e in m["entries"]:
        p = e["path"]
        if e["delivered_sha256"] != delivery[p]:
            raise ValueError("delivery evidence mismatch: " + p)
        expected = "unchanged" if (before.get(p) or historical.get(p)) == delivery[p] else ("modified" if p in before else "unknown")
        if e["classification"] != expected:
            raise ValueError("unsupported classification (no absence proof): " + p)
        if expected == "modified" and e.get("before_sha256") != before[p]:
            raise ValueError("before evidence mismatch: " + p)
    return m, sources


def main():
    a = argparse.ArgumentParser(description=__doc__)
    a.add_argument("--root", required=True, type=Path)
    a.add_argument("--manifest", required=True, type=Path)
    mode = a.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="default; no target writes")
    mode.add_argument("--apply-copy", action="store_true", help="only manifest-adjacent temporary-copies descendants")
    args = a.parse_args()
    report = {"mode": "apply-copy" if args.apply_copy else "dry-run", "root": str(args.root), "operations": [], "errors": [], "deletion_supported": False}
    try:
        m, sources = load_manifest(args.manifest)
        base = args.root.resolve(strict=True)
        if args.root.is_symlink() or not base.is_dir():
            raise ValueError("root must be a real directory")
        if args.apply_copy:
            allowed = args.manifest.resolve().parent / "temporary-copies"
            if not base.is_relative_to(allowed.resolve()) or base == allowed.resolve():
                raise ValueError("apply refused outside temporary-copies descendant; real rollback disabled")
            live = Path(__file__).resolve().parent.parent
            if base == live or live.is_relative_to(base):
                raise ValueError("live project root forbidden")
        restores = []
        for e in m["entries"]:
            op = {"path": e["path"], "classification": e["classification"], "action": "keep"}
            report["operations"].append(op)
            if e["classification"] != "modified":
                op["reason"] = "unchanged/unknown: never remove or overwrite"
                continue
            try:
                target = safe_file(base, e["path"])
                current = digest(target.read_bytes())
                op["current_sha256"] = current
                if current != e["delivered_sha256"]:
                    raise ValueError("current hash differs from delivered; preserve subsequent edits")
                expected_backup = sources["original_before"].parent / "backup"
                backup = safe_file(expected_backup, e["path"])
                declared = e.get("backup") or {}
                if Path(declared.get("file", "")).absolute() != backup.absolute():
                    raise ValueError("backup must be original before backup")
                data = backup.read_bytes()
                if digest(data) != e["before_sha256"] or declared.get("sha256") != e["before_sha256"]:
                    raise ValueError("backup hash mismatch")
                op["action"] = "would-restore"
                restores.append((e, target, data, op))
            except (ValueError, OSError) as error:
                op["action"] = "refuse"
                op["reason"] = str(error)
                report["errors"].append(e["path"] + ": " + str(error))
        if args.apply_copy and not report["errors"]:
            # Preflight ALL paths before the first mutation. Recheck each target
            # immediately before atomic replacement. No real-tree application.
            for e, target, data, op in restores:
                target = safe_file(base, e["path"])
                if digest(target.read_bytes()) != e["delivered_sha256"]:
                    raise ValueError("target changed after preflight: " + e["path"])
            for e, target, data, op in restores:
                target = safe_file(base, e["path"])
                fd, temporary = tempfile.mkstemp(prefix=".safe-rollback-", dir=target.parent)
                try:
                    with os.fdopen(fd, "wb") as stream:
                        stream.write(data)
                        stream.flush()
                        os.fsync(stream.fileno())
                    os.chmod(temporary, stat.S_IMODE(target.stat().st_mode))
                    safe_file(base, e["path"])
                    if digest(target.read_bytes()) != e["delivered_sha256"]:
                        raise ValueError("target changed before replace: " + e["path"])
                    os.replace(temporary, target)
                    if digest(target.read_bytes()) != e["before_sha256"]:
                        raise ValueError("restore readback mismatch: " + e["path"])
                    op["action"] = "restored"
                finally:
                    if os.path.exists(temporary):
                        os.unlink(temporary)
    except (ValueError, OSError, KeyError, TypeError, AttributeError) as error:
        report["errors"].append(str(error))
    report["ok"] = not report["errors"]
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report["ok"] else 2


if __name__ == "__main__":
    sys.exit(main())
