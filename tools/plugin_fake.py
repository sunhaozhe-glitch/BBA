#!/usr/bin/env python3
"""Isolated HTTP fake for the canonical NapCat plugin routes.

The fake binds an ephemeral 127.0.0.1 port (never 6099), validates the request
body against the contracts' PluginJoinRequest / PluginSearchRequest schema,
accepts only the canonical napcat-plugin-cleaner paths, and rejects the legacy
dxc-join-api route. It is used by tests only; there is no long-running server.
"""

from __future__ import annotations

import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Mapping, Sequence

_REPO_ROOT = Path(__file__).resolve().parents[1]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

from tools.validate_contracts import ContractLoader, RefGateError  # noqa: E402

CANONICAL_SEARCH = "/api/Plugin/ext/napcat-plugin-cleaner/search-group"
CANONICAL_JOIN = "/api/Plugin/ext/napcat-plugin-cleaner/join-group"
LEGACY_JOIN = "/api/Plugin/ext/dxc-join-api/join-group"
LEGACY_SEARCH = "/api/Plugin/ext/dxc-join-api/search-group"
MAX_BODY_BYTES = 1_048_576


class _PluginHandler(BaseHTTPRequestHandler):
    loader: ContractLoader
    components: dict[str, str]
    response_components: dict[str, str]
    request_counts: dict[str, int]
    response_delays: Mapping[str, float]

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
        return

    def _send(self, code: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            return

    def do_POST(self) -> None:  # noqa: N802
        if self.path in {LEGACY_SEARCH, LEGACY_JOIN}:
            self._send(404, {"error": "NOT_FOUND", "message": "legacy route is not registered"})
            return
        if self.path not in {CANONICAL_SEARCH, CANONICAL_JOIN}:
            self._send(404, {"error": "NOT_FOUND"})
            return
        with self.request_counts_lock:
            self.request_counts[self.path] = self.request_counts.get(self.path, 0) + 1

        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self._send(400, {"error": "INVALID_REQUEST", "message": "bad Content-Length"})
            return
        if length <= 0 or length > MAX_BODY_BYTES:
            self._send(400, {"error": "INVALID_REQUEST", "message": "body size"})
            return
        try:
            payload = json.loads(self.rfile.read(length))
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._send(400, {"error": "INVALID_REQUEST", "message": "invalid JSON"})
            return

        component = self.components[self.path]
        try:
            errors = self.loader.component_errors(component, payload)
        except RefGateError as exc:
            self._send(500, {"error": "CONTRACT_UNAVAILABLE", "message": str(exc)})
            return
        if errors:
            self._send(
                400,
                {
                    "error": "INVALID_REQUEST",
                    "message": "request does not match " + component,
                    "details": errors[:8],
                },
            )
            return
        if self.path in self.responses:
            response = self.responses[self.path]
            response_component = self.response_components[self.path]
            try:
                response_errors = self.loader.component_errors(response_component, response)
            except RefGateError as exc:
                self._send(500, {"error": "CONTRACT_UNAVAILABLE", "message": str(exc)})
                return
            if response_errors:
                self._send(
                    500,
                    {
                        "error": "FAKE_RESPONSE_CONTRACT_INVALID",
                        "details": response_errors[:8],
                    },
                )
                return
            delay = self.response_delays.get(self.path, 0.0)
            if delay > 0:
                time.sleep(delay)
            self._send(200, response)
            return
        self._send(200, {"accepted": True, "component": component})


class PluginFake:
    def __init__(
        self,
        contracts_root: Path | str,
        *,
        responses: Mapping[str, dict[str, Any]] | None = None,
        response_delays: Mapping[str, float] | None = None,
    ):
        self.loader = ContractLoader(contracts_root)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _PluginHandler)
        self.server.daemon_threads = True
        self.request_counts: dict[str, int] = {}
        self.request_counts_lock = threading.Lock()
        _PluginHandler.loader = self.loader
        _PluginHandler.components = {
            CANONICAL_SEARCH: "PluginSearchRequest",
            CANONICAL_JOIN: "PluginJoinRequest",
        }
        _PluginHandler.response_components = {
            CANONICAL_SEARCH: "PluginSearchResponse",
            CANONICAL_JOIN: "PluginJoinResponse",
        }
        _PluginHandler.request_counts = self.request_counts
        _PluginHandler.request_counts_lock = self.request_counts_lock
        _PluginHandler.responses = responses or {}
        _PluginHandler.response_delays = response_delays or {}
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        host, port = self.server.server_address[:2]
        return f"http://{host}:{port}"

    def __enter__(self) -> "PluginFake":
        self.thread.start()
        return self

    def __exit__(self, *exc_info: object) -> None:
        if self.thread.is_alive():
            self.server.shutdown()
            self.thread.join(timeout=5)
        self.server.server_close()


def request(
    url: str,
    method: str = "POST",
    payload: Any = None,
    timeout: float = 5,
) -> tuple[int, dict[str, Any]]:
    """Minimal stdlib HTTP client used by tests."""

    from urllib.error import HTTPError
    from urllib.request import Request, urlopen

    body = json.dumps(payload).encode("utf-8") if payload is not None else b""
    request_obj = Request(url, data=body, method=method)
    request_obj.add_header("Content-Type", "application/json")
    try:
        with urlopen(request_obj, timeout=timeout) as response:
            return response.status, json.loads(response.read())
    except HTTPError as exc:
        try:
            raw = exc.read()
        finally:
            exc.close()
        return exc.code, json.loads(raw) if raw else {}


def main(argv: Sequence[str] | None = None) -> int:  # pragma: no cover - diagnostic only
    import argparse

    parser = argparse.ArgumentParser(description="validate plugin schema fixtures without HTTP")
    parser.add_argument("contracts", nargs="?", default="contracts")
    args = parser.parse_args(argv)
    with PluginFake(args.contracts) as fake:
        print(f"plugin fake would bind {fake.base_url}; fixture test helper only")
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
