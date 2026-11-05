//go:build contractsmoke

package contract_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"bba.local/taskengine/internal/contract"
)

// componentTypes is deliberately exhaustive and compile-time checked: every
// contract component named by P0-01 must exist in the generated Go package.
func goType[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

var componentTypes = map[string]reflect.Type{
	// Offline goal-loop wire contract
	"GoalRunStartRequest":      goType[contract.GoalRunStartRequest](),
	"GoalAgentSnapshot":        goType[contract.GoalAgentSnapshot](),
	"GoalSkillSnapshot":        goType[contract.GoalSkillSnapshot](),
	"GoalModelSnapshot":        goType[contract.GoalModelSnapshot](),
	"GoalRoleSnapshot":         goType[contract.GoalRoleSnapshot](),
	"GoalConfiguration":        goType[contract.GoalConfiguration](),
	"GoalMemory":               goType[contract.GoalMemory](),
	"GoalAsset":                goType[contract.GoalAsset](),
	"GoalNode":                 goType[contract.GoalNode](),
	"GoalEdge":                 goType[contract.GoalEdge](),
	"GoalAnchor":               goType[contract.GoalAnchor](),
	"GoalGraph":                goType[contract.GoalGraph](),
	"GoalEvidence":             goType[contract.GoalEvidence](),
	"GoalStep":                 goType[contract.GoalStep](),
	"GoalRound":                goType[contract.GoalRound](),
	"GoalRun":                  goType[contract.GoalRun](),
	"GoalPlanOutput":           goType[contract.GoalPlanOutput](),
	"GoalAnalysisOutput":       goType[contract.GoalAnalysisOutput](),
	"GoalRunEnvelope":          goType[contract.GoalRunEnvelope](),
	"GoalRunControlRequest":    goType[contract.GoalRunControlRequest](),
	"GoalFixtureWindowRequest": goType[contract.GoalFixtureWindowRequest](),
	"GoalRunListEnvelope":      goType[contract.GoalRunListEnvelope](),

	// operator-conversations.json: B7 durable messages, reviews and local notices
	"OperatorConversationPageEnvelope":  goType[contract.OperatorConversationPageEnvelope](),
	"OperatorMessagePageEnvelope":       goType[contract.OperatorMessagePageEnvelope](),
	"OperatorAgentMessageRequest":       goType[contract.OperatorAgentMessageRequest](),
	"OperatorAgentMessageEnvelope":      goType[contract.OperatorAgentMessageEnvelope](),
	"OperatorQQDraftRequest":            goType[contract.OperatorQQDraftRequest](),
	"OperatorQQDraftEnvelope":           goType[contract.OperatorQQDraftEnvelope](),
	"OperatorQQDraftDetailEnvelope":     goType[contract.OperatorQQDraftDetailEnvelope](),
	"OperatorQQInboxEventRequest":       goType[contract.OperatorQQInboxEventRequest](),
	"OperatorQQInboxEventEnvelope":      goType[contract.OperatorQQInboxEventEnvelope](),
	"OperatorNotificationPageEnvelope":  goType[contract.OperatorNotificationPageEnvelope](),
	"OperatorNotificationReadRequest":   goType[contract.OperatorNotificationReadRequest](),
	"OperatorNotificationReadEnvelope":  goType[contract.OperatorNotificationReadEnvelope](),
	"OperatorConversationErrorEnvelope": goType[contract.OperatorConversationErrorEnvelope](),

	// common.json
	"Id":                    goType[contract.Id](),
	"QQId":                  goType[contract.QQId](),
	"DecimalSequence":       goType[contract.DecimalSequence](),
	"Digest":                goType[contract.Digest](),
	"UTCDateTime":           goType[contract.UTCDateTime](),
	"RequestMeta":           goType[contract.RequestMeta](),
	"Error":                 goType[contract.Error](),
	"ErrorEnvelope":         goType[contract.ErrorEnvelope](),
	"Target":                goType[contract.Target](),
	"Pagination":            goType[contract.Pagination](),
	"Receipt":               goType[contract.Receipt](),
	"Account":               goType[contract.Account](),
	"AccountIdentityStatus": goType[contract.AccountIdentityStatus](),
	"AccountIdentitySource": goType[contract.AccountIdentitySource](),
	"AccountIdentity":       goType[contract.AccountIdentity](),
	"AccountIdentityPage":   goType[contract.AccountIdentityPage](),
	"AccountPage":           goType[contract.AccountPage](),
	"CapabilityAction":      goType[contract.CapabilityAction](),
	"Capability":            goType[contract.Capability](),
	"CapabilityPage":        goType[contract.CapabilityPage](),
	"AccountControlRequest": goType[contract.AccountControlRequest](),
	"TaskControlRequest":    goType[contract.TaskControlRequest](),
	"VersionRequest":        goType[contract.VersionRequest](),
	"ReconcileRequest":      goType[contract.ReconcileRequest](),
	"HeartbeatRequest":      goType[contract.HeartbeatRequest](),
	"HeartbeatReceipt":      goType[contract.HeartbeatReceipt](),

	// tasks.json
	"TaskState":              goType[contract.TaskState](),
	"ExecutionMode":          goType[contract.ExecutionMode](),
	"Goal":                   goType[contract.Goal](),
	"FixedCandidate":         goType[contract.FixedCandidate](),
	"CandidateSource":        goType[contract.CandidateSource](),
	"TaskLimits":             goType[contract.TaskLimits](),
	"CreateTaskRequest":      goType[contract.CreateTaskRequest](),
	"Task":                   goType[contract.Task](),
	"Subtask":                goType[contract.Subtask](),
	"SubtaskPage":            goType[contract.SubtaskPage](),
	"Result":                 goType[contract.Result](),
	"ResultPage":             goType[contract.ResultPage](),
	"TaskEvent":              goType[contract.TaskEvent](),
	"TaskEventPage":          goType[contract.TaskEventPage](),
	"TaskEnvelope":           goType[contract.TaskEnvelope](),
	"TaskPageEnvelope":       goType[contract.TaskPageEnvelope](),
	"ReceiptEnvelope":        goType[contract.ReceiptEnvelope](),
	"AccountPageEnvelope":    goType[contract.AccountPageEnvelope](),
	"CapabilityPageEnvelope": goType[contract.CapabilityPageEnvelope](),
	"SubtaskPageEnvelope":    goType[contract.SubtaskPageEnvelope](),
	"ResultPageEnvelope":     goType[contract.ResultPageEnvelope](),
	"TaskEventPageEnvelope":  goType[contract.TaskEventPageEnvelope](),
	"HeartbeatEnvelope":      goType[contract.HeartbeatEnvelope](),

	// bridge.json
	"Action":                   goType[contract.Action](),
	"EffectCertainty":          goType[contract.EffectCertainty](),
	"Disposition":              goType[contract.Disposition](),
	"ExecutionState":           goType[contract.ExecutionState](),
	"JoinParams":               goType[contract.JoinParams](),
	"SearchParams":             goType[contract.SearchParams](),
	"MembershipParams":         goType[contract.MembershipParams](),
	"JoinObservationParams":    goType[contract.JoinObservationParams](),
	"SendTextParams":           goType[contract.SendTextParams](),
	"ExecutionRequest":         goType[contract.ExecutionRequest](),
	"Operation":                goType[contract.Operation](),
	"OperationEnvelope":        goType[contract.OperationEnvelope](),
	"Observation":              goType[contract.Observation](),
	"ObservationReceipt":       goType[contract.ObservationReceipt](),
	"ObservationEnvelope":      goType[contract.ObservationEnvelope](),
	"ObservationQueryRequest":  goType[contract.ObservationQueryRequest](),
	"ObservationQueryResult":   goType[contract.ObservationQueryResult](),
	"ObservationQueryEnvelope": goType[contract.ObservationQueryEnvelope](),
	"DispatchPermitRequest":    goType[contract.DispatchPermitRequest](),
	"DispatchPermit":           goType[contract.DispatchPermit](),
	"DispatchPermitEnvelope":   goType[contract.DispatchPermitEnvelope](),
	"ChatPermitRequest":        goType[contract.ChatPermitRequest](),
	"ChatPermit":               goType[contract.ChatPermit](),
	"ChatPermitEnvelope":       goType[contract.ChatPermitEnvelope](),

	// models.json
	"PlanProposalRequest": goType[contract.PlanProposalRequest](),
	"PlanApplyRequest":    goType[contract.PlanApplyRequest](),
	"PlanStep":            goType[contract.PlanStep](),
	"PlanProposal":        goType[contract.PlanProposal](),
	"IntentDraft":         goType[contract.IntentDraft](),
	"Summary":             goType[contract.Summary](),
	"ModelRequest":        goType[contract.ModelRequest](),
	"ModelResponse":       goType[contract.ModelResponse](),
	"ModelEnvelope":       goType[contract.ModelEnvelope](),

	// models.json: Summary.v1 runtime routes
	"GroupSummaryIngestRequest":  goType[contract.GroupSummaryIngestRequest](),
	"GroupSummaryIngestResponse": goType[contract.GroupSummaryIngestResponse](),
	"GroupSummaryIngestEnvelope": goType[contract.GroupSummaryIngestEnvelope](),
	"GroupContextTopic":          goType[contract.GroupContextTopic](),
	"GroupContext":               goType[contract.GroupContext](),
	"GroupContextEnvelope":       goType[contract.GroupContextEnvelope](),

	// models.json: proposal read/approve routes
	"PlanProposalApprovalRequest":  goType[contract.PlanProposalApprovalRequest](),
	"PlanProposalApprovalReceipt":  goType[contract.PlanProposalApprovalReceipt](),
	"PlanProposalApprovalEnvelope": goType[contract.PlanProposalApprovalEnvelope](),
	"PlanProposalView":             goType[contract.PlanProposalView](),
	"PlanProposalBasis":            goType[contract.PlanProposalBasis](),
	"PlanProposalEnvelope":         goType[contract.PlanProposalEnvelope](),

	// models.json: task graph projection route
	"TaskGraphAnchor":   goType[contract.TaskGraphAnchor](),
	"TaskGraphNode":     goType[contract.TaskGraphNode](),
	"TaskGraphFact":     goType[contract.TaskGraphFact](),
	"TaskGraphEdge":     goType[contract.TaskGraphEdge](),
	"TaskGraph":         goType[contract.TaskGraph](),
	"TaskGraphEnvelope": goType[contract.TaskGraphEnvelope](),

	// login.json
	"LoginSessionState":         goType[contract.LoginSessionState](),
	"LoginSessionFailureReason": goType[contract.LoginSessionFailureReason](),
	"LoginSessionNextAction":    goType[contract.LoginSessionNextAction](),
	"LoginProviderMode":         goType[contract.LoginProviderMode](),
	"LoginStatus":               goType[contract.LoginStatus](),
	"LoginStatusEnvelope":       goType[contract.LoginStatusEnvelope](),
	"LoginGenerationRequest":    goType[contract.LoginGenerationRequest](),
	"LoginSessionIdentity":      goType[contract.LoginSessionIdentity](),
	"LoginSessionProjection":    goType[contract.LoginSessionProjection](),
	"LoginSession":              goType[contract.LoginSession](),
	"LoginSessionPage":          goType[contract.LoginSessionPage](),
	"LoginSessionEnvelope":      goType[contract.LoginSessionEnvelope](),
	"LoginSessionPageEnvelope":  goType[contract.LoginSessionPageEnvelope](),

	// graph.json
	"GraphSyncProviderStatus":           goType[contract.GraphSyncProviderStatus](),
	"GraphSyncError":                    goType[contract.GraphSyncError](),
	"GraphSyncCheckpoint":               goType[contract.GraphSyncCheckpoint](),
	"GraphSyncGraphStatus":              goType[contract.GraphSyncGraphStatus](),
	"GraphSyncStatus":                   goType[contract.GraphSyncStatus](),
	"GraphSyncStatusEnvelope":           goType[contract.GraphSyncStatusEnvelope](),
	"GraphRootRequest":                  goType[contract.GraphRootRequest](),
	"GraphPlanSummaryRef":               goType[contract.GraphPlanSummaryRef](),
	"GraphPlanCandidate":                goType[contract.GraphPlanCandidate](),
	"GraphPlanningSnapshot":             goType[contract.GraphPlanningSnapshot](),
	"GraphPlanningSnapshotEnvelope":     goType[contract.GraphPlanningSnapshotEnvelope](),
	"GraphPlanningSnapshotList":         goType[contract.GraphPlanningSnapshotList](),
	"GraphPlanningSnapshotListEnvelope": goType[contract.GraphPlanningSnapshotListEnvelope](),
	"GraphSyncReconcileRequest":         goType[contract.GraphSyncReconcileRequest](),
	"GraphSyncOutboxItem":               goType[contract.GraphSyncOutboxItem](),
	"GraphSyncOutboxItemEnvelope":       goType[contract.GraphSyncOutboxItemEnvelope](),

	// product.json and runtime.json
	"MaterializeRequest":         goType[contract.MaterializeRequest](),
	"WorkItemAttemptView":        goType[contract.WorkItemAttemptView](),
	"OrchestrationMode":          goType[contract.OrchestrationMode](),
	"OrchestrationModeRequest":   goType[contract.OrchestrationModeRequest](),
	"WorkItemView":               goType[contract.WorkItemView](),
	"WorkItemPageEnvelope":       goType[contract.WorkItemPageEnvelope](),
	"OperatorRuntimeEnvelope":    goType[contract.OperatorRuntimeEnvelope](),
	"OperatorRuntime":            goType[contract.OperatorRuntime](),
	"RuntimeMeta":                goType[contract.RuntimeMeta](),
	"GroupContextReportEnvelope": goType[contract.GroupContextEnvelope](),

	// plugin.json
	"PluginSearchRequest":  goType[contract.PluginSearchRequest](),
	"PluginGroup":          goType[contract.PluginGroup](),
	"PluginSearchResponse": goType[contract.PluginSearchResponse](),
	"PluginJoinRequest":    goType[contract.PluginJoinRequest](),
	"PluginJoinResponse":   goType[contract.PluginJoinResponse](),
}

type fixtureRecord struct {
	SchemaRef   string          `json:"schema_ref"`
	Valid       bool            `json:"valid"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("CONTRACT_ROOT"); root != "" {
		return filepath.Join(root, "examples")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate smoke_test.go")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "examples")
}

func componentFromRef(ref string) (string, error) {
	hash := strings.LastIndex(ref, "#")
	if hash < 0 || hash == len(ref)-1 {
		return "", fmt.Errorf("invalid schema_ref %q", ref)
	}
	fragment := ref[hash+1:]
	if !strings.HasPrefix(fragment, "/") {
		return "", fmt.Errorf("schema_ref %q has a non-pointer fragment", ref)
	}
	return fragment[strings.LastIndex(fragment, "/")+1:], nil
}

func jsonFieldTags(t reflect.Type) map[string]reflect.StructField {
	tags := map[string]reflect.StructField{}
	for i := range t.NumField() {
		field := t.Field(i)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		tags[name] = field
	}
	return tags
}

func hasCustomUnmarshal(t reflect.Type) bool {
	unmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	return reflect.PointerTo(t).Implements(unmarshaler)
}

func TestGeneratedTypesCoverContractComponents(t *testing.T) {
	var required = []string{
		"Id", "QQId", "DecimalSequence", "Digest", "UTCDateTime", "RequestMeta",
		"Error", "ErrorEnvelope", "Target", "Pagination", "Receipt", "Account",
		"AccountIdentityStatus", "AccountIdentitySource", "AccountIdentity", "AccountIdentityPage",
		"AccountPage", "CapabilityAction", "Capability", "CapabilityPage",
		"AccountControlRequest", "TaskControlRequest", "VersionRequest",
		"ReconcileRequest", "HeartbeatRequest", "HeartbeatReceipt",
		"TaskState", "ExecutionMode", "Goal", "FixedCandidate", "CandidateSource",
		"TaskLimits", "CreateTaskRequest", "Task", "Subtask", "SubtaskPage",
		"Result", "ResultPage", "TaskEvent", "TaskEventPage", "TaskEnvelope", "TaskPageEnvelope",
		"ReceiptEnvelope", "AccountPageEnvelope", "CapabilityPageEnvelope",
		"SubtaskPageEnvelope", "ResultPageEnvelope", "TaskEventPageEnvelope",
		"HeartbeatEnvelope", "Action", "EffectCertainty", "Disposition",
		"ExecutionState", "JoinParams", "SearchParams", "MembershipParams",
		"JoinObservationParams", "SendTextParams", "ExecutionRequest",
		"Operation", "OperationEnvelope", "Observation", "ObservationReceipt",
		"ObservationEnvelope", "ObservationQueryRequest", "ObservationQueryResult",
		"ObservationQueryEnvelope", "DispatchPermitRequest", "DispatchPermit",
		"DispatchPermitEnvelope", "ChatPermitRequest", "ChatPermit",
		"ChatPermitEnvelope", "PlanProposalRequest", "PlanApplyRequest", "PlanStep",
		"PlanProposal", "IntentDraft", "Summary", "ModelRequest", "ModelResponse",
		"ModelEnvelope", "PluginSearchRequest", "PluginGroup", "PluginSearchResponse",
		"PluginJoinRequest", "PluginJoinResponse",
	}
	for _, name := range required {
		if _, ok := componentTypes[name]; !ok {
			t.Errorf("generated type for component %s is missing", name)
		}
	}
}

func TestPluginJoinConfirmIsString(t *testing.T) {
	typ := componentTypes["PluginJoinRequest"]
	if typ.Kind() != reflect.Struct {
		t.Fatalf("PluginJoinRequest is %s, want struct", typ.Kind())
	}
	field, ok := jsonFieldTags(typ)["confirm"]
	if !ok {
		t.Fatal("PluginJoinRequest has no json 'confirm' field")
	}
	if field.Type.Kind() != reflect.String {
		t.Fatalf("PluginJoinRequest.confirm is %s, want string (wire confirm \"1\")", field.Type)
	}
}

func TestAccountIdentityGeneratedFieldsPreserveNullableReadback(t *testing.T) {
	fields := jsonFieldTags(componentTypes["AccountIdentity"])
	for _, name := range []string{"account_id", "qq_self_id", "runtime_id", "adapter_id", "adapter_instance_id", "napcat_instance_id", "character_runtime_id", "character_db_id", "character_cache_namespace", "capability_profile_id", "status", "identity_verified_at", "identity_source", "runtime_generation"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("AccountIdentity generated type is missing json field %q", name)
		}
	}
}

func TestFixturesRoundTripGeneratedTypes(t *testing.T) {
	root := fixtureRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixture files under %s: %v", root, err)
	}

	tested := 0
	for _, path := range files {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var records []fixtureRecord
		if err := json.Unmarshal(payload, &records); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for index, record := range records {
			if !record.Valid {
				continue
			}
			component, err := componentFromRef(record.SchemaRef)
			if err != nil {
				t.Fatalf("%s[%d]: %v", path, index, err)
			}
			typ, ok := componentTypes[component]
			if !ok {
				t.Fatalf("%s[%d]: no generated Go type for %q", path, index, component)
			}

			holder := reflect.New(typ)
			if err := json.Unmarshal(record.Value, holder.Interface()); err != nil {
				t.Errorf("%s[%d] %s: generated Go type rejected valid fixture: %v",
					path, index, component, err)
				continue
			}

			encoded, err := json.Marshal(holder.Elem().Interface())
			if err != nil {
				t.Errorf("%s[%d] %s: encode failed for valid fixture: %v",
					path, index, component, err)
				continue
			}

			var original, roundTripped any
			if err := json.Unmarshal(record.Value, &original); err != nil {
				t.Fatalf("%s[%d]: fixture is not valid JSON: %v", path, index, err)
			}
			if err := json.Unmarshal(encoded, &roundTripped); err != nil {
				t.Errorf("%s[%d] %s: generated Go type encoded invalid JSON: %v",
					path, index, component, err)
				continue
			}
			if !reflect.DeepEqual(original, roundTripped) {
				t.Errorf("%s[%d] %s: encode/decode lost or changed fields\nfixture: %s\ngenerated: %s",
					path, index, component, record.Value, encoded)
			}

			if object, ok := original.(map[string]any); ok && typ.Kind() == reflect.Struct {
				tags := jsonFieldTags(typ)
				if len(tags) == 0 && hasCustomUnmarshal(typ) {
					// Opaque oneOf wrapper: the generated MarshalJSON owns the fields.
					tested++
					continue
				}
				for key := range object {
					if _, ok := tags[key]; !ok {
						t.Errorf("%s[%d] %s: generated Go type has no json field for %q",
							path, index, component, key)
					}
				}
			}
			tested++
		}
	}
	if tested == 0 {
		t.Fatal("no valid fixtures exercised generated Go types")
	}
	t.Logf("encoded/decoded %d valid fixtures through generated Go types", tested)
}

// The detail route returns historical JSON verbatim. Keep generated Go types
// from coercing nested integers through interface{} / float64.
func TestOperatorDraftHistoricalEvidencePrecision(t *testing.T) {
	for _, evidence := range []string{
		`null`, `[]`, `"history"`, `42`, `true`,
		`{"nested":{"value":9007199254740993}}`,
		`[{"value":9223372036854775807}]`,
	} {
		t.Run(evidence, func(t *testing.T) {
			input := []byte(`{"data":{"id":"draft_contract","account_id":"acc_contract","conversation_id":"conv_contract","message_id":"msg_contract","payload_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","rules_version":9007199254740993,"decision":"draft","evidence":` + evidence + `,"send_permit":"absent","version":1}}`)
			var value contract.OperatorQQDraftDetailEnvelope
			if err := json.Unmarshal(input, &value); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Data struct {
					Evidence     json.RawMessage `json:"evidence"`
					RulesVersion json.RawMessage `json:"rules_version"`
				} `json:"data"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			if string(envelope.Data.Evidence) != evidence {
				t.Fatalf("evidence changed: want %s got %s", evidence, envelope.Data.Evidence)
			}
			if string(envelope.Data.RulesVersion) != "9007199254740993" {
				t.Fatalf("rules_version changed: %s", envelope.Data.RulesVersion)
			}
		})
	}
}
