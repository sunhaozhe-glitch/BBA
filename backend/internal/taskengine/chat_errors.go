package taskengine

// P0-15 chat gate error codes. They are a separate family from the task and
// bridge codes so a runtime can distinguish chat authorization failures from
// task state. The HTTP status table lives beside the permit transport; this
// file only owns the vocabulary so trigger, admin, and permit code agree.
const (
	chatCodeTriggerConflict    = "CHAT_TRIGGER_CONFLICT"
	chatCodeTriggerUnknown     = "CHAT_TRIGGER_UNKNOWN"
	chatCodeTriggerExpired     = "CHAT_TRIGGER_EXPIRED"
	chatCodePermitConsumed     = "CHAT_PERMIT_CONSUMED"
	chatCodePermitExpired      = "CHAT_PERMIT_EXPIRED"
	chatCodePermitUnknown      = "CHAT_PERMIT_UNKNOWN"
	chatCodePermitNotConsumed  = "CHAT_PERMIT_NOT_CONSUMED"
	chatCodeSendDigestConflict = "CHAT_SEND_DIGEST_CONFLICT"
	chatCodeSegmentOutOfOrder  = "CHAT_SEGMENT_OUT_OF_ORDER"
	chatCodePolicyConflict     = "CHAT_POLICY_CONFLICT"
	chatCodeBindingConflict    = "CHAT_BINDING_CONFLICT"
	chatCodePolicyDisabled     = "CHAT_POLICY_DISABLED"
	chatCodeSpeakerNotActive   = "CHAT_SPEAKER_NOT_ACTIVE"
	chatCodeBudgetExhausted    = "CHAT_BUDGET_EXHAUSTED"
	chatCodeOutcomeConflict    = "CHAT_OUTCOME_CONFLICT"
	chatCodeManagedSelfID      = "CHAT_MANAGED_SELF_ID"
	chatCodeAccountRestricted  = "CHAT_ACCOUNT_RESTRICTED"
)

// chatSafeMessages keeps client-visible chat failures stable and free of
// account, group, or message identifiers. The transport in chat_permit_http.go
// owns the status table; thWriteErrorCode consults both maps.
var chatSafeMessages = map[string]string{
	chatCodeTriggerConflict:    "chat trigger conflicts with an existing trigger",
	chatCodeTriggerUnknown:     "chat trigger not found",
	chatCodeTriggerExpired:     "chat trigger expired",
	chatCodePermitConsumed:     "chat permit already consumed",
	chatCodePermitExpired:      "chat permit expired",
	chatCodePermitUnknown:      "chat permit not found",
	chatCodePermitNotConsumed:  "chat permit has not been consumed",
	chatCodeSendDigestConflict: "chat text digest does not match the permit",
	chatCodeSegmentOutOfOrder:  "chat segment is not the next permitted segment",
	chatCodePolicyConflict:     "chat policy changed or is stale",
	chatCodeBindingConflict:    "chat binding epoch or lease is stale",
	chatCodePolicyDisabled:     "chat sending is disabled for this account",
	chatCodeSpeakerNotActive:   "account is not the active speaker for this conversation",
	chatCodeBudgetExhausted:    "chat daily budget is exhausted",
	chatCodeOutcomeConflict:    "chat outcome conflicts with the recorded outcome",
	chatCodeManagedSelfID:      "managed account identity cannot be used",
	chatCodeAccountRestricted:  "chat account or runtime is not currently authorized",
}
