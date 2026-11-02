#!/usr/bin/env python3
"""Local-only OpenAPI / JSON Schema reference gate.

This gate runs *before* redocly, oapi-codegen, openapi-typescript, or any
other third-party contract tool.  It deliberately implements a small,
closed reference grammar:

* only relative references into files below the contracts root are allowed;
  remote URLs, schemes, absolute paths, Windows drive paths, query strings,
  backslashes and percent-encoding are rejected;
* ``..`` path traversal is rejected before the filesystem is touched;
* symlinks are resolved and must stay below the real contracts root;
* fragments must be RFC 6901 JSON Pointers and must resolve to a value.

No network access is ever attempted.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any, Iterable, Iterator, Sequence

try:  # PyYAML is available in the contract validation environment.
    import yaml
except ImportError:  # pragma: no cover - exercised only on broken installs
    yaml = None


_SCHEME_RE = re.compile(r"^[A-Za-z][A-Za-z0-9+.\-]*:")
_DRIVE_RE = re.compile(r"^[A-Za-z]:[\\/]")
_ALLOWED_SUFFIXES = {".json", ".yaml", ".yml"}
_CONTROL_RE = re.compile(r"[\x00-\x1f\x7f]")


class RefGateError(ValueError):
    """A reference failed the local-only gate."""


@dataclass(frozen=True)
class GateIssue:
    file: Path
    pointer: str
    message: str

    def render(self, root: Path) -> str:
        try:
            shown = self.file.relative_to(root)
        except ValueError:
            shown = self.file
        return f"{shown}:{self.pointer}: {self.message}"


def load_document(path: Path) -> Any:
    """Load JSON or YAML without enabling unsafe YAML constructors."""

    text = path.read_text(encoding="utf-8")
    suffix = path.suffix.lower()
    if suffix == ".json":
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            raise RefGateError(f"{path}: invalid JSON: {exc}") from exc
    if suffix in {".yaml", ".yml"}:
        # contracts/openapi.yaml is JSON syntax that is also valid YAML 1.2.
        # Parsing JSON first lets the pinned node image run the ref gate even
        # when PyYAML is absent, without weakening non-JSON YAML support.
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            pass
        if yaml is None:  # pragma: no cover
            raise RefGateError(
                f"{path}: PyYAML is required for non-JSON YAML contract files"
            )
        try:
            return yaml.safe_load(text)
        except yaml.YAMLError as exc:
            raise RefGateError(f"{path}: invalid YAML: {exc}") from exc
    raise RefGateError(f"{path}: unsupported contract file extension {suffix!r}")


def _escape_pointer_token(token: str) -> str:
    return token.replace("~", "~0").replace("/", "~1")


def _parse_json_pointer(fragment: str) -> list[str]:
    if fragment == "":
        return []
    if not fragment.startswith("/"):
        raise RefGateError(
            "fragment must be an absolute JSON Pointer beginning with '/' "
            f"(anchors are not supported): #{fragment}"
        )
    if _CONTROL_RE.search(fragment):
        raise RefGateError("fragment contains control characters")
    if "%" in fragment:
        raise RefGateError("percent-encoded references are forbidden")
    tokens: list[str] = []
    for raw in fragment.split("/")[1:]:
        out: list[str] = []
        index = 0
        while index < len(raw):
            char = raw[index]
            if char != "~":
                out.append(char)
                index += 1
                continue
            if index + 1 >= len(raw):
                raise RefGateError(f"invalid JSON Pointer escape in '~{raw}'")
            escaped = raw[index + 1]
            if escaped == "0":
                out.append("~")
            elif escaped == "1":
                out.append("/")
            else:
                raise RefGateError(f"invalid JSON Pointer escape '~{escaped}'")
            index += 2
        tokens.append("".join(out))
    return tokens


def _resolve_pointer(document: Any, tokens: Sequence[str]) -> Any:
    current = document
    walked: list[str] = []
    for token in tokens:
        walked.append(token)
        pointer = "/" + "/".join(_escape_pointer_token(part) for part in walked)
        if isinstance(current, dict):
            if token not in current:
                raise RefGateError(f"unresolved JSON Pointer at {pointer}")
            current = current[token]
        elif isinstance(current, list):
            if token == "-" or not token.isdigit() or (len(token) > 1 and token[0] == "0"):
                raise RefGateError(f"invalid array index {token!r} at {pointer}")
            index = int(token)
            if index >= len(current):
                raise RefGateError(f"array index out of range at {pointer}")
            current = current[index]
        else:
            raise RefGateError(f"pointer traverses a scalar at {pointer}")
    return current


class RefGate:
    """Resolve and validate references while pinning them to one directory."""

    def __init__(self, contracts_root: Path | str):
        self.root = Path(contracts_root).resolve(strict=True)
        if not self.root.is_dir():
            raise RefGateError(f"contracts root is not a directory: {self.root}")
        self._documents: dict[Path, Any] = {}
        self._checked_documents: set[Path] = set()

    # -- document loading -------------------------------------------------
    def load(self, path: Path) -> Any:
        real = path.resolve(strict=True)
        if real not in self._documents:
            if not real.is_file():
                raise RefGateError(f"reference target is not a file: {path}")
            self._documents[real] = load_document(real)
        return self._documents[real]

    # -- path handling ----------------------------------------------------
    def _assert_inside(self, candidate: Path, *, allow_missing: bool = False) -> Path:
        try:
            real = candidate.resolve(strict=not allow_missing)
        except FileNotFoundError as exc:
            raise RefGateError(f"reference target does not exist: {candidate}") from exc
        try:
            real.relative_to(self.root)
        except ValueError as exc:
            raise RefGateError(
                f"reference escapes contracts root via symlink or path: {candidate}"
            ) from exc
        return real

    def _resolve_file(self, reference_path: str, base_file: Path) -> Path:
        if not reference_path:
            raise RefGateError("empty reference path")
        if _SCHEME_RE.match(reference_path):
            raise RefGateError(f"remote/scheme references are forbidden: {reference_path!r}")
        if reference_path.startswith("//"):
            raise RefGateError(f"protocol-relative references are forbidden: {reference_path!r}")
        if reference_path.startswith(("/", "\\")):
            raise RefGateError(f"absolute references are forbidden: {reference_path!r}")
        if _DRIVE_RE.match(reference_path):
            raise RefGateError(f"drive-letter references are forbidden: {reference_path!r}")
        if "\\" in reference_path:
            raise RefGateError(f"backslash references are forbidden: {reference_path!r}")
        if "%" in reference_path:
            raise RefGateError(f"percent-encoded references are forbidden: {reference_path!r}")
        if "?" in reference_path:
            raise RefGateError(f"query strings are forbidden in references: {reference_path!r}")
        if _CONTROL_RE.search(reference_path):
            raise RefGateError("reference path contains control characters")

        pure = PurePosixPath(reference_path)
        if pure.is_absolute():
            raise RefGateError(f"absolute references are forbidden: {reference_path!r}")
        if ".." in pure.parts:
            raise RefGateError(f"parent traversal is forbidden: {reference_path!r}")
        if not pure.parts:
            raise RefGateError(f"invalid reference path: {reference_path!r}")
        if pure.suffix.lower() not in _ALLOWED_SUFFIXES:
            raise RefGateError(
                f"reference target must be .json/.yaml/.yml: {reference_path!r}"
            )

        candidate = (base_file.parent / Path(*pure.parts)).resolve(strict=False)
        real = self._assert_inside(candidate)
        if not real.is_file():
            raise RefGateError(f"reference target is not a file: {candidate}")
        return real

    # -- public reference API --------------------------------------------
    def resolve(self, reference: str, base_file: Path | str) -> tuple[Path, Any]:
        """Resolve one reference; returns (real target file, pointed value)."""

        base = Path(base_file).resolve(strict=True)
        self._assert_inside(base)
        if not isinstance(reference, str) or not reference:
            raise RefGateError("reference must be a non-empty string")
        if _CONTROL_RE.search(reference):
            raise RefGateError("reference contains control characters")
        if reference.count("#") > 1:
            raise RefGateError(f"reference contains more than one '#': {reference!r}")

        if "#" in reference:
            path_part, fragment = reference.split("#", 1)
        else:
            path_part, fragment = reference, ""

        if path_part:
            target = self._resolve_file(path_part, base)
        else:
            target = base

        tokens = _parse_json_pointer(fragment)
        document = self.load(target)
        value = _resolve_pointer(document, tokens)
        return target, value

    def check_document(self, path: Path | str) -> tuple[int, list[GateIssue]]:
        """Walk every reachable local document once and validate all refs."""

        file_path = Path(path).resolve(strict=True)
        try:
            file_path.relative_to(self.root)
        except ValueError as exc:
            raise RefGateError(f"document is outside contracts root: {file_path}") from exc
        issues: list[GateIssue] = []
        count = 0

        def walk_document(current_file: Path, stack: set[Path]) -> None:
            nonlocal count
            current_file = current_file.resolve(strict=True)
            if current_file in stack or current_file in self._checked_documents:
                return
            stack.add(current_file)
            document = self.load(current_file)

            def walk(value: Any, pointer: str) -> None:
                nonlocal count
                if isinstance(value, dict):
                    for key, child in value.items():
                        token = _escape_pointer_token(str(key))
                        child_pointer = f"{pointer}/{token}"
                        if key in {"$ref", "$dynamicRef", "$recursiveRef"}:
                            if key != "$ref":
                                issues.append(
                                    GateIssue(
                                        current_file,
                                        child_pointer,
                                        f"{key} is not supported; use local $ref only",
                                    )
                                )
                                continue
                            count += 1
                            if not isinstance(child, str):
                                issues.append(GateIssue(current_file, child_pointer, "$ref must be a string"))
                                continue
                            try:
                                target, _ = self.resolve(child, current_file)
                                walk_document(target, stack)
                            except (RefGateError, FileNotFoundError, OSError) as exc:
                                issues.append(GateIssue(current_file, child_pointer, str(exc)))
                        else:
                            walk(child, child_pointer)
                elif isinstance(value, list):
                    for index, child in enumerate(value):
                        walk(child, f"{pointer}/{index}")

            walk(document, "#")
            stack.remove(current_file)
            self._checked_documents.add(current_file)

        walk_document(file_path, set())
        return count, issues


def default_spec_files(contracts_root: Path) -> list[Path]:
    files: list[Path] = []
    openapi = contracts_root / "openapi.yaml"
    if openapi.is_file():
        files.append(openapi)
    schemas = contracts_root / "schemas"
    if schemas.is_dir():
        files.extend(sorted(schemas.rglob("*.json")))
    return files


def gate_files(contracts_root: Path | str, files: Iterable[Path]) -> list[GateIssue]:
    gate = RefGate(contracts_root)
    issues: list[GateIssue] = []
    for file_path in files:
        _, file_issues = gate.check_document(file_path)
        issues.extend(file_issues)
    return issues


def _expand_inputs(root: Path, inputs: Sequence[str]) -> list[Path]:
    if not inputs:
        return default_spec_files(root)
    files: list[Path] = []
    for raw in inputs:
        candidate = Path(raw)
        if not candidate.is_absolute():
            candidate = (Path.cwd() / candidate).resolve()
        if candidate.is_dir():
            files.extend(
                sorted(
                    path
                    for path in candidate.rglob("*")
                    if path.is_file() and path.suffix.lower() in _ALLOWED_SUFFIXES
                )
            )
        else:
            files.append(candidate)
    return files


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("paths", nargs="*", help="files/dirs to scan (default: contracts)")
    parser.add_argument(
        "--root",
        default="contracts",
        help="contracts root that references may not escape (default: contracts)",
    )
    args = parser.parse_args(argv)

    try:
        root = Path(args.root).resolve(strict=True)
    except FileNotFoundError:
        print(f"refgate: contracts root does not exist: {args.root}", file=sys.stderr)
        return 2

    try:
        files = _expand_inputs(root, args.paths)
    except (RefGateError, OSError) as exc:
        print(f"refgate: {exc}", file=sys.stderr)
        return 2

    if not files:
        print(f"refgate: no contract files found below {root}", file=sys.stderr)
        return 2

    try:
        gate = RefGate(root)
        total_refs = 0
        issues: list[GateIssue] = []
        for file_path in files:
            count, file_issues = gate.check_document(file_path)
            total_refs += count
            issues.extend(file_issues)
    except (RefGateError, OSError) as exc:
        print(f"refgate: FATAL: {exc}", file=sys.stderr)
        return 2

    for issue in issues:
        print(f"refgate: {issue.render(root)}", file=sys.stderr)
    if issues:
        print(
            f"refgate: FAILED: {len(issues)} issue(s) across {len(files)} file(s)",
            file=sys.stderr,
        )
        return 1

    print(f"refgate: OK: {total_refs} local ref(s) in {len(files)} file(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
