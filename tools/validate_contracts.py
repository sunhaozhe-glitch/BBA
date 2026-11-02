#!/usr/bin/env python3
"""Strict, local-only validation for the P0-01 contract bundle.

Validation happens in this order and fails closed:

1. The local reference gate (``refgate``) runs over every OpenAPI/schema file.
2. Schema Objects are checked against the OAS 3.0 subset used by this project
   (explicit object strictness, no exotic JSON-Schema keywords, boolean-only
   ``nullable``), then every named component is compiled with jsonschema.
3. The OpenAPI 3.0.3 document structure, operation ids and plugin routes are
   checked, including canonical-only NapCat plugin paths.
4. Every fixture record in ``contracts/examples/*.json`` must be accepted when
   ``valid: true`` and rejected when ``valid: false``. Semantic edges such as
   "pending/unknown is not success" are applied in addition to JSON Schema.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path, PurePosixPath
from typing import Any, Iterable, Iterator, Mapping, Sequence

_REPO_ROOT = Path(__file__).resolve().parents[1]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

from jsonschema import Draft4Validator, FormatChecker, ValidationError
from jsonschema import validators
from referencing.exceptions import Unresolvable
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT4

from tools.refgate import (
    GateIssue,
    RefGate,
    RefGateError,
    default_spec_files,
    load_document,
)

# ---------------------------------------------------------------------------
# Fixed contract surface (brief §"Shared component names")
# ---------------------------------------------------------------------------

EXPECTED_COMPONENTS: dict[str, tuple[str, ...]] = {
    "common.json": (
        "Id",
        "QQId",
        "DecimalSequence",
        "Digest",
        "UTCDateTime",
        "RequestMeta",
        "Error",
        "ErrorEnvelope",
        "Target",
        "Pagination",
        "Receipt",
        "Account",
        "AccountIdentityStatus",
        "AccountIdentitySource",
        "AccountIdentity",
        "AccountIdentityPage",
        "AccountPage",
        "CapabilityAction",
        "Capability",
        "CapabilityPage",
        "AccountControlRequest",
        "TaskControlRequest",
        "VersionRequest",
        "ReconcileRequest",
        "HeartbeatRequest",
        "HeartbeatReceipt",
    ),
    "tasks.json": (
        "TaskState",
        "ExecutionMode",
        "Goal",
        "FixedCandidate",
        "CandidateSource",
        "TaskLimits",
        "CreateTaskRequest",
        "Task",
        "Subtask",
        "SubtaskPage",
        "Result",
        "ResultPage",
        "TaskEvent",
        "TaskEventPage",
        "TaskEnvelope",
        "ReceiptEnvelope",
        "AccountPageEnvelope",
        "CapabilityPageEnvelope",
        "SubtaskPageEnvelope",
        "ResultPageEnvelope",
        "TaskEventPageEnvelope",
        "HeartbeatEnvelope",
    ),
    "bridge.json": (
        "Action",
        "EffectCertainty",
        "Disposition",
        "ExecutionState",
        "JoinParams",
        "SearchParams",
        "MembershipParams",
        "JoinObservationParams",
        "SendTextParams",
        "ExecutionRequest",
        "Operation",
        "OperationEnvelope",
        "Observation",
        "ObservationReceipt",
        "ObservationEnvelope",
        "ObservationQueryRequest",
        "ObservationQueryResult",
        "ObservationQueryEnvelope",
        "DispatchPermitRequest",
        "DispatchPermit",
        "DispatchPermitEnvelope",
        "ChatPermitRequest",
        "ChatPermit",
        "ChatPermitEnvelope",
    ),
    "models.json": (
        "PlanProposalRequest",
        "PlanApplyRequest",
        "PlanStep",
        "PlanProposal",
        "IntentDraft",
        "Summary",
        "ModelRequest",
        "ModelResponse",
        "ModelEnvelope",
        "GroupSummaryIngestRequest",
        "GroupSummaryIngestResponse",
        "GroupSummaryIngestEnvelope",
        "GroupContextTopic",
        "GroupContext",
        "GroupContextEnvelope",
        "PlanProposalApprovalRequest",
        "PlanProposalApprovalReceipt",
        "PlanProposalApprovalEnvelope",
        "PlanProposalView",
        "PlanProposalBasis",
        "PlanProposalEnvelope",
        "TaskGraphAnchor",
        "TaskGraphNode",
        "TaskGraphFact",
        "TaskGraphEdge",
        "TaskGraph",
        "TaskGraphEnvelope",
    ),
    "plugin.json": (
        "PluginSearchRequest",
        "PluginGroup",
        "PluginSearchResponse",
        "PluginJoinRequest",
        "PluginJoinResponse",
    ),
}

CANONICAL_PLUGIN_PATHS = {
    "/api/Plugin/ext/napcat-plugin-cleaner/search-group",
    "/api/Plugin/ext/napcat-plugin-cleaner/join-group",
}
LEGACY_PLUGIN_MARKER = "dxc-join-api"

HTTP_METHODS = {"get", "put", "post", "delete", "options", "head", "patch", "trace"}

ALLOWED_SCHEMA_KEYS = {
    "$ref",
    "title",
    "description",
    "type",
    "format",
    "default",
    "enum",
    "multipleOf",
    "maximum",
    "exclusiveMaximum",
    "minimum",
    "exclusiveMinimum",
    "maxLength",
    "minLength",
    "pattern",
    "maxItems",
    "minItems",
    "uniqueItems",
    "maxProperties",
    "minProperties",
    "required",
    "properties",
    "additionalProperties",
    "items",
    "allOf",
    "oneOf",
    "anyOf",
    "not",
    "nullable",
    "discriminator",
    "readOnly",
    "writeOnly",
    "xml",
    "externalDocs",
    "example",
    "deprecated",
}

SCHEMA_TYPES = {"object", "array", "string", "number", "integer", "boolean"}

_UTC_DATETIME_RE = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$"
)
_COMPONENT_NAME_RE = re.compile(r"^[A-Za-z][A-Za-z0-9]*$")


# ---------------------------------------------------------------------------
# OAS 3.0 -> JSON Schema Draft 4 bridge
# ---------------------------------------------------------------------------

_DRAFT4_TYPE = Draft4Validator.VALIDATORS["type"]
_DRAFT4_ENUM = Draft4Validator.VALIDATORS["enum"]


def _oas3_type(validator: Any, types: Any, instance: Any, schema: Any) -> Iterator[ValidationError]:
    """OAS 3.0 ``nullable``: null is valid only when nullable is true."""

    if instance is None:
        if schema.get("nullable") is True:
            return
        expected = types if isinstance(types, str) else ", ".join(map(str, types))
        yield ValidationError(f"null is not allowed unless nullable: true (type={expected})")
        return
    yield from _DRAFT4_TYPE(validator, types, instance, schema)


def _oas3_enum(validator: Any, enum: Any, instance: Any, schema: Any) -> Iterator[ValidationError]:
    if instance is None and schema.get("nullable") is True:
        return
    yield from _DRAFT4_ENUM(validator, enum, instance, schema)


def _oas3_nullable(validator: Any, value: Any, instance: Any, schema: Any) -> Iterator[ValidationError]:
    if not isinstance(value, bool):
        yield ValidationError("nullable must be a boolean")


def _oas3_ignored(validator: Any, value: Any, instance: Any, schema: Any) -> Iterator[ValidationError]:
    return
    yield  # pragma: no cover - keep this a generator for jsonschema


ContractValidator = validators.extend(
    Draft4Validator,
    {
        "type": _oas3_type,
        "enum": _oas3_enum,
        "discriminator": _oas3_ignored,
        "example": _oas3_ignored,
        "xml": _oas3_ignored,
        "externalDocs": _oas3_ignored,
    },
)

FORMAT_CHECKER = FormatChecker()


@FORMAT_CHECKER.checks("date-time", raises=ValueError)
def _strict_utc_datetime(value: Any) -> bool:
    if not isinstance(value, str):
        return True
    if not _UTC_DATETIME_RE.fullmatch(value):
        raise ValueError("date-time must be RFC3339 UTC with a trailing Z")
    try:
        datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as exc:
        raise ValueError(f"invalid calendar date-time: {exc}") from exc
    return True


def _local_registry(documents: Mapping[str, Any]) -> Registry[Any]:
    resources = [
        (uri, Resource.from_contents(document, default_specification=DRAFT4))
        for uri, document in documents.items()
    ]
    return Registry().with_resources(resources)

# ---------------------------------------------------------------------------
# Reports
# ---------------------------------------------------------------------------


@dataclass
class ValidationReport:
    errors: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    stats: dict[str, int] = field(default_factory=dict)

    @property
    def ok(self) -> bool:
        return not self.errors


@dataclass
class FixtureRecord:
    source: Path
    index: int
    schema_ref: str
    component: str
    value: Any
    valid: bool
    description: str


# ---------------------------------------------------------------------------
# Loader
# ---------------------------------------------------------------------------


class ContractLoader:
    def __init__(self, contracts_root: Path | str):
        self.root = Path(contracts_root).resolve(strict=True)
        self.schema_dir = self.root / "schemas"
        self.openapi_path = self.root / "openapi.yaml"
        self.gate = RefGate(self.root)
        self.store: dict[str, Any] = {}
        self.documents: dict[Path, Any] = {}
        for path in self._all_contract_documents():
            real = path.resolve(strict=True)
            document = self.gate.load(real)
            self.documents[real] = document
            self.store[real.as_uri()] = document
        self.registry = _local_registry(self.store)
        self._components: dict[str, tuple[Any, Path]] | None = None

    def _all_contract_documents(self) -> list[Path]:
        paths: list[Path] = []
        if self.openapi_path.is_file():
            paths.append(self.openapi_path)
        if self.schema_dir.is_dir():
            paths.extend(sorted(self.schema_dir.rglob("*.json")))
        return paths

    def require_openapi(self) -> None:
        if not self.openapi_path.is_file():
            raise RefGateError(f"missing OpenAPI root: {self.openapi_path}")

    def schema_files(self) -> list[Path]:
        if not self.schema_dir.is_dir():
            return []
        return sorted(self.schema_dir.glob("*.json"))

    def components(self) -> dict[str, tuple[Any, Path]]:
        """Return component name -> (schema, source file), rejecting clashes."""

        if self._components is not None:
            return self._components

        result: dict[str, tuple[Any, Path]] = {}
        conflicts: list[str] = []
        for path in self.schema_files():
            document = self.gate.load(path)
            if not isinstance(document, dict):
                raise RefGateError(f"{path}: schema file must be a JSON object")
            for key in ("$schema", "$id"):
                if key in document:
                    raise RefGateError(
                        f"{path}: plain component dictionary must not declare {key}"
                    )
            for name, schema in document.items():
                if not isinstance(name, str) or not _COMPONENT_NAME_RE.fullmatch(name):
                    raise RefGateError(f"{path}: invalid component name {name!r}")
                if name in result:
                    conflicts.append(
                        f"duplicate component {name!r} in {path} and {result[name][1]}"
                    )
                    continue
                result[name] = (schema, path)
        if conflicts:
            raise RefGateError("; ".join(conflicts))
        self._components = result
        return result

    def resolve_component(self, component: str) -> tuple[Any, Path]:
        components = self.components()
        if component not in components:
            raise RefGateError(f"unknown component {component!r}")
        return components[component]

    # -- validation ------------------------------------------------------
    def schema_errors(self, value: Any, schema: Any, base_file: Path) -> list[str]:
        try:
            validator = ContractValidator(
                schema,
                registry=self.registry,
                _resolver=self.registry.resolver(base_uri=base_file.as_uri()),
                format_checker=FORMAT_CHECKER,
            )
            errors = sorted(
                validator.iter_errors(value),
                key=lambda error: tuple(str(part) for part in error.absolute_path),
            )
        except (RefGateError, Unresolvable) as exc:
            return [str(exc)]
        rendered: list[str] = []
        for error in errors:
            path = "/".join(str(part) for part in error.absolute_path)
            rendered.append(f"{path or '<root>'}: {error.message}")
        return rendered

    def component_errors(self, component: str, value: Any) -> list[str]:
        schema, source = self.resolve_component(component)
        return self.schema_errors(value, schema, source)


# ---------------------------------------------------------------------------
# Schema definition checks
# ---------------------------------------------------------------------------


def _resolve_schema_node_issues(loader: ContractLoader, node: Any, source: Path) -> list[str]:
    # Ref resolution itself is already covered by RefGate; this is a lights-out
    # assertion so no consumer can compile a spec with an unresolved pointer.
    issues: list[str] = []

    def walk(value: Any, pointer: str) -> None:
        if isinstance(value, dict):
            ref = value.get("$ref")
            if isinstance(ref, str):
                try:
                    loader.gate.resolve(ref, source)
                except (RefGateError, OSError) as exc:
                    issues.append(f"{pointer}: unresolved $ref {ref!r}: {exc}")
                return
            for key, child in value.items():
                walk(child, f"{pointer}/{key}")
        elif isinstance(value, list):
            for index, child in enumerate(value):
                walk(child, f"{pointer}/{index}")

    walk(node, "#")
    return issues


def schema_definition_errors(
    loader: ContractLoader,
    schema: Any,
    source: Path,
    pointer: str = "#",
    *,
    component_name: str | None = None,
) -> list[str]:
    errors: list[str] = []

    def fail(where: str, message: str) -> None:
        errors.append(f"{where}: {message}")

    def expect_schema(child: Any, where: str) -> None:
        if isinstance(child, bool):
            fail(where, "OAS 3.0 does not allow boolean schemas")
            return
        if not isinstance(child, dict):
            fail(where, "schema must be an object")
            return
        walk(child, where)

    def walk(node: Any, where: str) -> None:
        if isinstance(node, bool):
            fail(where, "OAS 3.0 does not allow boolean schemas")
            return
        if not isinstance(node, dict):
            fail(where, "schema must be an object")
            return

        unknown = {
            key
            for key in node
            if key not in ALLOWED_SCHEMA_KEYS and not key.startswith("x-")
        }
        for key in sorted(unknown):
            fail(where, f"unsupported/unknown schema keyword {key!r}")

        if "$ref" in node:
            if set(node) != {"$ref"}:
                fail(
                    where,
                    "$ref must be the only member; OAS 3.0 ignores siblings "
                    f"(found {sorted(set(node) - {'$ref'})})",
                )
            elif not isinstance(node["$ref"], str):
                fail(where, "$ref must be a string")
            return

        node_type = node.get("type")
        if node_type is not None:
            if isinstance(node_type, list):
                fail(where, "OAS 3.0 type must be a single string, not an array")
            elif not isinstance(node_type, str) or node_type not in SCHEMA_TYPES:
                fail(where, f"invalid OAS 3.0 type {node_type!r}")

        nullable = node.get("nullable")
        if nullable is not None:
            if not isinstance(nullable, bool):
                fail(where, "nullable must be a boolean")
            elif nullable and "type" not in node:
                fail(where, "nullable requires an explicit type in OAS 3.0")

        for flag in ("readOnly", "writeOnly", "deprecated", "uniqueItems"):
            if flag in node and not isinstance(node[flag], bool):
                fail(where, f"{flag} must be a boolean")

        if "properties" in node:
            if not isinstance(node["properties"], dict):
                fail(where, "properties must be an object")
            else:
                for name, child in node["properties"].items():
                    if not isinstance(name, str) or not name:
                        fail(where, "property names must be non-empty strings")
                        continue
                    expect_schema(child, f"{where}/properties/{name}")

        if "required" in node:
            required = node["required"]
            if not isinstance(required, list) or not required:
                fail(where, "required must be a non-empty array of property names")
            else:
                seen: set[str] = set()
                properties = node.get("properties")
                for name in required:
                    if not isinstance(name, str) or not name:
                        fail(where, "required entries must be non-empty strings")
                        continue
                    if name in seen:
                        fail(where, f"required contains duplicate {name!r}")
                    seen.add(name)
                    if isinstance(properties, dict) and name not in properties:
                        fail(where, f"required property {name!r} is not declared in properties")

        if node_type == "object":
            if "additionalProperties" not in node:
                fail(
                    where,
                    "object schema must explicitly declare additionalProperties "
                    "(false, or a bounded map value schema)",
                )
            else:
                additional = node["additionalProperties"]
                if additional is True:
                    # Only an explicitly bounded opaque metadata map may accept
                    # arbitrary keys. Request components are checked below and
                    # may never opt into this.
                    max_properties = node.get("maxProperties")
                    if (
                        isinstance(max_properties, bool)
                        or not isinstance(max_properties, int)
                        or not 1 <= max_properties <= 256
                    ):
                        fail(
                            where,
                            "additionalProperties: true is allowed only with a "
                            "bounded maxProperties (1..256)",
                        )
                elif additional is False:
                    pass
                elif isinstance(additional, dict):
                    additional_type = additional.get("type")
                    if additional_type == "object":
                        fail(
                            where,
                            "additionalProperties map values must be bounded scalars, "
                            "not nested objects",
                        )
                    expect_schema(additional, f"{where}/additionalProperties")
                else:
                    fail(where, "additionalProperties must be false or a schema")

        if "items" in node:
            if isinstance(node["items"], list):
                fail(where, "OAS 3.0 items must be a single schema object")
            else:
                expect_schema(node["items"], f"{where}/items")

        for combiner in ("allOf", "oneOf", "anyOf"):
            if combiner not in node:
                continue
            branches = node[combiner]
            if not isinstance(branches, list) or not branches:
                fail(where, f"{combiner} must be a non-empty array")
                continue
            for index, branch in enumerate(branches):
                expect_schema(branch, f"{where}/{combiner}/{index}")

        if "not" in node:
            expect_schema(node["not"], f"{where}/not")

        if "enum" in node:
            enum = node["enum"]
            if not isinstance(enum, list) or not enum:
                fail(where, "enum must be a non-empty array")
            else:
                encoded = [json.dumps(item, sort_keys=True) for item in enum]
                if len(set(encoded)) != len(encoded):
                    fail(where, "enum values must be unique")

        if "discriminator" in node:
            discriminator = node["discriminator"]
            if not isinstance(discriminator, dict):
                fail(where, "discriminator must be an object")
            else:
                property_name = discriminator.get("propertyName")
                if not isinstance(property_name, str) or not property_name:
                    fail(where, "discriminator.propertyName must be a non-empty string")
                mapping = discriminator.get("mapping", {})
                if not isinstance(mapping, dict):
                    fail(where, "discriminator.mapping must be an object")

        # Draft 4 meta-schema type checks for numeric/string/array constraints.
        try:
            ContractValidator.check_schema(node)
        except Exception as exc:  # jsonschema raises SchemaError
            fail(where, f"invalid Schema Object: {exc}")

    walk(schema, pointer)

    if component_name and component_name.endswith("Request"):
        if isinstance(schema, dict) and schema.get("type") == "object":
            if schema.get("additionalProperties") is not False:
                errors.append(
                    f"{pointer}: request component {component_name} must set "
                    "additionalProperties: false"
                )

    errors.extend(_resolve_schema_node_issues(loader, schema, source))
    return errors


# ---------------------------------------------------------------------------
# OpenAPI structural checks
# ---------------------------------------------------------------------------


def _validate_inline_schema(
    loader: ContractLoader,
    schema: Any,
    source: Path,
    where: str,
    errors: list[str],
) -> None:
    errors.extend(schema_definition_errors(loader, schema, source, pointer=where))


def validate_openapi(loader: ContractLoader) -> list[str]:
    errors: list[str] = []
    loader.require_openapi()
    source = loader.openapi_path
    document = loader.gate.load(source)
    if not isinstance(document, dict):
        return [f"{source}: OpenAPI root must be an object"]
    if document.get("openapi") != "3.0.3":
        errors.append(f"{source}: openapi must be exactly 3.0.3")

    info = document.get("info")
    if not isinstance(info, dict):
        errors.append(f"{source}: info is required")
    else:
        for key in ("title", "version"):
            if not isinstance(info.get(key), str) or not info.get(key):
                errors.append(f"{source}: info.{key} must be a non-empty string")

    paths = document.get("paths")
    if not isinstance(paths, dict) or not paths:
        errors.append(f"{source}: paths must be a non-empty object")
        paths = {}

    operation_ids: dict[str, str] = {}
    for raw_path, path_item in paths.items():
        if not isinstance(raw_path, str) or not raw_path.startswith("/"):
            errors.append(f"{source}: path key {raw_path!r} must start with '/'")
            continue
        if not isinstance(path_item, dict):
            errors.append(f"{source}: path {raw_path} must be an object")
            continue
        for method, operation in path_item.items():
            if method not in HTTP_METHODS:
                continue
            where = f"{raw_path} {method.upper()}"
            if not isinstance(operation, dict):
                errors.append(f"{source}: {where} operation must be an object")
                continue
            operation_id = operation.get("operationId")
            if not isinstance(operation_id, str) or not operation_id:
                errors.append(f"{source}: {where} must declare operationId")
            elif operation_id in operation_ids:
                errors.append(
                    f"{source}: duplicate operationId {operation_id!r} "
                    f"({operation_ids[operation_id]} and {where})"
                )
            else:
                operation_ids[operation_id] = where

            responses = operation.get("responses")
            if not isinstance(responses, dict) or not responses:
                errors.append(f"{source}: {where} must declare responses")
            else:
                concrete = [code for code in responses if code != "default"]
                if not concrete:
                    errors.append(f"{source}: {where} must declare a concrete response")
                for code, response in responses.items():
                    if not isinstance(code, str):
                        errors.append(f"{source}: {where} response code must be a string")
                        continue
                    if code != "default" and not re.fullmatch(r"[1-5][0-9]{2}", code):
                        errors.append(f"{source}: {where} invalid response code {code!r}")
                    if isinstance(response, dict) and "$ref" not in response:
                        if not isinstance(response.get("description"), str):
                            errors.append(
                                f"{source}: {where} response {code} needs a description"
                            )

            request_body = operation.get("requestBody")
            if request_body is not None:
                _validate_body_or_response(
                    loader, request_body, source, f"{where} requestBody", errors
                )
            for code, response in responses.items():
                if isinstance(response, dict):
                    _validate_body_or_response(
                        loader, response, source, f"{where} response {code}", errors
                    )

            parameters = operation.get("parameters")
            if parameters is not None:
                if not isinstance(parameters, list):
                    errors.append(f"{source}: {where} parameters must be an array")
                else:
                    names: set[tuple[str, str]] = set()
                    for index, parameter in enumerate(parameters):
                        if not isinstance(parameter, dict):
                            errors.append(
                                f"{source}: {where} parameter {index} must be an object"
                            )
                            continue
                        if "$ref" in parameter:
                            continue
                        name = parameter.get("name")
                        location = parameter.get("in")
                        if not isinstance(name, str) or not name:
                            errors.append(
                                f"{source}: {where} parameter {index} needs a name"
                            )
                        if location not in {"query", "header", "path", "cookie"}:
                            errors.append(
                                f"{source}: {where} parameter {index} has invalid in"
                            )
                        if location == "path" and parameter.get("required") is not True:
                            errors.append(
                                f"{source}: {where} path parameter {name!r} must be required"
                            )
                        key = (str(location), str(name))
                        if name and key in names:
                            errors.append(
                                f"{source}: {where} duplicate parameter {name!r}"
                            )
                        names.add(key)
                        if "schema" in parameter:
                            _validate_inline_schema(
                                loader,
                                parameter["schema"],
                                source,
                                f"{where} parameter {name}",
                                errors,
                            )

            if operation_id in {"createTask", "getTask", "controlTask"}:
                requirements = operation.get("security", [])
                task_entry_enabled = any(
                    isinstance(requirement, dict) and "TaskEntryCredential" in requirement
                    for requirement in requirements
                )
                parameter_refs = {
                    parameter.get("$ref")
                    for parameter in operation.get("parameters", [])
                    if isinstance(parameter, dict)
                }
                required_task_entry_parameters = {
                    "#/components/parameters/TaskEntryAccount",
                    "#/components/parameters/TaskEntrySender",
                }
                if task_entry_enabled and not required_task_entry_parameters.issubset(parameter_refs):
                    errors.append(f"{source}: {where} TaskEntryCredential requires account and sender identity parameters")
            elif any(
                isinstance(requirement, dict) and "TaskEntryCredential" in requirement
                for requirement in operation.get("security", [])
            ):
                errors.append(f"{source}: {where} cannot use TaskEntryCredential")

    components = document.get("components")
    if components is not None and not isinstance(components, dict):
        errors.append(f"{source}: components must be an object")
        components = None
    if components:
        schemas = components.get("schemas")
        if schemas is not None and not isinstance(schemas, dict):
            errors.append(f"{source}: components.schemas must be an object")
        elif isinstance(schemas, dict):
            for name, schema in schemas.items():
                errors.extend(
                    schema_definition_errors(
                        loader,
                        schema,
                        source,
                        pointer=f"#/components/schemas/{name}",
                        component_name=name,
                    )
                )

    _validate_plugin_paths(paths, errors)
    return errors


def _validate_body_or_response(
    loader: ContractLoader,
    body: Any,
    source: Path,
    where: str,
    errors: list[str],
) -> None:
    if not isinstance(body, dict):
        errors.append(f"{source}: {where} must be an object")
        return
    if "$ref" in body:
        return
    content = body.get("content")
    if content is None:
        return
    if not isinstance(content, dict):
        errors.append(f"{source}: {where}.content must be an object")
        return
    for media_type, media in content.items():
        if not isinstance(media, dict):
            errors.append(f"{source}: {where}.content[{media_type!r}] must be an object")
            continue
        if "schema" in media:
            _validate_inline_schema(
                loader,
                media["schema"],
                source,
                f"{where} content {media_type}",
                errors,
            )


def _validate_plugin_paths(paths: Mapping[str, Any], errors: list[str]) -> None:
    for path in paths:
        lowered = str(path).lower()
        if LEGACY_PLUGIN_MARKER in lowered:
            errors.append(
                f"plugin route {path!r} is the legacy dxc-join-api route; "
                "only /api/Plugin/ext/napcat-plugin-cleaner/{search-group,join-group} is allowed"
            )
        if "napcat-plugin-cleaner" in lowered and path not in CANONICAL_PLUGIN_PATHS:
            errors.append(
                f"plugin route {path!r} is not one of the canonical plugin paths "
                f"{sorted(CANONICAL_PLUGIN_PATHS)}"
            )

    for canonical in sorted(CANONICAL_PLUGIN_PATHS):
        path_item = paths.get(canonical)
        if not isinstance(path_item, dict):
            errors.append(f"missing canonical plugin path {canonical!r}")
        elif "post" not in path_item:
            errors.append(f"canonical plugin path {canonical!r} must define POST")


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


def _fixture_schema(
    loader: ContractLoader, schema_ref: str
) -> tuple[str, Any, Path]:
    if not isinstance(schema_ref, str) or not schema_ref:
        raise RefGateError("schema_ref must be a non-empty string")
    if schema_ref.count("#") != 1:
        raise RefGateError("schema_ref must be '<file>#/<Component>'")
    path_part, fragment = schema_ref.split("#", 1)
    if path_part == "openapi.yaml":
        if not fragment.startswith("/components/schemas/"):
            raise RefGateError("OpenAPI fixture ref must target /components/schemas/<Name>")
        target, schema = loader.gate.resolve("#" + fragment, loader.openapi_path)
        component = fragment.rsplit("/", 1)[-1]
    else:
        pure = PurePosixPath(path_part)
        if pure.is_absolute() or len(pure.parts) != 1 or pure.suffix != ".json":
            raise RefGateError("schema_ref file must be a plain .json name in contracts/schemas")
        if not fragment.startswith("/") or fragment.count("/") != 1:
            raise RefGateError("schema_ref must target a top-level component: '#/<Name>'")
        component = fragment[1:]
        schema_files = loader.schema_files()
        if not schema_files:
            raise RefGateError("no schema files available")
        target, schema = loader.gate.resolve(
            f"{path_part}#{fragment}", schema_files[0]
        )
        if target.parent != loader.schema_dir.resolve(strict=True):
            raise RefGateError("schema_ref escaped contracts/schemas")
    if not isinstance(schema, dict):
        raise RefGateError(f"fixture target {schema_ref!r} is not a Schema Object")
    if component not in loader.components():
        raise RefGateError(f"fixture target {schema_ref!r} is not a named component")
    return component, schema, target


def load_fixtures(loader: ContractLoader, report: ValidationReport) -> list[FixtureRecord]:
    examples_dir = loader.root / "examples"
    if not examples_dir.is_dir():
        report.errors.append(f"{examples_dir}: fixture directory is missing")
        return []
    files = sorted(examples_dir.glob("*.json"))
    if not files:
        report.errors.append(f"{examples_dir}: no fixture JSON files found")
        return []

    records: list[FixtureRecord] = []
    for path in files:
        try:
            payload = load_document(path)
        except RefGateError as exc:
            report.errors.append(f"{path}: {exc}")
            continue
        if not isinstance(payload, list):
            report.errors.append(f"{path}: fixture file must be a JSON array of records")
            continue
        for index, raw in enumerate(payload):
            where = f"{path}: record {index}"
            if not isinstance(raw, dict):
                report.errors.append(f"{where}: record must be an object")
                continue
            missing = {"schema_ref", "valid", "value", "description"} - set(raw)
            extra = set(raw) - {"schema_ref", "valid", "value", "description"}
            if missing:
                report.errors.append(f"{where}: missing keys {sorted(missing)}")
                continue
            if extra:
                report.errors.append(f"{where}: unexpected keys {sorted(extra)}")
                continue
            if not isinstance(raw["valid"], bool):
                report.errors.append(f"{where}: valid must be a boolean")
                continue
            if not isinstance(raw["description"], str) or not raw["description"].strip():
                report.errors.append(f"{where}: description must be a non-empty string")
                continue
            try:
                component, _schema, _target = _fixture_schema(loader, raw["schema_ref"])
            except (RefGateError, OSError) as exc:
                report.errors.append(f"{where}: {exc}")
                continue
            records.append(
                FixtureRecord(
                    source=path,
                    index=index,
                    schema_ref=raw["schema_ref"],
                    component=component,
                    value=raw["value"],
                    valid=raw["valid"],
                    description=raw["description"],
                )
            )
    return records


def _walk_values(value: Any) -> Iterator[Any]:
    yield value
    if isinstance(value, dict):
        for child in value.values():
            yield from _walk_values(child)
    elif isinstance(value, list):
        for child in value:
            yield from _walk_values(child)


_NON_SUCCESS_MARKERS = {"success", "succeeded", "confirmed", "joined", "member", "applied"}
_NON_SUCCESS_DISPOSITIONS = {"pending", "submitted", "rejected", "failed_no_effect", "unknown"}
_NON_SUCCESS_CERTAINTIES = {
    "no_effect",
    "confirmed_submitted",
    "confirmed_rejected",
    "unknown",
    "pending",
    "submitted",
}


_BASIS_SUMMARY_FIELDS = (
    "summary_id",
    "summary_version",
    "evidence_kinds",
    "source_manifest_sha256",
)


def _plan_proposal_basis_errors(label: str, node: Any) -> list[str]:
    """The four summary reference fields are all-null or all-present."""

    if not isinstance(node, dict):
        return [f"{label}: basis entry must be an object"]
    present = [field for field in _BASIS_SUMMARY_FIELDS if node.get(field) is not None]
    if present and len(present) != len(_BASIS_SUMMARY_FIELDS):
        missing = sorted(set(_BASIS_SUMMARY_FIELDS) - set(present))
        return [
            f"{label}: {', '.join(_BASIS_SUMMARY_FIELDS)} are null together or "
            f"present together (missing {missing})"
        ]
    return []


def semantic_errors(component: str, value: Any) -> list[str]:
    """Business edges that a structural validator cannot encode in OAS 3.0."""

    errors: list[str] = []

    for node in _walk_values(value):
        if not isinstance(node, dict):
            continue

        certainty = node.get("effect_certainty")
        if isinstance(certainty, str) and certainty.lower() in _NON_SUCCESS_CERTAINTIES:
            for marker in _NON_SUCCESS_MARKERS:
                if node.get(marker) is True:
                    errors.append(
                        f"{component}: {marker}=true is claimed while "
                        f"effect_certainty={certainty!r}; pending/unknown/submitted "
                        "never means success"
                    )

        disposition = node.get("disposition")
        state = node.get("state")
        normalized = (
            disposition.lower()
            if isinstance(disposition, str)
            else state.lower()
            if isinstance(state, str)
            else ""
        )
        if normalized in _NON_SUCCESS_DISPOSITIONS:
            for marker in _NON_SUCCESS_MARKERS:
                if node.get(marker) is True:
                    errors.append(
                        f"{component}: {marker}=true is claimed while "
                        f"state/disposition={normalized!r}"
                    )

        outcome = node.get("outcome")
        if isinstance(outcome, str) and outcome.lower() in _NON_SUCCESS_DISPOSITIONS:
            if isinstance(certainty, str) and certainty.lower() == "confirmed":
                errors.append(
                    f"{component}: outcome={outcome!r} cannot have "
                    "effect_certainty='confirmed'"
                )

        if normalized == "joined" and node.get("snapshot_complete") is False:
            errors.append(
                f"{component}: joined cannot be claimed from an incomplete membership snapshot"
            )

        for raw in _walk_values(node):
            if isinstance(raw, str) and "/api/plugin/" in raw.lower():
                if LEGACY_PLUGIN_MARKER in raw.lower():
                    errors.append(
                        f"{component}: legacy plugin route {raw!r} must not be accepted"
                    )
                elif raw not in CANONICAL_PLUGIN_PATHS:
                    errors.append(
                        f"{component}: non-canonical plugin route {raw!r}"
                    )

    if component == "PlanProposalBasis" and isinstance(value, dict):
        errors.extend(_plan_proposal_basis_errors("PlanProposalBasis", value))

    if component == "PlanProposalView" and isinstance(value, dict):
        basis = value.get("basis")
        if not isinstance(basis, list):
            errors.append(
                "PlanProposalView: basis must be an array, empty when nothing was referenced"
            )
        else:
            for index, entry in enumerate(basis):
                errors.extend(
                    _plan_proposal_basis_errors(f"PlanProposalView: basis[{index}]", entry)
                )

    if component == "GroupSummaryIngestRequest" and isinstance(value, dict):
        summary = value.get("summary")
        evidence = value.get("topic_evidence")
        summary_id = value.get("summary_id")
        window_id = value.get("window_id")
        if isinstance(summary_id, str) and not re.fullmatch(r"[0-9a-f]{64}", summary_id):
            errors.append(
                "GroupSummaryIngestRequest: summary_id must be 64 lowercase hex characters"
            )
        if isinstance(window_id, str) and not re.fullmatch(r"[0-9a-f]{64}", window_id):
            errors.append(
                "GroupSummaryIngestRequest: window_id must be 64 lowercase hex characters"
            )
        if isinstance(summary, dict) and isinstance(evidence, list):
            topics = summary.get("topics")
            if isinstance(topics, list) and len(topics) != len(evidence):
                errors.append(
                    "GroupSummaryIngestRequest: topic_evidence must have exactly one "
                    "entry per summary topic"
                )
            # A local-only summary may not carry model-derived topics: that would
            # imply message bodies crossed the collector boundary.
            if summary.get("privacy_class") == "local_only" and "model_inference" in evidence:
                errors.append(
                    "GroupSummaryIngestRequest: a local_only summary must not carry "
                    "a model_inference topic"
                )

    if component == "Summary" and isinstance(value, dict):
        collector = value.get("collector_scope")
        producer = value.get("producer")
        producer_scope = producer.get("collector_scope") if isinstance(producer, dict) else None
        if isinstance(collector, dict) and isinstance(producer, dict):
            if producer.get("account_id") != collector.get("account_id"):
                errors.append("Summary: producer.account_id must match collector_scope.account_id")
            if isinstance(producer_scope, dict) and any(
                producer_scope.get(field) != collector.get(field)
                for field in ("account_id", "conversation_kind", "conversation_id")
            ):
                errors.append("Summary: producer.collector_scope must match collector_scope")

    if component == "AccountIdentity" and isinstance(value, dict):
        status = value.get("status")
        self_id = value.get("qq_self_id")
        verified_at = value.get("identity_verified_at")
        source = value.get("identity_source")
        if status == "verified" and (not isinstance(self_id, str) or not self_id.isdigit() or self_id.startswith("0") or not verified_at or source != "napcat_get_login_info"):
            errors.append("AccountIdentity: verified requires a QQ self-id, verification timestamp, and authenticated get_login_info source")
        if status in {"verified", "unverified"} and (isinstance(value.get("runtime_generation"), bool) or not isinstance(value.get("runtime_generation"), int) or value["runtime_generation"] < 1):
            errors.append("AccountIdentity: runtime_generation must be a positive integer")
        if status == "unverified" and (self_id is not None or verified_at is not None or source is not None):
            errors.append("AccountIdentity: unverified must not claim a self-id, verification timestamp, or identity source")

    if component.startswith("Plugin") and "confirm" in value if isinstance(value, dict) else False:
        confirm = value.get("confirm")
        if not isinstance(confirm, str) or confirm != "1":
            errors.append(
                f"{component}: wire confirm must be the string '1' "
                f"(got {confirm!r})"
            )

    if component == "PluginSearchRequest" and isinstance(value, dict) and "count" in value:
        count = value["count"]
        if isinstance(count, bool) or not isinstance(count, int) or not 1 <= count <= 50:
            errors.append(f"{component}: count must be an integer in 1..50 (got {count!r})")

    return errors


def check_fixtures(
    loader: ContractLoader,
    report: ValidationReport,
    *,
    enforce_coverage: bool = True,
) -> list[FixtureRecord]:
    records = load_fixtures(loader, report)
    valid_by_component: dict[str, int] = {}
    invalid_by_component: dict[str, int] = {}

    for record in records:
        where = f"{record.source}: record {record.index} ({record.component})"
        errors = loader.component_errors(record.component, record.value)
        semantic = semantic_errors(record.component, record.value)
        combined = errors + semantic
        if record.valid:
            valid_by_component[record.component] = (
                valid_by_component.get(record.component, 0) + 1
            )
            if combined:
                report.errors.append(
                    f"{where}: valid fixture was rejected:\n  - " + "\n  - ".join(combined)
                )
        else:
            invalid_by_component[record.component] = (
                invalid_by_component.get(record.component, 0) + 1
            )
            if not combined:
                report.errors.append(
                    f"{where}: invalid fixture was accepted; description: "
                    f"{record.description}"
                )

    if enforce_coverage:
        expected = {
            name for names in EXPECTED_COMPONENTS.values() for name in names
        }
        known = set(loader.components())
        for name in sorted(expected - known):
            report.errors.append(f"missing required component {name!r}")
        for name in sorted(expected):
            if valid_by_component.get(name, 0) < 1:
                report.errors.append(f"component {name!r} has no valid fixture")
            if invalid_by_component.get(name, 0) < 1:
                report.errors.append(f"component {name!r} has no invalid fixture")

    report.stats.update(
        {
            "fixtures": len(records),
            "fixtures_valid": sum(valid_by_component.values()),
            "fixtures_invalid": sum(invalid_by_component.values()),
            "components_with_valid": len(valid_by_component),
            "components_with_invalid": len(invalid_by_component),
        }
    )
    return records


# ---------------------------------------------------------------------------
# Top-level entry point
# ---------------------------------------------------------------------------


def validate_contracts(
    contracts_root: Path | str,
    *,
    enforce_fixtures: bool = True,
    enforce_coverage: bool = True,
) -> ValidationReport:
    report = ValidationReport()
    loader = ContractLoader(contracts_root)

    files = default_spec_files(loader.root)
    if not files:
        report.errors.append(f"{loader.root}: no OpenAPI/schema files found")
        return report
    gate = RefGate(loader.root)
    for path in files:
        _, issues = gate.check_document(path)
        for issue in issues:
            report.errors.append(issue.render(loader.root))
    report.stats["gate_files"] = len(files)

    if not loader.schema_files():
        report.errors.append(f"{loader.schema_dir}: no schema files found")
    else:
        try:
            components = loader.components()
        except RefGateError as exc:
            report.errors.append(str(exc))
            components = {}
        for name, (schema, source) in sorted(components.items()):
            for message in schema_definition_errors(
                loader,
                schema,
                source,
                pointer=f"{source.name}#/{name}",
                component_name=name,
            ):
                report.errors.append(message)

    report.errors.extend(validate_openapi(loader))

    if enforce_fixtures:
        check_fixtures(loader, report, enforce_coverage=enforce_coverage)

    report.stats["errors"] = len(report.errors)
    report.stats["warnings"] = len(report.warnings)
    return report


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("contracts", nargs="?", default="contracts")
    parser.add_argument(
        "--no-fixtures",
        action="store_true",
        help="skip contracts/examples validation (not used by the pipeline)",
    )
    parser.add_argument(
        "--no-coverage",
        action="store_true",
        help="do not require positive and negative fixtures for every component",
    )
    args = parser.parse_args(argv)

    try:
        report = validate_contracts(
            args.contracts,
            enforce_fixtures=not args.no_fixtures,
            enforce_coverage=not args.no_coverage,
        )
    except (RefGateError, OSError) as exc:
        print(f"contracts: FATAL: {exc}", file=sys.stderr)
        return 2

    for error in report.errors:
        print(f"contracts: {error}", file=sys.stderr)
    if report.errors:
        print(
            f"contracts: FAILED: {report.stats.get('errors', len(report.errors))} error(s)",
            file=sys.stderr,
        )
        return 1

    stats = " ".join(f"{key}={value}" for key, value in sorted(report.stats.items()))
    print(f"contracts: OK: {stats}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
