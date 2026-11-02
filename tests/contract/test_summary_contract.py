"""Summary.v1 contract edges exercised through ``ContractLoader``.

The valid fixture in ``contracts/examples/models.json`` is the base for each
mutation so every test isolates one rule at a time. Lengths count Unicode code
points, matching JSON Schema ``maxLength`` semantics.
"""

import copy
import json
import unittest
from pathlib import Path

from tools.validate_contracts import ContractLoader, semantic_errors

ROOT = Path(__file__).resolve().parents[2]
CONTRACTS = ROOT / "contracts"
MODELS_EXAMPLES = CONTRACTS / "examples" / "models.json"


class SummaryContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.loader = ContractLoader(CONTRACTS)
        records = json.loads(MODELS_EXAMPLES.read_text(encoding="utf-8"))
        cls.summary = next(
            record["value"]
            for record in records
            if record["schema_ref"] == "models.json#/Summary" and record["valid"]
        )

    def base(self) -> dict:
        return copy.deepcopy(self.summary)

    def errors(self, value: object) -> list[str]:
        return self.loader.component_errors("Summary", value)

    def assert_accepted(self, value: object) -> None:
        self.assertEqual(self.errors(value), [])

    def assert_rejected_at(self, value: object, path: str) -> None:
        errors = self.errors(value)
        self.assertTrue(errors, f"expected {path} to be rejected")
        self.assertTrue(
            any(error.startswith(path + ":") for error in errors),
            f"expected an error rooted at {path}, got {errors}",
        )

    def test_required_nested_producer_fields(self) -> None:
        paths = {
            "producer": ("producer",),
            "producer.account_id": ("producer", "account_id"),
            "producer.collector_scope": ("producer", "collector_scope"),
            "producer.plugin_version": ("producer", "plugin_version"),
            "producer.source_manifest_sha256": ("producer", "source_manifest_sha256"),
            "producer.collector_scope.account_id": (
                "producer",
                "collector_scope",
                "account_id",
            ),
            "producer.collector_scope.conversation_kind": (
                "producer",
                "collector_scope",
                "conversation_kind",
            ),
            "producer.collector_scope.conversation_id": (
                "producer",
                "collector_scope",
                "conversation_id",
            ),
        }
        for name, keys in paths.items():
            with self.subTest(field=name):
                value = self.base()
                container = value
                for key in keys[:-1]:
                    container = container[key]
                del container[keys[-1]]
                self.assert_rejected_at(value, "/".join(keys[:-1]) or "<root>")

    def test_required_nested_collector_scope_and_topic_fields(self) -> None:
        cases = {
            "collector_scope": ("collector_scope",),
            "collector_scope.account_id": ("collector_scope", "account_id"),
            "collector_scope.conversation_kind": (
                "collector_scope",
                "conversation_kind",
            ),
            "collector_scope.conversation_id": (
                "collector_scope",
                "conversation_id",
            ),
            "topics[0].summary": ("topics", 0, "summary"),
            "topics[0].message_refs": ("topics", 0, "message_refs"),
            "limitations": ("limitations",),
        }
        for name, keys in cases.items():
            with self.subTest(field=name):
                value = self.base()
                container = value
                for key in keys[:-1]:
                    container = container[key]
                del container[keys[-1]]
                path = "/".join(str(key) for key in keys[:-1]) or "<root>"
                self.assert_rejected_at(value, path)

    def test_privacy_class_is_a_closed_enum(self) -> None:
        for privacy_class in ("local_only", "redacted_external_allowed"):
            with self.subTest(privacy_class=privacy_class):
                value = self.base()
                value["privacy_class"] = privacy_class
                self.assert_accepted(value)

        for privacy_class in ("public", "external", "", None):
            with self.subTest(privacy_class=privacy_class):
                value = self.base()
                value["privacy_class"] = privacy_class
                self.assert_rejected_at(value, "privacy_class")

    def test_retention_days_is_bounded_to_one_through_365(self) -> None:
        for retention_days in (1, 365):
            with self.subTest(retention_days=retention_days):
                value = self.base()
                value["retention_days"] = retention_days
                self.assert_accepted(value)

        for retention_days in (0, -1, 366, 1.5, "7"):
            with self.subTest(retention_days=retention_days):
                value = self.base()
                value["retention_days"] = retention_days
                self.assert_rejected_at(value, "retention_days")

    def test_expires_at_must_be_utc_with_a_trailing_z(self) -> None:
        value = self.base()
        value["expires_at"] = "2026-10-31T11:00:00Z"
        self.assert_accepted(value)

        for expires_at in (
            "2026-10-31T11:00:00+08:00",
            "2026-10-31T11:00:00+00:00",
            "2026-10-31T11:00:00",
            "2026-10-31 11:00:00Z",
            "2026-10-31T11:00:00z",
        ):
            with self.subTest(expires_at=expires_at):
                value = self.base()
                value["expires_at"] = expires_at
                self.assert_rejected_at(value, "expires_at")

    def test_unknown_fields_are_rejected_at_every_depth(self) -> None:
        cases = {
            "<root>": ("provenance_note",),
            "producer": ("producer", "hostname"),
            "producer/collector_scope": (
                "producer",
                "collector_scope",
                "label",
            ),
            "collector_scope": ("collector_scope", "label"),
            "topics/0": ("topics", 0, "confidence"),
        }
        for path, keys in cases.items():
            with self.subTest(path=path):
                value = self.base()
                container = value
                for key in keys[:-1]:
                    container = container[key]
                container[keys[-1]] = "extra"
                self.assert_rejected_at(value, path)

    def test_message_refs_accept_500_and_reject_501(self) -> None:
        value = self.base()
        value["topics"][0]["message_refs"] = [
            f"msg_boundary_{index}" for index in range(500)
        ]
        self.assert_accepted(value)

        value["topics"][0]["message_refs"].append("msg_boundary_500")
        self.assert_rejected_at(value, "topics/0/message_refs")

    def test_topic_summary_accepts_4000_unicode_characters_and_rejects_4001(
        self,
    ) -> None:
        # U+1D11E is a single code point outside the BMP; JSON Schema maxLength
        # counts code points, not UTF-8 bytes or UTF-16 code units.
        value = self.base()
        value["topics"][0]["summary"] = "\U0001d11e" * 4000
        self.assert_accepted(value)

        value["topics"][0]["summary"] += "\U0001d11e"
        self.assert_rejected_at(value, "topics/0/summary")


    def test_source_manifest_sha256_requires_64_lowercase_hex(self) -> None:
        value = self.base()
        self.assert_accepted(value)

        for digest in (
            "0" * 63,
            "0" * 65,
            "g" * 64,
            "A" * 64,
            "0123456789abcdef0123456789abcdef01234567",
        ):
            with self.subTest(digest=digest):
                value = self.base()
                value["producer"]["source_manifest_sha256"] = digest
                self.assert_rejected_at(value, "producer/source_manifest_sha256")
    def test_producer_identity_matches_summary_collector_scope(self) -> None:
        value = self.base()
        self.assertEqual(semantic_errors("Summary", value), [])

        value["producer"]["account_id"] = "acc_other"
        self.assertTrue(semantic_errors("Summary", value))

        value = self.base()
        value["producer"]["collector_scope"]["conversation_id"] = "987654321"
        self.assertTrue(semantic_errors("Summary", value))


if __name__ == "__main__":
    unittest.main()
