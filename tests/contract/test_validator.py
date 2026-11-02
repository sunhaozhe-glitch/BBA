import json
import tempfile
import unittest
from pathlib import Path

from tools.validate_contracts import (
    ContractLoader,
    ValidationReport,
    check_fixtures,
    schema_definition_errors,
    semantic_errors,
)


class ValidatorTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name) / "contracts"
        (self.root / "schemas").mkdir(parents=True)
        self.schema_file = self.root / "schemas" / "common.json"
        self.loader = ContractLoader(self.root)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def set_schema(self, name: str, schema: dict) -> None:
        self.schema_file.write_text(json.dumps({name: schema}), encoding="utf-8")
        self.loader = ContractLoader(self.root)
        self.schema = schema

    def test_nullable_true_accepts_null_and_false_rejects(self) -> None:
        schema = {
            "type": "object",
            "additionalProperties": False,
            "required": ["value"],
            "properties": {"value": {"type": "string", "nullable": True}},
        }
        self.set_schema("Thing", schema)
        self.assertEqual(self.loader.schema_errors({"value": None}, schema, self.schema_file), [])
        self.assertEqual(self.loader.schema_errors({"value": "x"}, schema, self.schema_file), [])

        non_nullable = dict(schema)
        non_nullable["properties"] = {"value": {"type": "string"}}
        errors = self.loader.schema_errors({"value": None}, non_nullable, self.schema_file)
        self.assertTrue(any("nullable" in error for error in errors))

    def test_required_and_additional_properties_are_enforced(self) -> None:
        schema = {
            "type": "object",
            "additionalProperties": False,
            "required": ["value"],
            "properties": {"value": {"type": "integer", "minimum": 1}},
        }
        self.set_schema("Thing", schema)
        self.assertTrue(self.loader.schema_errors({}, schema, self.schema_file))
        self.assertTrue(
            self.loader.schema_errors({"value": 1, "extra": True}, schema, self.schema_file)
        )
        self.assertTrue(self.loader.schema_errors({"value": 0}, schema, self.schema_file))

    def test_datetime_requires_utc_z_and_real_calendar_date(self) -> None:
        schema = {"type": "string", "format": "date-time"}
        self.set_schema("Stamp", schema)
        self.assertEqual(
            self.loader.schema_errors("2026-10-24T12:00:00Z", schema, self.schema_file), []
        )
        self.assertTrue(
            self.loader.schema_errors("2026-10-24T12:00:00+00:00", schema, self.schema_file)
        )
        self.assertTrue(
            self.loader.schema_errors("2026-02-30T12:00:00Z", schema, self.schema_file)
        )

    def test_schema_definition_strictness(self) -> None:
        cases = [
            (
                "missing additionalProperties",
                {"type": "object", "properties": {"a": {"type": "string"}}},
            ),
            (
                "unbounded open object",
                {"type": "object", "additionalProperties": True},
            ),
            (
                "unknown keyword",
                {"type": "string", "const": "x"},
            ),
            (
                "$ref sibling",
                {"$ref": "#/A", "description": "ignored"},
            ),
            (
                "type array",
                {"type": ["string", "null"]},
            ),
            (
                "nullable without type",
                {"nullable": True},
            ),
        ]
        for label, schema in cases:
            with self.subTest(label=label):
                errors = schema_definition_errors(
                    self.loader, schema, self.schema_file, component_name="Probe"
                )
                self.assertTrue(errors, label)

    def test_bounded_open_map_and_onoeof_branch_without_type_are_allowed(self) -> None:
        bounded_map = {
            "type": "object",
            "additionalProperties": True,
            "maxProperties": 64,
        }
        self.assertEqual(
            schema_definition_errors(
                self.loader, bounded_map, self.schema_file, component_name="RawEcho"
            ),
            [],
        )
        branch = {
            "oneOf": [
                {"properties": {"action": {"type": "string", "enum": ["a"]}}},
                {"properties": {"action": {"type": "string", "enum": ["b"]}}},
            ]
        }
        self.assertEqual(
            schema_definition_errors(
                self.loader, branch, self.schema_file, component_name="Branch"
            ),
            [],
        )

    def test_request_component_must_reject_unknown_fields(self) -> None:
        schema = {
            "type": "object",
            "additionalProperties": True,
            "maxProperties": 8,
        }
        errors = schema_definition_errors(
            self.loader, schema, self.schema_file, component_name="ThingRequest"
        )
        self.assertTrue(any("additionalProperties: false" in error for error in errors))

    def test_semantic_pending_unknown_is_never_success(self) -> None:
        self.assertTrue(
            semantic_errors("Observation", {"effect_certainty": "unknown", "success": True})
        )
        self.assertTrue(
            semantic_errors("Observation", {"state": "pending", "confirmed": True})
        )
        self.assertTrue(
            semantic_errors(
                "Observation", {"outcome": "submitted", "effect_certainty": "confirmed"}
            )
        )
        self.assertEqual(
            semantic_errors("Observation", {"effect_certainty": "confirmed", "success": True}),
            [],
        )
        self.assertTrue(
            semantic_errors(
                "Observation",
                {"state": "joined", "snapshot_complete": False},
            )
        )

    def test_plugin_semantics(self) -> None:
        self.assertEqual(
            semantic_errors(
                "PluginJoinRequest",
                {"group_id": 1, "confirm": "1", "reason": "x"},
            ),
            [],
        )
        self.assertTrue(
            semantic_errors(
                "PluginJoinRequest",
                {"group_id": 1, "confirm": 1, "reason": "x"},
            )
        )
        self.assertTrue(
            semantic_errors(
                "PluginSearchRequest",
                {"keyword": "x", "count": 51},
            )
        )
        self.assertTrue(
            semantic_errors(
                "PluginJoinResponse",
                {"path": "/api/Plugin/ext/dxc-join-api/join-group"},
            )
        )
        self.assertEqual(
            semantic_errors(
                "PluginJoinResponse",
                {"path": "/api/Plugin/ext/napcat-plugin-cleaner/join-group"},
            ),
            [],
        )

    def test_account_identity_status_requires_authenticated_readback(self) -> None:
        verified = {
            "status": "verified",
            "qq_self_id": "10001",
            "identity_verified_at": "2026-10-24T12:00:00Z",
            "identity_source": "napcat_get_login_info",
        }
        verified["runtime_generation"] = 1
        self.assertEqual(semantic_errors("AccountIdentity", verified), [])
        self.assertTrue(
            semantic_errors(
                "AccountIdentity",
                {**verified, "qq_self_id": None},
            )
        )
        self.assertTrue(
            semantic_errors(
                "AccountIdentity",
                {
                    "status": "unverified",
                    "qq_self_id": "10001",
                    "identity_verified_at": None,
                    "identity_source": None,
                },
            )
        )
    def test_nullable_enum_accepts_null_only_when_nullable(self) -> None:
        schema = {"type": "string", "enum": ["read"], "nullable": True}
        self.set_schema("NullableEnum", schema)
        self.assertEqual(self.loader.schema_errors(None, schema, self.schema_file), [])
        self.assertTrue(self.loader.schema_errors("write", schema, self.schema_file))
        non_nullable = {"type": "string", "enum": ["read"]}
        self.assertTrue(self.loader.schema_errors(None, non_nullable, self.schema_file))

    def test_unknown_remote_ref_fails_closed(self) -> None:
        self.set_schema("Remote", {"$ref": "https://example.invalid/schema.json#/Value"})
        errors = self.loader.component_errors("Remote", "anything")
        self.assertTrue(errors)
        self.assertIn("https://example.invalid/schema.json", errors[0])

    def test_fixture_runner_accepts_valid_and_rejects_invalid(self) -> None:
        self.set_schema(
            "Thing",
            {
                "type": "object",
                "additionalProperties": False,
                "required": ["value"],
                "properties": {"value": {"type": "integer", "minimum": 1}},
            },
        )
        examples = self.root / "examples"
        examples.mkdir()
        (examples / "common.json").write_text(
            json.dumps(
                [
                    {
                        "schema_ref": "common.json#/Thing",
                        "valid": True,
                        "value": {"value": 1},
                        "description": "positive",
                    },
                    {
                        "schema_ref": "common.json#/Thing",
                        "valid": False,
                        "value": {"value": 0},
                        "description": "negative",
                    },
                ]
            ),
            encoding="utf-8",
        )
        loader = ContractLoader(self.root)
        report = ValidationReport()
        check_fixtures(loader, report, enforce_coverage=False)
        self.assertEqual(report.errors, [])

        (examples / "common.json").write_text(
            json.dumps(
                [
                    {
                        "schema_ref": "common.json#/Thing",
                        "valid": False,
                        "value": {"value": 1},
                        "description": "falsely invalid",
                    }
                ]
            ),
            encoding="utf-8",
        )
        loader = ContractLoader(self.root)
        report = ValidationReport()
        check_fixtures(loader, report, enforce_coverage=False)
        self.assertTrue(any("invalid fixture was accepted" in error for error in report.errors))


if __name__ == "__main__":
    unittest.main()
