#!/usr/bin/env python3
"""Independent, stdlib-only verifier for round-4 evidence packages.

This file is deliberately separate from ``tools/round4-evidence.sh``: the shell
runner owns the commands and the raw logs, this verifier re-implements the
integrity rules and refuses to trust the runner's own conclusions.

It never writes to a package except for the explicit ``finalize`` command.  The
``scan`` command prints only path/line/category for a finding; it never prints a
credential value, cookie value or request header body.

Commands
--------
  fingerprint   Compute a root-relative source manifest with the round-4 scope.
  verify-source Verify a source manifest against a project root.
  verify        Verify a finalized package (manifest, source, logs, scan).
  finalize      After REPORT.md exists, scan and write a package-relative manifest.
  scan          Scan a package for credential-like material without echoing it.
  inventory     Read-only sha256 inventory (used for old archives).
  negative      Copy a finalized package to a temporary directory, tamper with
                each copy, require verify to fail, and prove the original is
                byte-for-byte unchanged.

No network, no third-party modules.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

# ---------------------------------------------------------------------------
# Round-4 source scope.  Keep this in sync with tools/round4-evidence.sh; the
# source-verify step intentionally recomputes the scope so a drifted rule is
# visible instead of silently accepted.
# ---------------------------------------------------------------------------

SOURCE_DIRS = (
    "backend",
    "koishi",
    "tools",
    "docs",
    "tests",
    "contracts",
    "evals",
    "plugins",
    "patches",
)
SOURCE_ROOT_FILES = (
    "package.json",
    "package-lock.json",
    "redocly.yaml",
    "tsconfig.contract.json",
    "tsconfig.contract-smoke.json",
    ".gitignore",
    "go.mod",
    "go.sum",
)
SOURCE_SUFFIXES = (
    ".go",
    ".ts",
    ".tsx",
    ".js",
    ".jsx",
    ".css",
    ".html",
    ".py",
    ".sh",
    ".bash",
    ".sql",
    ".mjs",
    ".cjs",
    ".json",
    ".jsonl",
    ".md",
    ".yml",
    ".yaml",
    ".patch",
)
SOURCE_NAMES = frozenset(("go.mod", "go.sum"))
PRUNE_NAMES = frozenset(
    (
        "node_modules",
        ".cache",
        "build",
        "lib",
        "dist",
        "coverage",
        "__pycache__",
        ".tmp",
        ".work",
        ".git",
    )
)

MANIFEST_LINE = re.compile(r"^([0-9a-fA-F]{64}) [ *](.+)$")
UTC_FORMAT = "%Y-%m-%dT%H:%M:%SZ"
UTC_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
IMAGE_KEYS = {
    "kernel_go_image": "golang:1.26",
    "contract_go_image": "golang:1.26",
    "contract_node_image": "node:22-bookworm",
    "kernel_postgres_image": "postgres:16-bookworm",
    "browser_postgres_image": "postgres:16-alpine",
    "browser_go_image": "golang:1.26",
    "node22_image": "node:22",
}
REQUIRED_STEP_LABELS = frozenset(
    (
        "schema",
        "go-test-all",
        "chain-proposal",
        "chain-worker",
        "chain-summary",
        "contract-check",
        "node22-verify",
        "source-fingerprint",
        "source-verify",
    )
)


def now_utc() -> str:
    return datetime.now(timezone.utc).strftime(UTC_FORMAT)


def die(message: str, code: int = 1) -> "None":
    print(f"round4-verify: {message}", file=sys.stderr)
    raise SystemExit(code)


class Problems:
    def __init__(self) -> None:
        self.items: list[tuple[str, str]] = []

    def add(self, kind: str, detail: str = "") -> None:
        self.items.append((kind, detail))

    def extend(self, other: "Problems") -> None:
        self.items.extend(other.items)

    def __bool__(self) -> bool:
        return bool(self.items)

    def __len__(self) -> int:
        return len(self.items)


# ---------------------------------------------------------------------------
# Hashing and path safety
# ---------------------------------------------------------------------------


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def normalize_rel(name: str) -> str | None:
    """Return a safe root-relative POSIX path, or None for an escaping path."""
    if not name or "\x00" in name or "\n" in name or "\r" in name:
        return None
    if name.startswith("/"):
        return None
    parts: list[str] = []
    for part in name.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            return None
        parts.append(part)
    if not parts:
        return None
    return "/".join(parts)


def parse_manifest(path: Path) -> tuple[list[tuple[str, str, int]], Problems]:
    problems = Problems()
    entries: list[tuple[str, str, int]] = []
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        problems.add("manifest-unreadable", f"{path}: {exc}")
        return entries, problems
    seen: dict[str, int] = {}
    for lineno, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            problems.add("manifest-blank-line", f"{path}:{lineno}")
            continue
        match = MANIFEST_LINE.match(line)
        if not match:
            problems.add("manifest-malformed", f"{path}:{lineno}")
            continue
        digest, raw_name = match.group(1).lower(), match.group(2)
        norm = normalize_rel(raw_name)
        if norm is None:
            problems.add("manifest-path-escape", f"{path}:{lineno} path={raw_name!r}")
            continue
        if norm in seen:
            problems.add(
                "manifest-duplicate",
                f"{path}:{lineno} path={norm} first={seen[norm]}",
            )
            continue
        seen[norm] = lineno
        entries.append((norm, digest, lineno))
    return entries, problems


def check_entries(
    root: Path,
    entries: list[tuple[str, str, int]],
    problems: Problems,
    label: str,
    forbid: frozenset[str] = frozenset(),
) -> None:
    try:
        root = root.resolve()
    except OSError as exc:
        problems.add(f"{label}-root-unreadable", f"{root}: {exc}")
        return
    for rel, want, lineno in entries:
        if rel in forbid:
            problems.add(f"{label}-forbidden-entry", f"line={lineno} path={rel}")
            continue
        target = root / rel
        try:
            st = target.lstat()
        except FileNotFoundError:
            problems.add(f"{label}-missing", f"{rel}")
            continue
        except OSError as exc:
            problems.add(f"{label}-stat-error", f"{rel}: {exc}")
            continue
        if stat.S_ISLNK(st.st_mode):
            problems.add(f"{label}-symlink", f"{rel}")
            continue
        if not stat.S_ISREG(st.st_mode):
            problems.add(f"{label}-not-regular", f"{rel}")
            continue
        try:
            resolved = target.resolve(strict=True)
            resolved.relative_to(root)
        except (OSError, ValueError):
            problems.add(f"{label}-path-escape", f"{rel}")
            continue
        try:
            got = sha256_file(target)
        except OSError as exc:
            problems.add(f"{label}-read-error", f"{rel}: {exc}")
            continue
        if got != want:
            problems.add(f"{label}-hash-mismatch", f"{rel} expected={want} got={got}")


# ---------------------------------------------------------------------------
# Scope walks
# ---------------------------------------------------------------------------


def walk_package(root: Path) -> tuple[list[str], list[str]]:
    """Return (regular files, symlinks) relative to root, never following links."""
    root = root.resolve()
    files: list[str] = []
    symlinks: list[str] = []
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        cur = Path(dirpath)
        keep: list[str] = []
        for name in dirnames:
            candidate = cur / name
            relative = str(candidate.relative_to(root)).replace(os.sep, "/")
            if candidate.is_symlink():
                symlinks.append(relative)
            else:
                keep.append(name)
        dirnames[:] = sorted(keep)
        for name in filenames:
            candidate = cur / name
            relative = str(candidate.relative_to(root)).replace(os.sep, "/")
            if candidate.is_symlink():
                symlinks.append(relative)
            elif candidate.is_file():
                files.append(relative)
    return sorted(files), sorted(symlinks)


def is_source_name(name: str) -> bool:
    return name in SOURCE_NAMES or any(name.endswith(suffix) for suffix in SOURCE_SUFFIXES)


def source_scope(root: Path) -> set[str]:
    """Recompute the round-4 source file set from an explicit project root."""
    root = root.resolve()
    found: set[str] = set()
    for top in SOURCE_DIRS:
        base = root / top
        if not base.is_dir() or base.is_symlink():
            continue
        for dirpath, dirnames, filenames in os.walk(base, followlinks=False):
            cur = Path(dirpath)
            relative_parts = cur.relative_to(root).parts
            keep: list[str] = []
            for name in dirnames:
                if name in PRUNE_NAMES:
                    continue
                if name == "results" and relative_parts and relative_parts[-1] == "evals":
                    continue
                if (cur / name).is_symlink():
                    continue
                keep.append(name)
            dirnames[:] = sorted(keep)
            for name in filenames:
                candidate = cur / name
                if candidate.is_symlink() or not candidate.is_file():
                    continue
                if is_source_name(name):
                    found.add(str(candidate.relative_to(root)).replace(os.sep, "/"))
    for name in SOURCE_ROOT_FILES:
        candidate = root / name
        if candidate.is_file() and not candidate.is_symlink():
            found.add(name)
    return found


# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------


def verify_source(
    project_root: Path,
    manifest: Path,
    problems: Problems,
    check_scope: bool,
) -> dict[str, int]:
    entries, manifest_problems = parse_manifest(manifest)
    for kind, detail in manifest_problems.items:
        problems.add(f"source-{kind}", detail)
    check_entries(project_root, entries, problems, "source")
    listed = {rel for rel, _digest, _lineno in entries}
    stats = {"entries": len(entries), "scope": 0, "unlisted": 0, "out_of_scope": 0}
    if check_scope:
        try:
            scope = source_scope(project_root)
        except OSError as exc:
            problems.add("source-scope-error", f"{project_root}: {exc}")
            return stats
        stats["scope"] = len(scope)
        for rel in sorted(scope - listed):
            problems.add("source-file-not-in-manifest", rel)
        for rel in sorted(listed - scope):
            problems.add("source-manifest-entry-not-in-scope", rel)
        stats["unlisted"] = len(scope - listed)
        stats["out_of_scope"] = len(listed - scope)
    return stats


def parse_utc(value: str, where: str, problems: Problems) -> datetime | None:
    if not UTC_RE.match(value):
        problems.add("run-time-format", f"{where}={value!r}")
        return None
    try:
        return datetime.strptime(value, UTC_FORMAT).replace(tzinfo=timezone.utc)
    except ValueError:
        problems.add("run-time-invalid", f"{where}={value!r}")
        return None


def verify_steps(package_root: Path, problems: Problems) -> dict[str, int]:
    path = package_root / "steps.tsv"
    if not path.is_file():
        problems.add("steps-missing", str(path))
        return {"steps": 0}
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        problems.add("steps-unreadable", f"{path}: {exc}")
        return {"steps": 0}
    count = 0
    labels: set[str] = set()
    for lineno, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            problems.add("steps-blank-line", f"line={lineno}")
            continue
        fields = line.split("\t")
        if len(fields) != 5:
            problems.add("steps-field-count", f"line={lineno} fields={len(fields)}")
            continue
        label = fields[0]
        values: dict[str, str] = {}
        malformed = False
        for field in fields[1:]:
            key, separator, value = field.partition("=")
            if not separator or key in values:
                malformed = True
                break
            values[key] = value
        if malformed or set(values) != {"start", "end", "exit", "command"}:
            problems.add("steps-fields", f"line={lineno} label={label}")
            continue
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", label):
            problems.add("steps-label", f"line={lineno} label={label!r}")
            continue
        if label in labels:
            problems.add("steps-duplicate-label", label)
            continue
        labels.add(label)
        count += 1
        start = parse_utc(values["start"], f"{label}:start", problems)
        end = parse_utc(values["end"], f"{label}:end", problems)
        if start and end and end < start:
            problems.add("steps-end-before-start", label)
        if not re.fullmatch(r"\d+", values["exit"]):
            problems.add("steps-exit-format", f"{label}: {values['exit']!r}")
        command = values["command"]
        if not command:
            problems.add("steps-command-empty", label)
        else:
            try:
                tokens = shlex.split(command)
            except ValueError:
                tokens = []
                problems.add("steps-command-unparseable", label)
            if not tokens:
                problems.add("steps-command-unparseable", label)
            else:
                if tokens[0] in {"bash", "sh", "dash", "zsh"} and "-c" in tokens:
                    problems.add("steps-masked-shell", label)
                if any(token in {"|", "||", "&&", ";", ">", ">>", "<", "2>", "&"} for token in tokens):
                    problems.add("steps-masked-pipeline", label)
        log_path = package_root / "logs" / f"{label}.log"
        if not log_path.is_file():
            problems.add("step-log-missing", str(log_path))
            continue
        try:
            log_lines = log_path.read_text(encoding="utf-8", errors="replace").splitlines()
        except OSError as exc:
            problems.add("step-log-unreadable", f"{log_path}: {exc}")
            continue
        while log_lines and log_lines[-1] == "":
            log_lines.pop()
        if len(log_lines) < 4:
            problems.add("step-log-short", label)
            continue
        if log_lines[0] != values["start"]:
            problems.add("step-log-start-mismatch", label)
        if log_lines[1] != "$ " + command:
            problems.add("step-log-command-mismatch", label)
        if log_lines[-2] != f"exit={values['exit']}":
            problems.add("step-log-exit-mismatch", label)
        if log_lines[-1] != f"end={values['end']}":
            problems.add("step-log-end-mismatch", label)
    for label in sorted(REQUIRED_STEP_LABELS - labels):
        problems.add("steps-required-missing", label)
    return {"steps": count}


def verify_environment(package_root: Path, problems: Problems) -> None:
    path = package_root / "environment.txt"
    if not path.is_file():
        problems.add("environment-missing", str(path))
        return
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        problems.add("environment-unreadable", f"{path}: {exc}")
        return
    start_match = re.search(r"^utc_start=(\S+)$", text, re.MULTILINE)
    if not start_match:
        problems.add("environment-utc-start-missing", "")
    else:
        parse_utc(start_match.group(1), "environment:utc_start", problems)
    if not re.search(r"^host=\S", text, re.MULTILINE):
        problems.add("environment-host-missing", "")
    if not re.search(r"^docker_version_exit=0$", text, re.MULTILINE):
        problems.add("environment-docker-version", "")
    if not re.search(r"^python3_exit=0$", text, re.MULTILINE):
        problems.add("environment-python3", "")
    required_images: set[str] = set()
    for key, default in IMAGE_KEYS.items():
        match = re.search(rf"^{re.escape(key)}=(\S+)$", text, re.MULTILINE)
        required_images.add(match.group(1) if match else default)
    for tag in sorted(required_images):
        match = re.search(
            rf"^image={re.escape(tag)} id=(\S+) inspect_exit=(\d+)$",
            text,
            re.MULTILINE,
        )
        if not match:
            problems.add("environment-image-missing", tag)
            continue
        image_id, exit_code = match.group(1), match.group(2)
        if exit_code != "0":
            problems.add("environment-image-inspect-failed", f"{tag} exit={exit_code}")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            problems.add("environment-image-id-invalid", f"{tag} id={image_id!r}")


def verify_scan(package_root: Path, problems: Problems) -> None:
    path = package_root / "secret-scan.txt"
    if not path.is_file():
        problems.add("secret-scan-missing", str(path))
        return
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        problems.add("secret-scan-unreadable", f"{path}: {exc}")
        return
    match = re.search(r"^findings=(\d+)$", text, re.MULTILINE)
    if not match:
        problems.add("secret-scan-summary-missing", "")
        return
    findings = int(match.group(1))
    if findings != 0:
        problems.add("secret-scan-findings", f"count={findings}")
    if findings == 0 and re.search(r"^finding:", text, re.MULTILINE):
        problems.add("secret-scan-inconsistent", "findings=0 but finding lines present")


def verify_package(
    project_root: Path,
    package_root: Path,
    check_scope: bool = True,
    require_browser: bool = False,
) -> tuple[Problems, dict[str, object]]:
    problems = Problems()
    stats: dict[str, object] = {}
    if not package_root.is_dir():
        problems.add("package-root-missing", str(package_root))
        return problems, stats
    manifest = package_root / "manifest.sha256"
    if not manifest.is_file():
        problems.add("manifest-missing", str(manifest))
        return problems, stats
    entries, manifest_problems = parse_manifest(manifest)
    problems.extend(manifest_problems)
    check_entries(
        package_root,
        entries,
        problems,
        "package",
        forbid=frozenset(("manifest.sha256",)),
    )
    files, symlinks = walk_package(package_root)
    for rel in symlinks:
        problems.add("package-symlink", rel)
    listed = {rel for rel, _digest, _lineno in entries}
    for rel in sorted(set(files) - listed - {"manifest.sha256"}):
        problems.add("package-file-not-in-manifest", rel)
    stats["package_entries"] = len(entries)
    stats["package_files"] = len(files)
    if not (package_root / "REPORT.md").is_file():
        problems.add("report-missing", str(package_root / "REPORT.md"))
    else:
        try:
            if (package_root / "REPORT.md").stat().st_size == 0:
                problems.add("report-empty", "")
        except OSError as exc:
            problems.add("report-unreadable", f"{exc}")
    source_manifest = package_root / "source-files.sha256"
    if not source_manifest.is_file():
        problems.add("source-manifest-missing", str(source_manifest))
    else:
        stats["source"] = verify_source(
            project_root, source_manifest, problems, check_scope=check_scope
        )
    stats.update(verify_steps(package_root, problems))
    verify_environment(package_root, problems)
    verify_scan(package_root, problems)
    browser_files = [rel for rel in files if rel.startswith("browser/")]
    stats["browser_files"] = len(browser_files)
    if require_browser and not browser_files:
        problems.add("browser-evidence-missing", "")
    return problems, stats


def print_problems(problems: Problems, limit: int = 200) -> None:
    for kind, detail in problems.items[:limit]:
        if detail:
            print(f"problem: {kind} {detail}")
        else:
            print(f"problem: {kind}")
    if len(problems) > limit:
        print(f"problem: ... {len(problems) - limit} more")


# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------


def cmd_fingerprint(args: argparse.Namespace) -> int:
    project_root = Path(args.project_root).resolve()
    output = Path(args.output)
    if not project_root.is_dir():
        die(f"project root is not a directory: {project_root}")
    if output.exists():
        die(f"refusing to overwrite {output}")
    files = sorted(source_scope(project_root))
    lines: list[str] = []
    for rel in files:
        path = project_root / rel
        try:
            digest = sha256_file(path)
        except OSError as exc:
            die(f"cannot hash {rel}: {exc}")
        lines.append(f"{digest}  {rel}\n")
    output.parent.mkdir(parents=True, exist_ok=True)
    try:
        with output.open("x", encoding="utf-8", newline="\n") as handle:
            handle.writelines(lines)
    except FileExistsError:
        die(f"refusing to overwrite {output}")
    except OSError:
        try:
            output.unlink()
        except OSError:
            pass
        raise
    print(f"fingerprint: files={len(files)} root={project_root} output={output}")
    return 0


def cmd_verify_source(args: argparse.Namespace) -> int:
    problems = Problems()
    stats = verify_source(
        Path(args.project_root),
        Path(args.manifest),
        problems,
        check_scope=not args.hash_only,
    )
    print(
        "source-verify: entries={entries} scope={scope} unlisted={unlisted} "
        "out_of_scope={out_of_scope}".format(**stats)
    )
    print_problems(problems)
    print(f"RESULT: {'FAIL' if problems else 'PASS'}")
    return 1 if problems else 0


def cmd_verify(args: argparse.Namespace) -> int:
    problems, stats = verify_package(
        Path(args.project_root),
        Path(args.package_root),
        check_scope=not args.hash_only_source,
        require_browser=args.require_browser,
    )
    print(f"verify: project_root={Path(args.project_root).resolve()}")
    print(f"verify: package_root={Path(args.package_root).resolve()}")
    if "source" in stats:
        source = stats["source"]
        print(
            "verify: source_entries={entries} source_scope={scope}".format(**source)
        )
    print(
        "verify: package_entries={package_entries} package_files={package_files} "
        "steps={steps} browser_files={browser_files}".format(**{
            "package_entries": stats.get("package_entries", 0),
            "package_files": stats.get("package_files", 0),
            "steps": stats.get("steps", 0),
            "browser_files": stats.get("browser_files", 0),
        })
    )
    print(f"verify: problems={len(problems)}")
    print_problems(problems)
    print(f"RESULT: {'FAIL' if problems else 'PASS'}")
    return 1 if problems else 0


def unique_path(parent: Path, stem: str) -> Path:
    candidate = parent / stem
    if not candidate.exists():
        return candidate
    counter = 1
    while True:
        candidate = parent / f"{stem}-{counter}"
        if not candidate.exists():
            return candidate
        counter += 1


def write_scan_report(
    path: Path,
    findings: list[tuple[str, int, str]],
    stats: dict[str, int],
) -> None:
    lines = [
        f"utc={now_utc()}",
        f"files={stats['files']}",
        f"scanned={stats['scanned']}",
        f"skipped_binary={stats['skipped_binary']}",
        f"findings={len(findings)}",
    ]
    for rel, lineno, category in findings:
        lines.append(f"finding: {rel}:{lineno} category={category}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def cmd_scan(args: argparse.Namespace) -> int:
    package_root = Path(args.package_root).resolve()
    if not package_root.is_dir():
        die(f"package root is not a directory: {package_root}")
    findings, stats = scan_package(package_root)
    if args.output:
        target = Path(args.output)
        if target.exists():
            die(f"refusing to overwrite {target}")
        write_scan_report(target, findings, stats)
    print(
        "secret-scan: files={files} scanned={scanned} skipped_binary={skipped_binary} "
        "findings={findings}".format(findings=len(findings), **stats)
    )
    for rel, lineno, category in findings:
        print(f"finding: {rel}:{lineno} category={category}")
    print(f"RESULT: {'FAIL' if findings else 'PASS'}")
    return 1 if findings else 0


def cmd_finalize(args: argparse.Namespace) -> int:
    project_root = Path(args.project_root).resolve()
    package_root = Path(args.package_root).resolve()
    if not package_root.is_dir():
        die(f"package root is not a directory: {package_root}")
    report = package_root / "REPORT.md"
    if not report.is_file() or report.stat().st_size == 0:
        die("finalize requires a non-empty REPORT.md in the package")
    source_manifest = package_root / "source-files.sha256"
    if not source_manifest.is_file():
        die(f"finalize requires {source_manifest}")
    problems = Problems()
    source_stats = verify_source(
        project_root, source_manifest, problems, check_scope=True
    )
    if problems:
        print("finalize: source manifest does not match the project tree")
        print_problems(problems)
        print(f"RESULT: {'FAIL' if problems else 'PASS'}")
        return 1
    manifest = package_root / "manifest.sha256"
    if manifest.exists() and not args.rebuild:
        die(
            "manifest.sha256 already exists; use --rebuild to preserve it "
            "and write a new manifest"
        )
    files, symlinks = walk_package(package_root)
    for rel in symlinks:
        problems.add("package-symlink", rel)
    if problems:
        print_problems(problems)
        print("RESULT: FAIL")
        return 1
    findings, scan_stats = scan_package(package_root)
    scan_target = package_root / "secret-scan.txt"
    if scan_target.exists():
        previous = unique_path(
            package_root,
            f"secret-scan.previous-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}",
        )
        os.rename(scan_target, previous)
    write_scan_report(scan_target, findings, scan_stats)
    print(
        "finalize: secret_scan files={files} scanned={scanned} "
        "skipped_binary={skipped_binary} findings={findings}".format(
            findings=len(findings), **scan_stats
        )
    )
    if findings:
        for rel, lineno, category in findings:
            print(f"finding: {rel}:{lineno} category={category}")
        print("finalize: refusing to write manifest while findings exist")
        print("RESULT: FAIL")
        return 1
    manifest = package_root / "manifest.sha256"
    if manifest.exists():
        previous = unique_path(
            package_root,
            f"manifest.sha256.previous-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}",
        )
        os.rename(manifest, previous)
    files, symlinks = walk_package(package_root)
    if symlinks:
        for rel in symlinks:
            print(f"problem: package-symlink {rel}")
        print("RESULT: FAIL")
        return 1
    entries = [rel for rel in files if rel != "manifest.sha256"]
    lines: list[str] = []
    for rel in sorted(entries):
        try:
            digest = sha256_file(package_root / rel)
        except OSError as exc:
            die(f"cannot hash {rel}: {exc}")
        lines.append(f"{digest}  ./{rel}\n")
    try:
        with manifest.open("x", encoding="utf-8", newline="\n") as handle:
            handle.writelines(lines)
    except FileExistsError:
        die(f"manifest appeared while finalizing: {manifest}")
    print(f"finalize: manifest entries={len(entries)} source_files={source_stats['entries']}")
    verify_problems, verify_stats = verify_package(
        project_root, package_root, check_scope=True, require_browser=False
    )
    if verify_problems:
        print_problems(verify_problems)
        print("RESULT: FAIL")
        return 1
    print(
        "finalize: verified package_entries={package_entries} package_files={package_files} "
        "steps={steps} browser_files={browser_files}".format(**{
            "package_entries": verify_stats.get("package_entries", 0),
            "package_files": verify_stats.get("package_files", 0),
            "steps": verify_stats.get("steps", 0),
            "browser_files": verify_stats.get("browser_files", 0),
        })
    )
    print("RESULT: PASS")
    return 0


def inventory_map(package_root: Path) -> dict[str, str]:
    files, _symlinks = walk_package(package_root)
    result: dict[str, str] = {}
    for rel in files:
        result[rel] = sha256_file(package_root / rel)
    return result


def cmd_inventory(args: argparse.Namespace) -> int:
    package_root = Path(args.package_root).resolve()
    if not package_root.is_dir():
        die(f"package root is not a directory: {package_root}")
    result = inventory_map(package_root)
    if args.json:
        print(json.dumps(result, indent=2, sort_keys=True))
    else:
        for rel in sorted(result):
            print(f"{result[rel]}  {rel}")
    return 0


# ---------------------------------------------------------------------------
# Secret scan
# ---------------------------------------------------------------------------

SECRET_PATTERNS: tuple[tuple[str, re.Pattern[str], int | None], ...] = (
    ("private-key", re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----"), None),
    (
        "authorization-header",
        re.compile(
            r"(?i)\bauthorization\b[\"']?\s*[:=]\s*[\"']?(?:bearer|basic)\s+"
            r"([A-Za-z0-9._~+/=-]{8,})"
        ),
        1,
    ),
    (
        "cookie-header",
        re.compile(r"(?i)\b(?:set-)?cookie\b[\"']?\s*[:=]\s*[\"']?([^\"'\s;]{6,})"),
        1,
    ),
    (
        "dsn-password",
        re.compile(
            r"(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis)://"
            r"[^\s:/@]+:([^\s@/]+)@"
        ),
        1,
    ),
    (
        "credential-assignment",
        re.compile(
            r"(?i)\b(password|passwd|secret|client[_-]?secret|api[_-]?key|apikey|"
            r"access[_-]?token|refresh[_-]?token|auth[_-]?token|token|"
            r"app[_-]?secret)\b[\"']?\s*[:=]\s*[\"']?"
            r"([^\"'\s,;]{6,})"
        ),
        2,
    ),
    (
        "bearer-token",
        re.compile(r"(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{16,})"),
        1,
    ),
    (
        "api-key-prefix",
        re.compile(r"\b(?:sk-[A-Za-z0-9]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16})\b"),
        None,
    ),
    ("bot-token", re.compile(r"\b\d{6,12}:[A-Za-z0-9_-]{30,}\b"), None),
)

PLACEHOLDER_RE = re.compile(
    r"^(?:<redacted>|\[redacted\]|\(redacted\)|redacted|\*{3,}|x{4,}|…|\.\.\.)$",
    re.IGNORECASE,
)


def is_placeholder(value: str) -> bool:
    cleaned = value.strip().strip("\"'")
    if cleaned.startswith("<") and cleaned.endswith(">"):
        return True
    return bool(PLACEHOLDER_RE.match(cleaned)) or "redacted" in cleaned.lower()


def scan_line(line: str) -> list[str]:
    categories: list[str] = []
    for category, pattern, group in SECRET_PATTERNS:
        match = pattern.search(line)
        if not match:
            continue
        if group is not None and is_placeholder(match.group(group)):
            continue
        if category == "credential-assignment" and re.fullmatch(
            r"(?:\./)?tools/[A-Za-z0-9_./-]+\.(?:sh|py)", match.group(2)
        ) and re.fullmatch(r"\s+(?:token|runtime-token|env)\)?[.;]?\s*", line[match.end():]):
            # A documented executable + credential-printing subcommand is a
            # command reference, not the credential value it would print.
            continue
        categories.append(category)
    return categories


def scan_package(package_root: Path) -> tuple[list[tuple[str, int, str]], dict[str, int]]:
    package_root = package_root.resolve()
    files, _symlinks = walk_package(package_root)
    findings: list[tuple[str, int, str]] = []
    scanned = 0
    skipped = 0
    for rel in files:
        path = package_root / rel
        try:
            with path.open("rb") as handle:
                head = handle.read(8192)
        except OSError:
            skipped += 1
            continue
        if b"\x00" in head:
            skipped += 1
            continue
        try:
            with path.open("r", encoding="utf-8", errors="replace") as handle:
                for lineno, line in enumerate(handle, 1):
                    for category in scan_line(line):
                        findings.append((rel, lineno, category))
        except OSError:
            skipped += 1
            continue
        scanned += 1
    return findings, {"files": len(files), "scanned": scanned, "skipped_binary": skipped}


# ---------------------------------------------------------------------------
# Negative test
# ---------------------------------------------------------------------------


def verify_cli(project_root: Path, package_root: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [
            sys.executable,
            str(Path(__file__).resolve()),
            "verify",
            "--project-root",
            str(project_root),
            "--package-root",
            str(package_root),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        check=False,
    )


def first_manifest_file(package_root: Path) -> str | None:
    manifest = package_root / "manifest.sha256"
    try:
        for line in manifest.read_text(encoding="utf-8").splitlines():
            match = MANIFEST_LINE.match(line)
            if match:
                norm = normalize_rel(match.group(2))
                if norm and norm != "manifest.sha256":
                    return norm
    except OSError:
        return None
    return None


def refresh_manifest_entry(package_copy: Path, rel: str) -> None:
    """Update only the temp copy's outer manifest hash for ``rel``.

    Source-negative cases must fail because of the source manifest, not because
    the package manifest still hashes the old source-files.sha256.
    """
    manifest = package_copy / "manifest.sha256"
    digest = sha256_file(package_copy / rel)
    lines = manifest.read_text(encoding="utf-8").splitlines()
    replaced = False
    updated: list[str] = []
    for line in lines:
        match = MANIFEST_LINE.match(line)
        if match and normalize_rel(match.group(2)) == rel:
            updated.append(f"{digest}  ./{rel}")
            replaced = True
        else:
            updated.append(line)
    if not replaced:
        updated.append(f"{digest}  ./{rel}")
    manifest.write_text("\n".join(updated) + "\n", encoding="utf-8")


def tamper_case(name: str, package_copy: Path) -> None:
    if name == "content-tamper":
        target = first_manifest_file(package_copy) or "REPORT.md"
        path = package_copy / target
        with path.open("ab") as handle:
            handle.write(b"\nnegative-tamper\n")
    elif name == "extra-file":
        (package_copy / "unexpected-extra.txt").write_text("extra\n", encoding="utf-8")
    elif name == "duplicate-entry":
        manifest = package_copy / "manifest.sha256"
        lines = manifest.read_text(encoding="utf-8").splitlines()
        for line in lines:
            if MANIFEST_LINE.match(line):
                manifest.write_text("\n".join(lines + [line]) + "\n", encoding="utf-8")
                break
    elif name == "missing-file":
        target = first_manifest_file(package_copy)
        if not target:
            raise RuntimeError("no file to delete")
        (package_copy / target).unlink()
    elif name == "path-escape":
        manifest = package_copy / "manifest.sha256"
        lines = manifest.read_text(encoding="utf-8").splitlines()
        for index, line in enumerate(lines):
            match = MANIFEST_LINE.match(line)
            if match:
                lines[index] = f"{'0' * 64}  ../escaped-path"
                break
        manifest.write_text("\n".join(lines) + "\n", encoding="utf-8")
    elif name == "source-hash-tamper":
        source = package_copy / "source-files.sha256"
        lines = source.read_text(encoding="utf-8").splitlines()
        for index, line in enumerate(lines):
            match = MANIFEST_LINE.match(line)
            if match:
                digest = match.group(1)
                flipped = ("0" if digest[0] != "0" else "1") + digest[1:]
                lines[index] = f"{flipped}  {match.group(2)}"
                break
        source.write_text("\n".join(lines) + "\n", encoding="utf-8")
        refresh_manifest_entry(package_copy, "source-files.sha256")
    elif name == "source-missing-entry":
        source = package_copy / "source-files.sha256"
        lines = source.read_text(encoding="utf-8").splitlines()
        for index, line in enumerate(lines):
            if MANIFEST_LINE.match(line):
                del lines[index]
                break
        source.write_text("\n".join(lines) + "\n", encoding="utf-8")
        refresh_manifest_entry(package_copy, "source-files.sha256")
    elif name == "source-path-escape":
        source = package_copy / "source-files.sha256"
        lines = source.read_text(encoding="utf-8").splitlines()
        for index, line in enumerate(lines):
            match = MANIFEST_LINE.match(line)
            if match:
                lines[index] = f"{match.group(1)}  ../source-escape"
                break
        source.write_text("\n".join(lines) + "\n", encoding="utf-8")
        refresh_manifest_entry(package_copy, "source-files.sha256")
    else:
        raise RuntimeError(f"unknown tamper case {name}")


def cmd_negative(args: argparse.Namespace) -> int:
    project_root = Path(args.project_root).resolve()
    package_root = Path(args.package_root).resolve()
    if not package_root.is_dir():
        die(f"package root is not a directory: {package_root}")
    baseline = verify_cli(project_root, package_root)
    if baseline.returncode != 0:
        print(baseline.stdout, end="")
        die("negative: original package does not verify; finalize it first")
    before = inventory_map(package_root)
    cases = (
        "content-tamper",
        "extra-file",
        "duplicate-entry",
        "missing-file",
        "path-escape",
        "source-hash-tamper",
        "source-missing-entry",
        "source-path-escape",
    )
    results: list[dict[str, object]] = []
    tempdir = Path(tempfile.mkdtemp(prefix="bba-round4-negative-"))
    try:
        for case in cases:
            copy = tempdir / case
            shutil.copytree(package_root, copy, symlinks=True)
            try:
                tamper_case(case, copy)
            except Exception as exc:  # noqa: BLE001 - report the case as failed
                results.append({"case": case, "expected": "fail", "exit": None, "error": str(exc)})
                continue
            outcome = verify_cli(project_root, copy)
            row: dict[str, object] = {
                "case": case,
                "expected": "fail",
                "exit": outcome.returncode,
                "detected": outcome.returncode != 0,
            }
            if case.startswith("source-"):
                row["source_problem"] = "problem: source-" in outcome.stdout
            results.append(row)
    finally:
        shutil.rmtree(tempdir, ignore_errors=True)
    after = inventory_map(package_root)
    final = verify_cli(project_root, package_root)
    failures = [
        row
        for row in results
        if not row.get("detected", False)
        or (str(row.get("case", "")).startswith("source-") and not row.get("source_problem", False))
    ]
    unchanged = before == after
    summary = {
        "package": str(package_root),
        "cases": results,
        "original_verify_exit": final.returncode,
        "original_unchanged": unchanged,
        "result": "PASS" if (not failures and unchanged and final.returncode == 0) else "FAIL",
    }
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0 if summary["result"] == "PASS" else 1


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("fingerprint", help="write a source manifest")
    p.add_argument("--project-root", required=True)
    p.add_argument("--output", required=True)
    p.set_defaults(func=cmd_fingerprint)

    p = sub.add_parser("verify-source", help="verify a source manifest")
    p.add_argument("--project-root", required=True)
    p.add_argument("--manifest", required=True)
    p.add_argument("--check-scope", action="store_true", default=True)
    p.add_argument("--hash-only", action="store_true")
    p.set_defaults(func=cmd_verify_source)

    p = sub.add_parser("verify", help="verify a finalized package")
    p.add_argument("--project-root", required=True)
    p.add_argument("--package-root", required=True)
    p.add_argument("--hash-only-source", action="store_true")
    p.add_argument("--require-browser", action="store_true")
    p.set_defaults(func=cmd_verify)

    p = sub.add_parser("finalize", help="scan then write a package manifest")
    p.add_argument("--project-root", required=True)
    p.add_argument("--package-root", required=True)
    p.add_argument("--rebuild", action="store_true")
    p.set_defaults(func=cmd_finalize)

    p = sub.add_parser("scan", help="scan for credential-like material")
    p.add_argument("--package-root", required=True)
    p.add_argument("--output")
    p.set_defaults(func=cmd_scan)

    p = sub.add_parser("inventory", help="read-only sha256 inventory")
    p.add_argument("--package-root", required=True)
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_inventory)

    p = sub.add_parser("negative", help="tampered-copy negative test")
    p.add_argument("--project-root", required=True)
    p.add_argument("--package-root", required=True)
    p.set_defaults(func=cmd_negative)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 - one-line final error for the CLI
        die(f"{type(exc).__name__}: {exc}")


if __name__ == "__main__":
    sys.exit(main())
