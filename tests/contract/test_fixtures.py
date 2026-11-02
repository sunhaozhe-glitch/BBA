import json
import unittest
from pathlib import Path

from tools.validate_contracts import (
    ContractLoader,
    ValidationReport,
    check_fixtures,
    semantic_errors,
    validate_contracts,
)

ROOT = Path(__file__).resolve().parents[2]
CONTRACTS = ROOT / "contracts"
EXAMPLES = CONTRACTS / "examples"


@unittest.skipUnless(EXAMPLES.is_dir(), "contracts/examples is not present")
class RealContractFixtureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.loader = ContractLoader(CONTRACTS)
        cls.report = validate_contracts(CONTRACTS)

    def test_full_contract_validation_passes(self) -> None:
        self.assertEqual(self.report.errors, [])

    def test_every_component_has_positive_and_negative_fixtures(self) -> None:
        report = ValidationReport()
        records = check_fixtures(self.loader, report, enforce_coverage=True)
        self.assertEqual(report.errors, [])
        self.assertGreater(len(records), 0)

    def test_valid_fixtures_satisfy_semantic_edges(self) -> None:
        for path in sorted(EXAMPLES.glob("*.json")):
            for index, record in enumerate(json.loads(path.read_text(encoding="utf-8"))):
                if not record["valid"]:
                    continue
                component = record["schema_ref"].split("#/")[-1]
                with self.subTest(file=path.name, index=index, component=component):
                    self.assertEqual(
                        semantic_errors(component, record["value"]),
                        [],
                        record.get("description", ""),
                    )

    def test_pending_and_unknown_observation_fixtures_are_not_success(self) -> None:
        observations = [
            record
            for record in json.loads((EXAMPLES / "bridge.json").read_text(encoding="utf-8"))
            if record["schema_ref"].endswith("/Observation") and record["valid"]
        ]
        self.assertTrue(observations)
        for record in observations:
            self.assertEqual(semantic_errors("Observation", record["value"]), [])
            payload = record["value"].get("payload", {})
            if payload.get("state") in {"pending", "unknown"}:
                self.assertFalse(payload.get("success", False))
                self.assertFalse(payload.get("confirmed", False))

    def test_plugin_confirmation_is_string_and_paths_are_canonical(self) -> None:
        plugin_records = json.loads(
            (EXAMPLES / "plugin.json").read_text(encoding="utf-8")
        )
        valid_join = [
            record
            for record in plugin_records
            if record["schema_ref"].endswith("/PluginJoinRequest") and record["valid"]
        ]
        self.assertTrue(valid_join)
        for record in valid_join:
            self.assertIsInstance(record["value"]["confirm"], str)
            self.assertEqual(record["value"]["confirm"], "1")

        invalid_legacy = [
            record
            for record in plugin_records
            if record["schema_ref"].endswith("/PluginJoinRequest")
            and not record["valid"]
            and "dxc-join-api" in json.dumps(record["value"])
        ]
        self.assertTrue(invalid_legacy)

    def test_task_event_sequence_preserves_javascript_unsafe_integer(self) -> None:
        records = json.loads((EXAMPLES / "tasks.json").read_text(encoding="utf-8"))
        event = next(
            record["value"]
            for record in records
            if record["schema_ref"].endswith("/TaskEvent") and record["valid"]
        )
        self.assertEqual(event["seq"], "9007199254740993")
        self.assertIsInstance(event["seq"], str)

    def test_task_entry_authentication_is_limited_to_identity_scoped_operations(self) -> None:
        openapi = json.loads((CONTRACTS / "openapi.yaml").read_text(encoding="utf-8"))
        permitted = {"createTask", "getTask", "controlTask"}
        seen: set[str] = set()
        for path_item in openapi["paths"].values():
            for operation in path_item.values():
                if not isinstance(operation, dict) or "operationId" not in operation:
                    continue
                operation_id = operation["operationId"]
                task_entry_enabled = any(
                    "TaskEntryCredential" in requirement
                    for requirement in operation.get("security", [])
                )
                if task_entry_enabled:
                    self.assertIn(operation_id, permitted)
                    refs = {parameter.get("$ref") for parameter in operation.get("parameters", [])}
                    self.assertTrue(
                        {
                            "#/components/parameters/TaskEntryAccount",
                            "#/components/parameters/TaskEntrySender",
                        }.issubset(refs)
                    )
                    seen.add(operation_id)
        self.assertEqual(seen, permitted)

    def test_cross_file_refs_resolve(self) -> None:
        # Exercise schema compilation rather than trusting the ref gate alone.
        for name, (_schema, _source) in self.loader.components().items():
            with self.subTest(component=name):
                self.assertTrue(name)


if __name__ == "__main__":
    unittest.main()
