import json
import tempfile
import unittest
from pathlib import Path

from tools.plugin_fake import (
    MAX_BODY_BYTES,
    CANONICAL_JOIN,
    CANONICAL_SEARCH,
    LEGACY_JOIN,
    PluginFake,
    request,
)

ROOT = Path(__file__).resolve().parents[2]
CONTRACTS = ROOT / "contracts"


def write_plugin_schema(root: Path) -> None:
    schema_dir = root / "schemas"
    schema_dir.mkdir(parents=True)
    (schema_dir / "plugin.json").write_text(
        json.dumps(
            {
                "PluginJoinRequest": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["group_id", "confirm", "reason", "answer"],
                    "properties": {
                        "group_id": {"type": "integer", "minimum": 1},
                        "confirm": {"type": "string", "enum": ["1"]},
                        "reason": {"type": "string", "maxLength": 500},
                        "answer": {"type": "string", "minLength": 1, "maxLength": 500},
                    },
                },
                "PluginSearchRequest": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["keyword", "count"],
                    "properties": {
                        "keyword": {"type": "string", "minLength": 1},
                        "count": {"type": "integer", "minimum": 1, "maximum": 50},
                    },
                },
            }
        ),
        encoding="utf-8",
    )


VALID_JOIN = {
    "group_id": 123456789,
    "confirm": "1",
    "reason": "operator-approved application text",
    "answer": "group-answer",
}


class PluginFakeTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name) / "contracts"
        write_plugin_schema(self.root)
        self.fake = PluginFake(self.root)
        self.fake.__enter__()

    def tearDown(self) -> None:
        self.fake.__exit__(None, None, None)
        self._tmp.cleanup()

    def test_canonical_join_accepts_string_confirm_one(self) -> None:
        self.assertNotIn("6099", self.fake.base_url)
        status, body = request(self.fake.base_url + CANONICAL_JOIN, payload=VALID_JOIN)
        self.assertEqual(status, 200, body)
        self.assertTrue(body["accepted"])

    def test_numeric_confirm_and_unknown_fields_are_rejected(self) -> None:
        numeric = dict(VALID_JOIN, confirm=1)
        status, body = request(self.fake.base_url + CANONICAL_JOIN, payload=numeric)
        self.assertEqual(status, 400, body)

        extra = dict(VALID_JOIN, path=LEGACY_JOIN)
        status, body = request(self.fake.base_url + CANONICAL_JOIN, payload=extra)
        self.assertEqual(status, 400, body)

    def test_legacy_dxc_route_is_not_registered(self) -> None:
        status, body = request(self.fake.base_url + LEGACY_JOIN, payload=VALID_JOIN)
        self.assertEqual(status, 404, body)
        self.assertEqual(body["error"], "NOT_FOUND")

    def test_search_count_bound(self) -> None:
        status, body = request(
            self.fake.base_url + CANONICAL_SEARCH,
            payload={"keyword": "AI", "count": 50},
        )
        self.assertEqual(status, 200, body)
        status, body = request(
            self.fake.base_url + CANONICAL_SEARCH,
            payload={"keyword": "AI", "count": 51},
        )
        self.assertEqual(status, 400, body)

    def test_invalid_json_and_oversized_body_are_rejected(self) -> None:
        from urllib.error import HTTPError
        from urllib.request import Request, urlopen

        bad = Request(
            self.fake.base_url + CANONICAL_JOIN,
            data=b"{not json",
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with self.assertRaises(HTTPError) as caught:
            urlopen(bad, timeout=5)
        try:
            self.assertEqual(caught.exception.code, 400)
        finally:
            caught.exception.close()

        oversized = Request(
            self.fake.base_url + CANONICAL_JOIN,
            data=b"{}",
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Content-Length": str(MAX_BODY_BYTES + 1),
            },
        )
        with self.assertRaises(HTTPError) as caught:
            urlopen(oversized, timeout=5)
        try:
            self.assertEqual(caught.exception.code, 400)
        finally:
            caught.exception.close()


@unittest.skipUnless(
    (CONTRACTS / "schemas" / "plugin.json").is_file()
    and (CONTRACTS / "examples" / "plugin.json").is_file(),
    "real plugin contract is not present",
)
class RealPluginSchemaFakeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.records = json.loads(
            (CONTRACTS / "examples" / "plugin.json").read_text(encoding="utf-8")
        )

    def _valid(self, component: str) -> dict:
        for record in self.records:
            if record["schema_ref"].endswith("/" + component) and record["valid"]:
                return record["value"]
        self.skipTest(f"no valid fixture for {component}")

    def test_real_schema_canonical_accepts_and_legacy_rejects(self) -> None:
        with PluginFake(CONTRACTS) as fake:
            status, body = request(fake.base_url + CANONICAL_JOIN, payload=self._valid("PluginJoinRequest"))
            self.assertEqual(status, 200, body)
            status, body = request(fake.base_url + CANONICAL_JOIN, payload={"group_id": 1, "confirm": 1, "reason": "x"})
            self.assertEqual(status, 400, body)
            status, body = request(fake.base_url + LEGACY_JOIN, payload=self._valid("PluginJoinRequest"))
            self.assertEqual(status, 404, body)
            status, body = request(
                fake.base_url + CANONICAL_SEARCH,
                payload=self._valid("PluginSearchRequest"),
            )
            self.assertEqual(status, 200, body)


if __name__ == "__main__":
    unittest.main()
