import json
import os
import tempfile
import unittest
from pathlib import Path

from tools.refgate import RefGate, RefGateError, gate_files


def write_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value), encoding="utf-8")


class RefGateTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name) / "contracts"
        self.schemas = self.root / "schemas"
        self.schemas.mkdir(parents=True)
        write_json(
            self.schemas / "common.json",
            {
                "Id": {"type": "string", "minLength": 1},
                "List": {"type": "array", "items": {"$ref": "#/Id"}},
            },
        )
        write_json(
            self.root / "openapi.yaml",
            {
                "openapi": "3.0.3",
                "components": {
                    "schemas": {"Id": {"$ref": "./schemas/common.json#/Id"}}
                },
            },
        )

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_accepts_relative_and_local_refs(self) -> None:
        gate = RefGate(self.root)
        target, value = gate.resolve("./schemas/common.json#/Id", self.root / "openapi.yaml")
        self.assertEqual(target, (self.schemas / "common.json").resolve())
        self.assertEqual(value["type"], "string")
        target, value = gate.resolve("#/Id", self.schemas / "common.json")
        self.assertEqual(target, (self.schemas / "common.json").resolve())
        self.assertEqual(value["type"], "string")

    def test_rejects_remote_scheme_absolute_and_traversal(self) -> None:
        bad_refs = [
            "https://example.com/schema.json#/Id",
            "//example.com/schema.json#/Id",
            "ftp://example.com/schema.json",
            "file:///etc/passwd",
            "common.json?x=1#/Id",
            "../outside.json#/Id",
            "%2e%2e/outside.json#/Id",
            "common%2Ejson#/Id",
            "common.json#/%2e%2e",
            "/etc/passwd",
            "C:\\schema.json",
            "common.json\\..\\x",
            "sub/common.json#/Id",
            "notes.md#/Id",
            "common.json#Id",
            "common.json#/Id~2",
            "common.json#/Missing",
            "common.json#/Id/0",
            "common.json#/List/01",
            "common.json#/List/-",
        ]
        gate = RefGate(self.root)
        for ref in bad_refs:
            with self.subTest(ref=ref):
                with self.assertRaises(RefGateError):
                    gate.resolve(ref, self.root / "openapi.yaml")

    def test_json_pointer_array_index_resolves(self) -> None:
        write_json(
            self.schemas / "common.json",
            {
                "Id": {"type": "string"},
                "List": {"type": "array", "items": {"$ref": "#/Id"}},
                "Tuple": [{"type": "string"}],
            },
        )
        gate = RefGate(self.root)
        _, value = gate.resolve("#/Tuple/0", self.schemas / "common.json")
        self.assertEqual(value, {"type": "string"})

    def test_symlink_escape_is_rejected_but_inside_alias_is_allowed(self) -> None:
        outside = Path(self._tmp.name) / "outside"
        write_json(outside / "secret.json", {"Secret": {"type": "string"}})
        escape = self.schemas / "escape.json"
        os.symlink(outside / "secret.json", escape)
        gate = RefGate(self.root)
        with self.assertRaises(RefGateError):
            gate.resolve("schemas/escape.json#/Secret", self.root / "openapi.yaml")

        inside = self.schemas / "alias.json"
        os.symlink(self.schemas / "common.json", inside)
        target, value = gate.resolve("schemas/alias.json#/Id", self.root / "openapi.yaml")
        self.assertEqual(target.parent, self.schemas.resolve())
        self.assertEqual(value["type"], "string")

    def test_check_document_reports_all_unresolved_refs(self) -> None:
        write_json(
            self.root / "openapi.yaml",
            {
                "openapi": "3.0.3",
                "components": {
                    "schemas": {
                        "Good": {"$ref": "./schemas/common.json#/Id"},
                        "Bad": {"$ref": "./schemas/common.json#/Nope"},
                        "Remote": {"$ref": "https://example.com/x.json#/Y"},
                    }
                },
            },
        )
        issues = gate_files(self.root, [self.root / "openapi.yaml"])
        self.assertEqual(len(issues), 2)
        rendered = "\n".join(issue.render(self.root) for issue in issues)
        self.assertIn("Nope", rendered)
        self.assertIn("remote/scheme", rendered)

    def test_referenced_documents_are_scanned_recursively(self) -> None:
        write_json(
            self.root / "openapi.yaml",
            {"components": {"schemas": {"Root": {"$ref": "schemas/first.json#/First"}}}},
        )
        write_json(
            self.schemas / "first.json",
            {"First": {"$ref": "nested/second.json#/Second"}},
        )
        write_json(
            self.schemas / "nested" / "second.json",
            {"Second": {"$ref": "https://example.invalid/schema.json#/Remote"}},
        )
        count, issues = RefGate(self.root).check_document(self.root / "openapi.yaml")
        self.assertEqual(count, 3)
        self.assertEqual(len(issues), 1)
        self.assertIn("remote/scheme", issues[0].message)

    def test_dynamic_ref_is_rejected(self) -> None:
        write_json(
            self.schemas / "dynamic.json",
            {"A": {"$dynamicRef": "#/B"}},
        )
        issues = gate_files(self.root, [self.schemas / "dynamic.json"])
        self.assertEqual(len(issues), 1)
        self.assertIn("$dynamicRef", issues[0].message)


if __name__ == "__main__":
    unittest.main()
