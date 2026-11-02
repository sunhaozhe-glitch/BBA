from __future__ import annotations

import json
import socket
import time
import unittest
from pathlib import Path
from typing import Any

from tools.plugin_fake import CANONICAL_JOIN, CANONICAL_SEARCH, LEGACY_JOIN, LEGACY_SEARCH, PluginFake, request
from tools.validate_contracts import ContractLoader, semantic_errors

ROOT = Path(__file__).resolve().parents[3]
CONTRACTS = ROOT / "contracts"
FIXTURES_PATH = Path(__file__).with_name("adapter-fixtures.jsonl")


def load_fixtures() -> list[dict[str, Any]]:
    return [
        json.loads(line)
        for line in FIXTURES_PATH.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def fixture_for(fixtures: list[dict[str, Any]], component: str, valid: bool = True) -> dict[str, Any]:
    suffix = "#/" + component
    return next(
        item
        for item in fixtures
        if item["schema_ref"].endswith(suffix) and item["valid"] is valid
    )


class AdapterFixtureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.fixtures = load_fixtures()
        cls.loader = ContractLoader(CONTRACTS)

    def test_jsonl_scenarios_match_contract_schemas_and_semantics(self) -> None:
        for index, item in enumerate(self.fixtures):
            component = item["schema_ref"].split("#/")[-1]
            errors = self.loader.component_errors(component, item["value"])
            semantic = semantic_errors(component, item["value"])
            with self.subTest(index=index, component=component, valid=item["valid"]):
                if item["valid"]:
                    self.assertEqual(errors + semantic, [], item["description"])
                else:
                    self.assertTrue(errors + semantic, item["description"])

    def test_outcome_fixtures_distinguish_unknown_rejected_and_no_effect(self) -> None:
        unknown = next(
            item["value"]
            for item in self.fixtures
            if item["schema_ref"].endswith("#/Operation")
            and item["valid"]
            and item["value"]["state"] == "UNKNOWN"
        )
        rejected = next(
            item["value"]
            for item in self.fixtures
            if item["schema_ref"].endswith("#/Operation")
            and item["valid"]
            and item["value"]["disposition"] == "rejected"
        )
        no_effect = next(
            item["value"]
            for item in self.fixtures
            if item["schema_ref"].endswith("#/Operation")
            and item["valid"]
            and item["value"]["disposition"] == "failed_no_effect"
        )
        self.assertEqual((unknown["disposition"], unknown["effect_certainty"]), ("unknown", "unknown"))
        self.assertEqual((rejected["disposition"], rejected["effect_certainty"]), ("rejected", "confirmed_rejected"))
        self.assertEqual((no_effect["disposition"], no_effect["effect_certainty"]), ("failed_no_effect", "no_effect"))
    def test_membership_not_joined_does_not_answer_pending_request(self) -> None:
        membership = next(
            item["value"]
            for item in self.fixtures
            if item["schema_ref"].endswith("#/ObservationQueryResult")
            and item["valid"]
            and item["value"]["kind"] == "membership"
        )
        join_request = next(
            item["value"]
            for item in self.fixtures
            if item["schema_ref"].endswith("#/ObservationQueryResult")
            and item["valid"]
            and item["value"]["kind"] == "join_request"
        )
        self.assertEqual((membership["kind"], membership["state"]), ("membership", "not_joined"))
        self.assertEqual((join_request["kind"], join_request["state"]), ("join_request", "unknown"))

    def test_fake_http_returns_schema_checked_canonical_plugin_responses(self) -> None:
        search = fixture_for(self.fixtures, "PluginSearchResponse")["value"]
        join = fixture_for(self.fixtures, "PluginJoinResponse")["value"]
        with PluginFake(
            CONTRACTS,
            responses={CANONICAL_SEARCH: search, CANONICAL_JOIN: join},
        ) as fake:
            search_status, search_body = request(
                fake.base_url + CANONICAL_SEARCH,
                payload={"keyword": "Example", "count": 20},
            )
            join_status, join_body = request(
                fake.base_url + CANONICAL_JOIN,
                payload={"group_id": 123456789, "confirm": "1", "reason": "fixture only", "answer": "fixture answer"},
            )

        self.assertEqual(search_status, 200)
        self.assertEqual(search_body, search)
        self.assertEqual(join_status, 200)
        self.assertEqual(join_body, join)
        self.assertEqual(join_body["result"], 1)
        self.assertNotIn("request_ref", join_body)
        self.assertNotIn("membership", join_body)

    def test_invalid_fake_response_fails_closed_before_http_success(self) -> None:
        invalid_response = {"ok": True, "result": -1}
        with PluginFake(CONTRACTS, responses={CANONICAL_JOIN: invalid_response}) as fake:
            status, body = request(
                fake.base_url + CANONICAL_JOIN,
                payload={"group_id": 123456789, "confirm": "1", "reason": "fixture only", "answer": "fixture answer"},
            )
        self.assertEqual(status, 500)
        self.assertEqual(body["error"], "FAKE_RESPONSE_CONTRACT_INVALID")

    def test_canonical_routes_and_legacy_route_rejection(self) -> None:
        search = fixture_for(self.fixtures, "PluginSearchResponse")["value"]
        join = fixture_for(self.fixtures, "PluginJoinResponse")["value"]
        with PluginFake(
            CONTRACTS,
            responses={CANONICAL_SEARCH: search, CANONICAL_JOIN: join},
        ) as fake:
            legacy_search = request(
                fake.base_url + LEGACY_SEARCH,
                payload={"keyword": "Example", "count": 20},
            )
            legacy_join = request(
                fake.base_url + LEGACY_JOIN,
                payload={"group_id": 123456789, "confirm": "1", "reason": "fixture only", "answer": "fixture answer"},
            )
            unknown_route = request(
                fake.base_url + "/api/Plugin/ext/napcat-plugin-cleaner/join-status",
                payload={},
            )
        self.assertEqual(legacy_search[0], 404)
        self.assertEqual(legacy_join[0], 404)
        self.assertEqual(unknown_route[0], 404)

    def test_post_dispatch_timeout_stays_unknown_and_is_not_replayed_by_fake(self) -> None:
        unknown = next(
            item["value"]
            for item in self.fixtures
            if item["schema_ref"].endswith("#/Operation")
            and item["valid"]
            and item["value"]["state"] == "UNKNOWN"
        )
        join_response = fixture_for(self.fixtures, "PluginJoinResponse")["value"]
        with PluginFake(
            CONTRACTS,
            responses={CANONICAL_JOIN: join_response},
            response_delays={CANONICAL_JOIN: 0.2},
        ) as fake:
            with self.assertRaises((TimeoutError, socket.timeout)):
                request(
                    fake.base_url + CANONICAL_JOIN,
                    payload={"group_id": 123456789, "confirm": "1", "reason": "fixture only", "answer": "fixture answer"},
                    timeout=0.04,
                )
            time.sleep(0.25)
            count_after_timeout = fake.request_counts.get(CANONICAL_JOIN, 0)
            membership_route = request(
                fake.base_url + "/api/Plugin/ext/napcat-plugin-cleaner/membership",
                payload={},
            )

        self.assertEqual(count_after_timeout, 1)
        self.assertEqual(membership_route[0], 404)
        self.assertEqual(unknown["state"], "UNKNOWN")
        self.assertEqual(unknown["disposition"], "unknown")
        self.assertIsNone(unknown["external_request_ref"])
        self.assertEqual(self.loader.component_errors("Operation", unknown), [])


if __name__ == "__main__":
    unittest.main()
