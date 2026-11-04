"""Independent B7 wire oracle, derived from the existing Go implementation.

Source review: operator_conversation.go (handler maps, decoding, pagination,
replay), task.go (request keys), task_http.go (rich errors), resolver.go
(operator/runtime Bearer dispatch), migrations 0016/0017 (persisted shapes).
Samples below are constructed here, never loaded from the contract author's
fixtures. These are static contract regressions, not HTTP/database/live tests.
UTF-8 byte limits and durable replay semantics also need execution coverage;
JSON Schema character lengths cannot prove either property.
"""

import copy
import json
import unittest
from pathlib import Path

from tools.validate_contracts import ContractLoader

ROOT = Path(__file__).resolve().parents[2]
SCHEMA_FILE = "operator-conversations.json"
STAMP = "2026-11-04T00:00:00Z"
DIGEST = "a" * 64
LARGE_INTEGER = 9007199254740993  # Intentionally above JavaScript's exact range.
INT64_MAX = 9223372036854775807

# method, path, request component, success responses, identity, permission
OPERATIONS = (
    ("get", "/v1/operator/conversations", None,
     {"200": "OperatorConversationPageEnvelope"}, "OperatorBearer", "conversation.read"),
    ("get", "/v1/operator/conversations/{id}/messages", None,
     {"200": "OperatorMessagePageEnvelope"}, "OperatorBearer", "conversation.read"),
    ("post", "/v1/operator/agent-threads/{id}/messages", "OperatorAgentMessageRequest",
     {"201": "OperatorAgentMessageEnvelope"}, "OperatorBearer", "conversation.write"),
    ("post", "/v1/operator/qq-drafts", "OperatorQQDraftRequest",
     {"201": "OperatorQQDraftEnvelope"}, "OperatorBearer", "conversation.write"),
    ("get", "/v1/operator/qq-drafts/{id}", None,
     {"200": "OperatorQQDraftDetailEnvelope"}, "OperatorBearer", "conversation.read"),
    ("post", "/v1/operator/qq-inbox/events", "OperatorQQInboxEventRequest",
     {"200": "OperatorQQInboxEventEnvelope", "201": "OperatorQQInboxEventEnvelope"},
     "RuntimeCredential", "chat.trigger"),
    ("get", "/v1/operator/notifications", None,
     {"200": "OperatorNotificationPageEnvelope"}, "OperatorBearer", "conversation.read"),
    ("post", "/v1/operator/notifications/{id}", "OperatorNotificationReadRequest",
     {"200": "OperatorNotificationReadEnvelope"}, "OperatorBearer", "notification.read"),
)
PUBLIC_COMPONENTS = {
    "OperatorConversationPageEnvelope", "OperatorMessagePageEnvelope",
    "OperatorAgentMessageRequest", "OperatorAgentMessageEnvelope",
    "OperatorQQDraftRequest", "OperatorQQDraftEnvelope", "OperatorQQDraftDetailEnvelope",
    "OperatorQQInboxEventRequest", "OperatorQQInboxEventEnvelope",
    "OperatorNotificationPageEnvelope", "OperatorNotificationReadRequest",
    "OperatorNotificationReadEnvelope", "OperatorConversationErrorEnvelope",
}
REQUESTS = {
    "OperatorAgentMessageRequest": {"body": "independent synthetic message"},
    "OperatorQQDraftRequest": {
        "account_id": "acc_contract", "conversation_kind": "group",
        "conversation_id": "12345678901234567890", "body": "review only",
        "expected_version": 1,
    },
    "OperatorQQInboxEventRequest": {
        "account_id": "acc_contract", "conversation_kind": "private",
        "conversation_id": "12345678901234567890", "event_id": "event_contract",
        "body": "inbound evidence",
    },
    "OperatorNotificationReadRequest": {"expected_version": 1, "read": True},
}


def page(item):
    return {"data": {"items": [item], "next_cursor": ""}}


CONVERSATION = {
    "id": "thread_contract", "kind": "agent", "account_id": None,
    "conversation_kind": "agent", "conversation_id": "external_thread_identity",
    "version": 1, "created_at": STAMP, "last_event_at": STAMP, "unread_count": 0,
}
MESSAGE = {
    "id": "msg_contract", "source": "agent", "direction": "inbound",
    "delivery_state": "received", "body": "independent message", "body_digest": DIGEST,
    "seq": 1, "created_at": STAMP,
}
NOTIFICATION = {
    "id": "notice_contract", "event_type": "agent.message", "object_id": "msg_contract",
    "account_id": None, "conversation_id": None, "summary": "Agent thread message",
    "evidence_ref": "", "delivery_state": "local_only", "read_at": None,
    "version": 1, "created_at": STAMP,
}
AGENT_RECEIPT = {
    "id": "msg_contract", "conversation_id": "thread_contract", "source": "agent",
    "direction": "inbound", "delivery_state": "received", "body_digest": DIGEST,
    "send_permit": "absent",
}
DRAFT_RECEIPT = {
    "id": "draft_contract", "message_id": "msg_contract", "payload_digest": DIGEST,
    "rules_version": 0, "decision": "draft", "send_permit": "absent", "version": 1,
    "evidence": {"hits": [], "text_digest": DIGEST,
                 "raw_text_stored_in_review": False, "send_permit": "absent"},
}
INBOX_RECEIPT = {
    "id": "msg_contract", "conversation_id": "conv_contract", "source": "qq",
    "direction": "inbound", "delivery_state": "received", "event_id": "event_contract",
    "body_digest": DIGEST, "idempotent_replay": False,
}
RESPONSES = {
    "OperatorConversationPageEnvelope": page(CONVERSATION),
    "OperatorMessagePageEnvelope": page(MESSAGE),
    "OperatorNotificationPageEnvelope": page(NOTIFICATION),
    "OperatorAgentMessageEnvelope": {"data": AGENT_RECEIPT},
    "OperatorQQDraftEnvelope": {"data": DRAFT_RECEIPT},
    "OperatorQQDraftDetailEnvelope": {
        "data": dict(DRAFT_RECEIPT, account_id="acc_contract", conversation_id="conv_contract")
    },
    "OperatorQQInboxEventEnvelope": {"data": INBOX_RECEIPT},
    "OperatorNotificationReadEnvelope": {
        "data": {"id": "notice_contract", "read": False, "version": 2,
                 "delivery_state": "local_only"}
    },
}


class OperatorConversationContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.loader = ContractLoader(ROOT / "contracts")
        cls.openapi = json.loads((ROOT / "contracts/openapi.yaml").read_text())

    def operation(self, method, path):
        self.assertTrue(path in self.openapi["paths"], f"missing B7 path {path}")
        self.assertIn(method, self.openapi["paths"][path], f"missing {method.upper()} {path}")
        return self.openapi["paths"][path][method]

    def errors(self, component, value):
        self.assertTrue(component in self.loader.components(), f"missing B7 schema {component}")
        return self.loader.component_errors(component, value)

    def accepted(self, component, value):
        self.assertEqual(self.errors(component, value), [], (component, value))

    def rejected(self, component, value):
        self.assertTrue(self.errors(component, value), f"{component} accepted {value!r}")

    def dereference(self, node):
        while node.get("$ref", "").startswith("#/"):
            ref = node["$ref"]
            node = self.openapi
            for segment in ref[2:].split("/"):
                node = node[segment.replace("~1", "/").replace("~0", "~")]
        return node

    def resolved_schema(self, node, source=None):
        source = source or self.loader.openapi_path
        while "$ref" in node:
            source, node = self.loader.gate.resolve(node["$ref"], source)
        return node

    def parameters(self, method, path):
        operation = self.operation(method, path)
        params = self.openapi["paths"][path].get("parameters", []) + operation.get("parameters", [])
        return {(p["in"], p["name"]): p for p in map(self.dereference, params)}

    def parameter_errors(self, parameter, value):
        return self.loader.schema_errors(value, parameter["schema"], self.loader.openapi_path)

    def assert_component_schema(self, schema, component):
        # The wire shape matters, not direct-vs-registered refs or key order.
        expected, source = self.loader.resolve_component(component)
        self.assertEqual(self.resolved_schema(schema), self.resolved_schema(expected, source))

    def test_all_thirteen_public_components_registered(self):
        for component in sorted(PUBLIC_COMPONENTS):
            with self.subTest(component=component):
                self.assertTrue(component in self.loader.components(), f"missing B7 schema {component}")
                _, source = self.loader.resolve_component(component)
                self.assertEqual(source.name, SCHEMA_FILE)
                registered = self.openapi["components"]["schemas"]
                self.assertIn(component, registered)
                self.assert_component_schema(registered[component], component)

    def test_eight_operations_have_correct_request_response_and_bearer_identity(self):
        for method, path, request, successes, identity, permission in OPERATIONS:
            with self.subTest(method=method, path=path):
                operation = self.operation(method, path)
                self.assertEqual(operation.get("security"), [{identity: []}])
                self.assertIn(permission, json.dumps(operation), "document the actual permission")
                if identity == "RuntimeCredential":
                    self.assertIn("conversation.write", json.dumps(operation))
                if request:
                    body = self.dereference(operation["requestBody"])
                    self.assertTrue(body["required"])
                    self.assert_component_schema(body["content"]["application/json"]["schema"], request)
                else:
                    self.assertNotIn("requestBody", operation)
                actual_successes = {s for s in operation["responses"] if s.startswith("2")}
                self.assertEqual(actual_successes, set(successes))
                for status, component in successes.items():
                    response = self.dereference(operation["responses"][status])
                    self.assert_component_schema(response["content"]["application/json"]["schema"], component)
                self.assertTrue({"401", "403"}.issubset(operation["responses"]))
                if request:
                    self.assertTrue({"400", "409", "415"}.issubset(operation["responses"]))
                for status, response in operation["responses"].items():
                    if status.startswith("2"):
                        continue
                    response = self.dereference(response)
                    self.assert_component_schema(response["content"]["application/json"]["schema"],
                                              "OperatorConversationErrorEnvelope")

    def test_write_decoder_errors_advertise_plaintext_json_and_rich_json(self):
        # operatorDecode uses Go 1.26 http.Error: text/plain; charset=utf-8.
        # Its JSON-looking body is a STRING for text/plain consumers. Parsing
        # it first would conceal a media schema that incorrectly expects an
        # object. GET has no operatorDecode.
        writes = [(method, path) for method, path, request, *_ in OPERATIONS if request]
        self.assertEqual(len(writes), 4)
        for method, path in writes:
            operation = self.operation(method, path)
            for status, code in (("400", "VALIDATION_ERROR"), ("415", "UNSUPPORTED_MEDIA_TYPE")):
                for media_type in ("text/plain", "application/json"):
                    with self.subTest(path=path, status=status, media_type=media_type):
                        response = self.dereference(operation["responses"][status])
                        content = response.get("content", {})
                        self.assertTrue(media_type in content,
                                        f"{method.upper()} {path} {status} missing {media_type}")
                        schema = content[media_type]["schema"]
                        minimal = {"error": {"code": code}}
                        # Exact synthetic http.Error payload, including newline.
                        raw_text = '{"error":{"code":"' + code + '"}}\n'
                        if media_type == "text/plain":
                            self.assertEqual(self.loader.schema_errors(
                                raw_text, schema, self.loader.openapi_path), [],
                                "text/plain must accept the raw response string before JSON parsing")
                            self.assertEqual(self.resolved_schema(schema).get("type"), "string")
                            self.assertTrue(self.loader.schema_errors(
                                minimal, schema, self.loader.openapi_path),
                                "text/plain schema must reject a parsed object instance")
                            # Content semantics are a separate, explicit parse.
                            parsed = json.loads(raw_text)
                            self.assertEqual(parsed, minimal)
                            self.accepted("OperatorConversationErrorEnvelope", parsed)
                        else:
                            self.assert_component_schema(schema, "OperatorConversationErrorEnvelope")
                            self.assertEqual(self.loader.schema_errors(
                                minimal, schema, self.loader.openapi_path), [])
                            self.assertTrue(self.loader.schema_errors(
                                raw_text, schema, self.loader.openapi_path),
                                "application/json schema expects the decoded object")
                            rich = {
                                "error": {"code": code, "message": "request failed", "retryable": False,
                                          "retry_after_ms": None, "details": {}},
                                "meta": {"request_id": "req_contract", "next_cursor": None, "has_more": False},
                            }
                            self.assertEqual(self.loader.schema_errors(
                                rich, schema, self.loader.openapi_path), [])

    def test_list_timestamps_accept_rfc3339_offsets_and_fractional_seconds(self):
        # Default pgx TimestamptzCodec has no ScanLocation override and binary
        # decoding uses time.Unix. A UTC database session does not force the
        # resulting Go time.Time to marshal with Z in a non-UTC process.
        fields = {
            "OperatorConversationPageEnvelope": ("created_at", "last_event_at"),
            "OperatorMessagePageEnvelope": ("created_at",),
            "OperatorNotificationPageEnvelope": ("created_at", "read_at"),
        }
        timestamps = (
            STAMP,
            "2026-11-04T08:00:00+08:00",
            "2026-11-03T18:30:00-05:30",
            "2026-11-04T08:00:00.123456789+08:00",
            "2026-11-03T18:30:00.123456-05:30",
        )
        for component, names in fields.items():
            for field in names:
                for timestamp in timestamps:
                    with self.subTest(component=component, field=field, timestamp=timestamp):
                        sample = copy.deepcopy(RESPONSES[component])
                        sample["data"]["items"][0][field] = timestamp
                        self.accepted(component, sample)
                for invalid in ("2026-11-04T08:00:00", "2026-11-04T08:00:00+0800", "not-a-timestamp"):
                    with self.subTest(component=component, field=field, invalid=invalid):
                        sample = copy.deepcopy(RESPONSES[component])
                        sample["data"]["items"][0][field] = invalid
                        self.rejected(component, sample)

    def test_common_utc_datetime_retains_its_z_only_contract(self):
        # B7's local compatibility must not relax unrelated public timestamps.
        self.accepted("UTCDateTime", STAMP)
        for offset in ("2026-11-04T08:00:00+08:00", "2026-11-03T18:30:00-05:30"):
            with self.subTest(offset=offset):
                self.rejected("UTCDateTime", offset)

    def test_request_keys_differ_from_inbox_event_dedup_and_notification_cas(self):
        for method, path, request, _, _, _ in OPERATIONS:
            with self.subTest(path=path):
                params = self.parameters(method, path)
                key = params.get(("header", "Idempotency-Key"))
                if request in ("OperatorAgentMessageRequest", "OperatorQQDraftRequest"):
                    self.assertIsNotNone(key)
                    self.assertTrue(key["required"])
                    for value in ("k", "k" * 128):
                        self.assertEqual(self.parameter_errors(key, value), [])
                    for value in ("", " ", "k" * 129):
                        self.assertTrue(self.parameter_errors(key, value), value)
                elif request == "OperatorQQInboxEventRequest":
                    self.assertIsNotNone(key)
                    self.assertTrue(key["required"])
                    # ingestEvent checks presence only, not taskCheckIdempotencyKey.
                    for value in (" ", "k" * 129):
                        self.assertEqual(self.parameter_errors(key, value), [])
                    self.assertTrue(self.parameter_errors(key, ""))
                else:
                    self.assertIsNone(key, "GET/CAS does not require a durable request key")
                if "{id}" in path:
                    resource = params[("path", "id")]
                    self.assertTrue(resource["required"])
                    self.assertEqual(self.parameter_errors(resource, "external-thread:1"), [])
                    for bad in ("", "one/two"):
                        self.assertTrue(self.parameter_errors(resource, bad), bad)

    def test_offset_pagination_filters_and_default_limit(self):
        for path in ("/v1/operator/conversations", "/v1/operator/conversations/{id}/messages",
                     "/v1/operator/notifications"):
            with self.subTest(path=path):
                params = self.parameters("get", path)
                limit = params[("query", "limit")]
                cursor = params[("query", "cursor")]
                self.assertFalse(limit.get("required", False))
                self.assertFalse(cursor.get("required", False))
                limit_schema = self.resolved_schema(limit["schema"])
                self.assertEqual(limit_schema.get("default"), 50)
                for good in (1, 100):
                    self.assertEqual(self.parameter_errors(limit, good), [])
                for bad in (0, 101, 1.5):
                    self.assertTrue(self.parameter_errors(limit, bad))
                # The wire query is text; an integer parameter delegates parsing
                # to the HTTP client, while a string schema must allow first page.
                numeric = self.resolved_schema(cursor["schema"]).get("type") == "integer"
                for value in ((0, 1000000) if numeric else ("", "0", "1000000")):
                    self.assertEqual(self.parameter_errors(cursor, value), [])
                for value in ((-1, 1000001) if numeric else ("-1", "1000001", "opaque-token")):
                    self.assertTrue(self.parameter_errors(cursor, value), value)
                expected = {("query", "limit"), ("query", "cursor")}
                if path == "/v1/operator/conversations":
                    expected |= {("query", x) for x in ("account_id", "conversation_kind", "conversation_id")}
                elif path == "/v1/operator/notifications":
                    expected.add(("query", "account_id"))
                self.assertEqual({p for p in params if p[0] == "query"}, expected)
                for location, name in expected - {("query", "limit"), ("query", "cursor")}:
                    self.assertFalse(params[(location, name)].get("required", False))
                    # SQL equality filters do not validate QQId or kind enums.
                    self.assertEqual(self.parameter_errors(params[(location, name)], "no-such-value"), [])

    def test_independent_request_samples_and_unknown_fields(self):
        for component, sample in REQUESTS.items():
            with self.subTest(component=component):
                self.accepted(component, sample)
                self.rejected(component, dict(sample, unexpected="must fail decoding"))
                for field in sample:
                    if field == "read":  # Go bool zero value: omitted/null means false.
                        continue
                    for replacement in (None, []):
                        mutated = dict(sample, **{field: replacement})
                        self.rejected(component, mutated)
                    missing = dict(sample)
                    del missing[field]
                    self.rejected(component, missing)

    def test_body_lengths_and_non_decimal_external_ids(self):
        for component in ("OperatorAgentMessageRequest", "OperatorQQDraftRequest", "OperatorQQInboxEventRequest"):
            with self.subTest(component=component):
                for body in (" ", "x" * 8192, "汉字🙂"):
                    self.accepted(component, dict(REQUESTS[component], body=body))
                for body in ("", "x" * 8193, 123):
                    self.rejected(component, dict(REQUESTS[component], body=body))
                if "conversation_id" in REQUESTS[component]:
                    self.accepted(component, dict(REQUESTS[component], conversation_id="external-room:alpha"))
                    for kind in ("group", "private"):
                        self.accepted(component, dict(REQUESTS[component], conversation_kind=kind))
                    self.rejected(component, dict(REQUESTS[component], conversation_kind="agent"))
                schema, source = self.loader.resolve_component(component)
                body_schema = self.resolved_schema(schema["properties"]["body"], source)
                descriptions = schema.get("description", "") + body_schema.get("description", "")
                self.assertRegex(descriptions.lower(), r"(?:utf.?8|bytes?|字节)",
                                 "character length alone cannot enforce handler byte limits")

    def test_notification_read_defaults_and_numeric_cas(self):
        component = "OperatorNotificationReadRequest"
        for sample in ({"expected_version": 1}, {"expected_version": 1, "read": None},
                       {"expected_version": LARGE_INTEGER, "read": False},
                       {"expected_version": INT64_MAX, "read": True}):
            self.accepted(component, sample)
        for version in (0, -1, "1", 1.5, True, INT64_MAX + 1):
            self.rejected(component, {"expected_version": version})
        for read in ("false", 0, {}):
            self.rejected(component, {"expected_version": 1, "read": read})
        for version in (0, 2, "1", None):
            self.rejected("OperatorQQDraftRequest", dict(REQUESTS["OperatorQQDraftRequest"], expected_version=version))

    def test_independent_success_samples_have_required_nonnullable_fields(self):
        nullable = {
            "OperatorConversationPageEnvelope": {"account_id"},
            "OperatorNotificationPageEnvelope": {"account_id", "conversation_id", "read_at"},
            # jsonb NOT NULL excludes SQL NULL, not the JSON value null.
            "OperatorQQDraftDetailEnvelope": {"evidence"},
        }
        for component, sample in RESPONSES.items():
            with self.subTest(component=component):
                self.accepted(component, sample)
                self.rejected(component, {})
                data = sample["data"]
                target = data["items"][0] if "items" in data else data
                for field in target:
                    missing = copy.deepcopy(sample)
                    row = missing["data"]["items"][0] if "items" in data else missing["data"]
                    del row[field]
                    self.rejected(component, missing)
                    if field not in nullable.get(component, set()):
                        null = copy.deepcopy(sample)
                        row = null["data"]["items"][0] if "items" in data else null["data"]
                        row[field] = None
                        self.rejected(component, null)

    def test_page_cursor_is_in_data_empty_at_end_and_never_null(self):
        for component in ("OperatorConversationPageEnvelope", "OperatorMessagePageEnvelope", "OperatorNotificationPageEnvelope"):
            with self.subTest(component=component):
                for cursor in ("", "50", "1000100"):
                    self.accepted(component, {"data": {"items": [], "next_cursor": cursor}})
                for value in (None, 50):
                    self.rejected(component, {"data": {"items": [], "next_cursor": value}})
                self.rejected(component, {"data": {"items": []}})
                self.rejected(component, {"data": {"items": []}, "meta": {"next_cursor": "50"}})

    def test_persisted_message_states_and_digest(self):
        component = "OperatorMessagePageEnvelope"
        for source, direction, state in (("agent", "inbound", "received"), ("qq", "inbound", "received"),
                                         ("qq", "outbound_draft", "draft"), ("qq", "outbound_draft", "blocked")):
            self.accepted(component, page(dict(MESSAGE, source=source, direction=direction, delivery_state=state)))
        for field, value in (("source", "model"), ("direction", "outbound"), ("delivery_state", "sent"),
                             ("body_digest", "A" * 64), ("body_digest", "a" * 63)):
            self.rejected(component, page(dict(MESSAGE, **{field: value})))
        qq = dict(CONVERSATION, kind="qq", account_id="acc_contract", conversation_kind="group")
        self.accepted("OperatorConversationPageEnvelope", page(qq))
        self.accepted("OperatorNotificationPageEnvelope", page(dict(NOTIFICATION, account_id="acc_contract",
                      conversation_id="conv_contract", read_at=STAMP)))

    def test_legacy_json_integers_preserve_bigint_and_reject_string_coercion(self):
        for component, fields in (("OperatorConversationPageEnvelope", ("version", "unread_count")),
                                  ("OperatorMessagePageEnvelope", ("seq",)),
                                  ("OperatorNotificationPageEnvelope", ("version",)),
                                  ("OperatorQQDraftEnvelope", ("version", "rules_version")),
                                  ("OperatorQQDraftDetailEnvelope", ("version", "rules_version")),
                                  ("OperatorNotificationReadEnvelope", ("version",))):
            for field in fields:
                with self.subTest(component=component, field=field):
                    for value, valid in ((LARGE_INTEGER, True), ("1", False), (True, False),
                                         (1.5, False), (-1, False), (0, field in ("unread_count", "rules_version"))):
                        sample = copy.deepcopy(RESPONSES[component])
                        data = sample["data"]
                        target = data["items"][0] if "items" in data else data
                        target[field] = value
                        (self.accepted if valid else self.rejected)(component, sample)

    def test_draft_decisions_nested_integer_evidence_and_absent_permit(self):
        for component in ("OperatorQQDraftEnvelope", "OperatorQQDraftDetailEnvelope"):
            for decision in ("draft", "blocked", "unsupported"):
                sample = copy.deepcopy(RESPONSES[component])
                sample["data"]["decision"] = decision
                sample["data"]["evidence"]["hits"] = [
                    {"rule_id": "rule_contract", "rule_version": LARGE_INTEGER, "byte_start": 0, "byte_end": 3}
                ]
                self.accepted(component, sample)
        for component in ("OperatorAgentMessageEnvelope", "OperatorQQDraftEnvelope", "OperatorQQDraftDetailEnvelope"):
            for permit in ("issued", "consumed", {"id": "permit_contract"}):
                sample = copy.deepcopy(RESPONSES[component])
                sample["data"]["send_permit"] = permit
                self.rejected(component, sample)
        for component in ("OperatorNotificationPageEnvelope", "OperatorNotificationReadEnvelope"):
            sample = copy.deepcopy(RESPONSES[component])
            data = sample["data"]
            (data["items"][0] if "items" in data else data)["delivery_state"] = "sent"
            self.rejected(component, sample)

    def test_get_draft_preserves_arbitrary_historical_json_evidence(self):
        # 0016 has no evidence JSON-type CHECK; getDraft emits RawMessage.
        # Current createDraft builds an object, but historical GET values can
        # be null, arrays or scalars. Presence of the evidence key still matters.
        for evidence in (None, [], [None, {"historical": True}], "historical text",
                         False, 0, LARGE_INTEGER, 1.25, {"legacy": ["opaque", 7]}):
            with self.subTest(evidence=evidence):
                sample = copy.deepcopy(RESPONSES["OperatorQQDraftDetailEnvelope"])
                sample["data"]["evidence"] = evidence
                self.accepted("OperatorQQDraftDetailEnvelope", sample)
        for evidence in (None, [], "historical text", False, 0):
            sample = copy.deepcopy(RESPONSES["OperatorQQDraftEnvelope"])
            sample["data"]["evidence"] = evidence
            self.rejected("OperatorQQDraftEnvelope", sample)

    def test_inbox_replay_is_boolean_and_distinct_from_agent_draft_receipts(self):
        for replay in (False, True):
            self.accepted("OperatorQQInboxEventEnvelope", {"data": dict(INBOX_RECEIPT, idempotent_replay=replay)})
        for replay in (None, "true", 1):
            self.rejected("OperatorQQInboxEventEnvelope", {"data": dict(INBOX_RECEIPT, idempotent_replay=replay)})
        for component in ("OperatorAgentMessageEnvelope", "OperatorQQDraftEnvelope"):
            self.rejected(component, RESPONSES["OperatorQQInboxEventEnvelope"])
        self.rejected("OperatorQQInboxEventEnvelope", RESPONSES["OperatorAgentMessageEnvelope"])
        detail = copy.deepcopy(RESPONSES["OperatorQQDraftDetailEnvelope"])
        del detail["data"]["account_id"]
        self.rejected("OperatorQQDraftDetailEnvelope", detail)

    def test_minimal_decode_auth_errors_and_rich_task_errors(self):
        component = "OperatorConversationErrorEnvelope"
        for code in ("UNAUTHENTICATED", "ACTION_FORBIDDEN", "METHOD_NOT_ALLOWED",
                     "UNSUPPORTED_MEDIA_TYPE", "VALIDATION_ERROR", "STATE_STORE_UNAVAILABLE"):
            self.accepted(component, {"error": {"code": code}})
        rich = {
            "error": {"code": "VERSION_CONFLICT", "message": "request failed", "retryable": False,
                      "retry_after_ms": None, "details": {}},
            "meta": {"request_id": "req_contract", "next_cursor": None, "has_more": False},
        }
        self.accepted(component, rich)
        for invalid in ({}, {"error": {}}, {"error": {"code": None}}, {"data": {"accepted": True}}):
            self.rejected(component, invalid)


if __name__ == "__main__":
    unittest.main()
