"""Real plugin route -> PluginJoinResponse contract integration test.

Unlike ``test_plugin_fake.py`` (an offline Python fake), this test imports the
actual ``plugins/napcat-plugin-cleaner/index.mjs`` package, runs its real
``plugin_init`` route registration, invokes the registered ``/join-group``
handler through a Node subprocess, and validates every response with the
project's existing ``ContractLoader.schema_errors`` implementation.

Six native return shapes are covered:

* ``void`` (undefined)          -> must fail closed, never become result=1
* numeric ``1``                 -> valid bounded raw integer
* 66-property object, result 1  -> raw must stay within the bounded contract
* 257-character ``request_id``  -> overlong request_ref is omitted entirely
* ``1.9``                       -> invalid result fails closed
* ``-1``                        -> invalid result fails closed
"""

from __future__ import annotations

import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
import unittest
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from tools.validate_contracts import ContractLoader

ROOT = Path(__file__).resolve().parents[2]
CONTRACTS = ROOT / "contracts"
PLUGIN = ROOT / "plugins" / "napcat-plugin-cleaner" / "index.mjs"
HARNESS = Path(__file__).with_name("fixtures") / "plugin_join_route_harness.mjs"
SCHEMA_FILE = CONTRACTS / "schemas" / "plugin.json"
FIXTURES_FILE = CONTRACTS / "examples" / "plugin.json"
MAX_SAFE_INTEGER = 9007199254740991
RAW_BUDGET_BYTES = 4096
FALLBACK_NODE = Path("/home/sun/.hermes/tools/node-26.7.0-linux-x64/bin/node")


def resolve_node() -> Path | None:
    """Env override first, then PATH, then the pinned local fallback."""

    for variable in ("PLUGIN_NODE_BIN", "NODE_BIN"):
        override = os.environ.get(variable)
        if override:
            candidate = Path(override)
            return candidate if candidate.is_file() else None
    found = shutil.which("node")
    if found:
        return Path(found)
    if FALLBACK_NODE.is_file():
        return FALLBACK_NODE
    return None


NODE = resolve_node()


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


@unittest.skipUnless(
    NODE is not None and PLUGIN.is_file() and HARNESS.is_file(),
    "real plugin route test requires node, plugin index.mjs, and harness",
)
class PluginRouteContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.loader = ContractLoader(CONTRACTS)
        cls.schema, cls.schema_source = cls.loader.resolve_component(
            "PluginJoinResponse"
        )
        node_version = subprocess.run(
            [str(NODE), "--version"],
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        ).stdout.strip()
        cls.metadata = {
            "utc": datetime.now(timezone.utc).isoformat(),
            "object": str(PLUGIN),
            "object_sha256": sha256(PLUGIN),
            "test": str(Path(__file__).resolve()),
            "test_sha256": sha256(Path(__file__).resolve()),
            "harness_sha256": sha256(HARNESS),
            "schema": str(SCHEMA_FILE),
            "schema_sha256": sha256(SCHEMA_FILE),
            "fixtures": str(FIXTURES_FILE),
            "fixtures_sha256": sha256(FIXTURES_FILE),
            "node": str(NODE),
            "node_version": node_version,
            "python": sys.version.split()[0],
            "platform": platform.platform(),
            "cwd": os.getcwd(),
            "env": {
                variable: os.environ[variable]
                for variable in ("PLUGIN_NODE_BIN", "NODE_BIN", "NODE_OPTIONS")
                if variable in os.environ
            },
        }
        print(
            "ROUTE_CONTRACT_META "
            + json.dumps(cls.metadata, sort_keys=True, ensure_ascii=False)
        )
        cls.results = {item["name"]: item for item in cls._run_harness()}

    @classmethod
    def _run_harness(cls) -> list[dict[str, Any]]:
        environment = os.environ.copy()
        environment["PLUGIN_UNDER_TEST"] = str(PLUGIN)
        completed = subprocess.run(
            [str(NODE), str(HARNESS)],
            cwd=str(ROOT),
            env=environment,
            check=False,
            capture_output=True,
            text=True,
            timeout=120,
        )
        if completed.returncode != 0:
            raise AssertionError(
                f"route harness exited {completed.returncode}\n"
                f"stdout:\n{completed.stdout}\nstderr:\n{completed.stderr}"
            )
        try:
            payload = json.loads(completed.stdout)
        except json.JSONDecodeError as exc:
            raise AssertionError(
                f"route harness did not emit JSON: {exc}\n"
                f"stdout:\n{completed.stdout}\nstderr:\n{completed.stderr}"
            ) from exc
        if not isinstance(payload, list) or len(payload) != 6:
            raise AssertionError(f"expected six route results, got {payload!r}")
        return payload

    def body_for(self, name: str) -> dict[str, Any]:
        item = self.results[name]
        self.assertEqual(
            item["native_calls"],
            1,
            f"{name}: route must dispatch the native call exactly once",
        )
        body = item["response"]
        self.assertIsInstance(body, dict, f"{name}: route must return a JSON object")
        return body

    def assert_contract_valid(self, name: str, body: dict[str, Any]) -> None:
        errors = self.loader.schema_errors(body, self.schema, self.schema_source)
        self.assertEqual(
            errors,
            [],
            f"{name}: response does not satisfy PluginJoinResponse: {body!r}",
        )

    def assert_bounded_raw(self, name: str, body: dict[str, Any]) -> None:
        raw = body.get("raw")
        if isinstance(raw, str):
            serialized = json.dumps(raw, ensure_ascii=False)
            self.assertLessEqual(
                len(serialized.encode("utf-8")),
                RAW_BUDGET_BYTES,
                f"{name}: raw string exceeds the 4096-byte UTF-8 budget",
            )
        elif isinstance(raw, dict):
            self.assertLessEqual(
                len(raw), 64, f"{name}: raw object exceeds 64 properties"
            )
        elif isinstance(raw, bool) or raw is None:
            pass
        else:
            self.assertIsInstance(raw, int, f"{name}: raw must stay schema-bounded")
            self.assertGreaterEqual(raw, 0, f"{name}: raw integer must be non-negative")
            self.assertLessEqual(
                raw, MAX_SAFE_INTEGER, f"{name}: raw integer must be safe"
            )

    def test_void_result_fails_closed_and_is_contract_valid(self) -> None:
        body = self.body_for("void_undefined")
        self.assertIs(body["ok"], False, "void native return must not be success")
        self.assertEqual(body.get("code"), "UNKNOWN_RESULT")
        self.assertNotIn("result", body, "void must never default to result=1")
        self.assert_contract_valid("void_undefined", body)
        self.assert_bounded_raw("void_undefined", body)

    def test_numeric_one_is_contract_valid(self) -> None:
        body = self.body_for("integer_one")
        self.assertIs(body["ok"], True)
        self.assertEqual(body["result"], 1)
        self.assert_contract_valid("integer_one", body)
        self.assert_bounded_raw("integer_one", body)

    def test_66_property_object_keeps_raw_bounded(self) -> None:
        body = self.body_for("object_66_props_with_result_one")
        self.assertIs(body["ok"], True)
        self.assertEqual(body["result"], 1)
        raw = body.get("raw")
        if isinstance(raw, dict):
            self.assertLessEqual(
                len(raw), 64, "66-property native object must degrade, not pass through"
            )
        self.assert_contract_valid("object_66_props_with_result_one", body)
        self.assert_bounded_raw("object_66_props_with_result_one", body)

    def test_257_char_request_id_is_omitted_entirely(self) -> None:
        body = self.body_for("request_id_257_chars")
        self.assertIs(body["ok"], True)
        self.assertEqual(body["result"], 1)
        self.assertNotIn(
            "request_ref",
            body,
            "request_ref longer than 256 characters must be omitted, not truncated",
        )
        self.assert_contract_valid("request_id_257_chars", body)
        self.assert_bounded_raw("request_id_257_chars", body)

    def test_non_integer_result_fails_closed(self) -> None:
        body = self.body_for("result_1_9")
        self.assertIs(body["ok"], False)
        self.assertNotIn("result", body, "1.9 must never be reported as a result code")
        self.assert_contract_valid("result_1_9", body)
        self.assert_bounded_raw("result_1_9", body)

    def test_negative_result_fails_closed(self) -> None:
        body = self.body_for("result_negative_one")
        self.assertIs(body["ok"], False)
        self.assertNotIn("result", body, "-1 must never be reported as a result code")
        self.assert_contract_valid("result_negative_one", body)
        self.assert_bounded_raw("result_negative_one", body)


if __name__ == "__main__":
    unittest.main()
