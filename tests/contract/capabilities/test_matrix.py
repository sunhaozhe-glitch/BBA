from __future__ import annotations

import json
import unittest
from pathlib import Path
from typing import Any

from tools.validate_contracts import ContractLoader

ROOT = Path(__file__).resolve().parents[3]
CONTRACTS = ROOT / "contracts"
MATRIX_PATH = Path(__file__).with_name("napcat-plugin-cleaner.matrix.json")

EXPECTED_ACTIONS = {
    "search_groups",
    "request_join",
    "observe_membership",
    "observe_join_request",
    "cancel_join_request",
    "send_text",
}
REQUIRED_ACTION_FIELDS = {
    "action",
    "status",
    "effect",
    "input_schema",
    "output_schema",
    "pagination",
    "external_effect",
    "reference_id",
    "read_back",
    "outcomes",
    "timeout_ms",
    "retry",
    "writer_surface",
    "evidence_refs",
}


def load_matrix() -> dict[str, Any]:
    return json.loads(MATRIX_PATH.read_text(encoding="utf-8"))


class CapabilityMatrixTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.matrix = load_matrix()
        cls.loader = ContractLoader(CONTRACTS)

    def test_every_bridge_action_has_fail_closed_evidence_fields(self) -> None:
        actions = self.matrix["actions"]
        by_name = {item["action"]: item for item in actions}
        self.assertEqual(set(by_name), EXPECTED_ACTIONS)

        for action, record in by_name.items():
            with self.subTest(action=action):
                self.assertTrue(REQUIRED_ACTION_FIELDS.issubset(record))
                self.assertIn(record["status"], {"supported", "unsupported", "unverified"})
                self.assertIn(record["effect"], {"read", "write"})
                self.assertTrue(record["evidence_refs"])
                self.assertIn("kind", record["pagination"])
                self.assertTrue({"returned", "stable", "schema"}.issubset(record["reference_id"]))
                self.assertTrue({"supported", "method"}.issubset(record["read_back"]))
                self.assertTrue({"timeout", "unknown", "rejected", "no_effect"}.issubset(record["outcomes"]))
                self.assertTrue({"plugin_internal", "adapter_retry", "policy"}.issubset(record["retry"]))

                if record["status"] == "supported":
                    self.assertEqual(self.matrix["adapter_scope"]["verification_state"], "verified")
                    self.assertIsNotNone(self.matrix["adapter_scope"]["verified_at"])
                self.assertIsInstance(self.loader.resolve_component(record["input_schema"])[0], dict)
                self.assertIsInstance(self.loader.resolve_component(record["output_schema"])[0], dict)

    def test_unsupported_observation_and_write_actions_fail_closed(self) -> None:
        actions = {item["action"]: item for item in self.matrix["actions"]}
        for action in (
            "observe_membership",
            "observe_join_request",
            "cancel_join_request",
            "send_text",
        ):
            with self.subTest(action=action):
                self.assertEqual(actions[action]["status"], "unsupported")

        self.assertEqual(self.matrix["adapter_scope"]["verification_state"], "unverified")
        self.assertTrue(all(item["status"] != "supported" for item in self.matrix["actions"]))
        self.assertFalse(self.matrix["safety_invariants"]["unknown_capability_supported"])

    def test_search_source_contract_has_no_cursor_and_is_candidate_only(self) -> None:
        search = next(item for item in self.matrix["actions"] if item["action"] == "search_groups")
        self.assertEqual(search["pagination"]["kind"], "none")
        self.assertFalse(search["pagination"]["cursor_input"])
        self.assertFalse(search["pagination"]["cursor_output"])
        self.assertFalse(search["reference_id"]["returned"])
        self.assertFalse(search["read_back"]["supported"])

        request_schema, _ = self.loader.resolve_component("PluginSearchRequest")
        response_schema, _ = self.loader.resolve_component("PluginSearchResponse")
        self.assertEqual(set(request_schema["properties"]), {"keyword", "count"})
        self.assertNotIn("next_cursor", str(response_schema))

    def test_join_has_no_stable_reference_or_read_back(self) -> None:
        join = next(item for item in self.matrix["actions"] if item["action"] == "request_join")
        self.assertFalse(join["reference_id"]["returned"])
        self.assertFalse(join["reference_id"]["stable"])
        self.assertFalse(join["read_back"]["supported"])
        self.assertIn("result=1", join["outcomes"]["unknown"])
        self.assertIn("unknown", join["outcomes"]["timeout"])

    def test_membership_absence_does_not_claim_no_pending_request(self) -> None:
        query_fixtures = [
            item
            for item in json.loads((CONTRACTS / "examples" / "bridge.json").read_text(encoding="utf-8"))
            if item["schema_ref"].endswith("/ObservationQueryResult") and item["valid"]
        ]
        membership = [item["value"] for item in query_fixtures if item["value"]["kind"] == "membership"]
        pending = [item["value"] for item in query_fixtures if item["value"]["kind"] == "join_request"]
        self.assertTrue(membership)
        self.assertTrue(pending)
        self.assertFalse(any(item["kind"] == "join_request" for item in membership))
        self.assertTrue(all(item["kind"] == "join_request" for item in pending))
        self.assertFalse(self.matrix["safety_invariants"]["membership_not_joined_proves_no_pending_request"])

    def test_unknown_write_timeout_is_never_auto_replayed(self) -> None:
        join = next(item for item in self.matrix["actions"] if item["action"] == "request_join")
        self.assertEqual(join["retry"]["policy"], "never automatically replay an ambiguous dispatch")
        self.assertFalse(self.matrix["safety_invariants"]["unknown_dispatch_automatic_retry"])

        self.assertEqual(join["timeout_ms"], 15000)
        self.assertIn("underlying Promise is not cancelled", join["outcomes"]["timeout"])
        self.assertFalse(join["reference_id"]["returned"])
        self.assertFalse(join["read_back"]["supported"])

    def test_writer_scope_does_not_claim_global_exclusivity(self) -> None:
        actions = {item["action"]: item for item in self.matrix["actions"]}
        join_writer = actions["request_join"]["writer_surface"]
        send_writer = actions["send_text"]["writer_surface"]
        self.assertIn("in-process NapCat GroupService", join_writer)
        self.assertIn("not exhaustively enumerated", join_writer)
        self.assertIn("not exhaustively enumerated", send_writer)

    def test_related_inbound_flag_action_is_not_outbound_observation(self) -> None:
        related = self.matrix["related_but_not_equivalent"][0]
        self.assertEqual(related["action"], "set_group_add_request")
        self.assertEqual(related["effect"], "write")
        self.assertIn("not observe_join_request", related["semantics"])
        self.assertIn("not cancellation", related["semantics"])

        scope = self.matrix["adapter_scope"]
        self.assertTrue(scope["plugin_enabled_in_static_config"])
        self.assertEqual(scope["runtime_loaded"], "unverified")
        self.assertEqual(scope["account_binding"], "unknown")


if __name__ == "__main__":
    unittest.main()
