import type { components } from "./types.gen";

type Schemas = components["schemas"];
type SchemaName = keyof Schemas;

// Every contract component must exist in the generated TypeScript surface.
const REQUIRED_COMPONENTS = [
  "Id", "QQId", "DecimalSequence", "Digest", "UTCDateTime", "RequestMeta",
  "Error", "ErrorEnvelope", "Target", "Pagination", "Receipt", "Account",
  "AccountPage", "CapabilityAction", "Capability", "CapabilityPage",
  "AccountControlRequest", "TaskControlRequest", "VersionRequest",
  "ReconcileRequest", "HeartbeatRequest", "HeartbeatReceipt",
  "TaskState", "ExecutionMode", "Goal", "FixedCandidate", "CandidateSource",
  "TaskLimits", "CreateTaskRequest", "Task", "Subtask", "SubtaskPage",
  "Result", "ResultPage", "TaskEvent", "TaskEventPage", "TaskEnvelope",
  "ReceiptEnvelope", "AccountPageEnvelope", "CapabilityPageEnvelope",
  "SubtaskPageEnvelope", "ResultPageEnvelope", "TaskEventPageEnvelope",
  "HeartbeatEnvelope", "Action", "EffectCertainty", "Disposition",
  "ExecutionState", "JoinParams", "SearchParams", "MembershipParams",
  "JoinObservationParams", "SendTextParams", "ExecutionRequest", "Operation",
  "OperationEnvelope", "Observation", "ObservationReceipt", "ObservationEnvelope",
  "ObservationQueryRequest", "ObservationQueryResult", "ObservationQueryEnvelope",
  "DispatchPermitRequest", "DispatchPermit", "DispatchPermitEnvelope",
  "ChatPermitRequest", "ChatPermit", "ChatPermitEnvelope", "PlanProposalRequest",
  "PlanApplyRequest", "PlanStep", "PlanProposal", "IntentDraft", "Summary",
  "ModelRequest", "ModelResponse", "ModelEnvelope", "PluginSearchRequest",
  "PluginGroup", "PluginSearchResponse", "PluginJoinRequest", "PluginJoinResponse",
] as const satisfies readonly SchemaName[];

void REQUIRED_COMPONENTS;

type PluginJoinRequest = Schemas["PluginJoinRequest"];
type ConfirmField = PluginJoinRequest extends { confirm: infer C } ? C : never;
const canonicalConfirm: ConfirmField = "1";
void canonicalConfirm;
// @ts-expect-error the plugin wire confirm is the string "1", never a JSON number
const numericConfirm: ConfirmField = 1;
void numericConfirm;

type PluginSearchRequest = Schemas["PluginSearchRequest"];
type CountField = PluginSearchRequest extends { count: infer C } ? C : never;
const minimumSearch: CountField = 1;
const maximumSearch: CountField = 50;
void minimumSearch;
void maximumSearch;

export {};
