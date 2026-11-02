package characterscope

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var qqIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

type ErrorCode string

const (
	ErrInvalidBinding  ErrorCode = "invalid_binding"
	ErrDuplicateScope  ErrorCode = "duplicate_scope"
	ErrContextMismatch ErrorCode = "context_mismatch"
	ErrTaskNotFound    ErrorCode = "task_not_found"
	ErrManagedBotEvent ErrorCode = "managed_bot_event"
	ErrChatGateMissing ErrorCode = "chat_gate_missing"
)

type ScopeError struct {
	Code ErrorCode
	Err  error
}

func (e *ScopeError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Err.Error()
}
func (e *ScopeError) Unwrap() error { return e.Err }

func hasCode(err error, code ErrorCode) bool {
	var scopeErr *ScopeError
	return errors.As(err, &scopeErr) && scopeErr.Code == code
}

type Binding struct {
	WorkspaceID             string
	Platform                string
	AccountID               string
	QQSelfID                string
	RuntimeID               string
	AdapterID               string
	AdapterInstanceID       string
	CharacterRuntimeID      string
	CharacterDBID           string
	CharacterCacheNamespace string
	PersonaID               string
}

type ConversationKind string

const (
	ConversationGroup   ConversationKind = "group"
	ConversationPrivate ConversationKind = "private"
)

type Message struct {
	Role string
	Text string
}

type EventContext struct {
	WorkspaceID, Platform, QQSelfID, PersonaID                   string
	AccountID, RuntimeID, AdapterID, AdapterInstanceID, SenderID string
}

type taskState struct {
	context        EventContext
	payload, retry string
	hasRetry       bool
}

type AdapterCall struct {
	AccountID, RuntimeID, AdapterID, SenderID string
	Kind, TargetID, Text                      string
}

// ChatGate consumes the exact durable permit before the sole text adapter path.
type ChatGate interface {
	SendText(EventContext, string, ConversationKind, string, string) (GatewayReceipt, error)
}

// GatewayReceipt is a model/test contract, not proof of a network writer.
type GatewayReceipt struct { State, MessageID string }

type Runtime struct {
	mu               sync.Mutex
	bindings         map[string]Binding
	runtimeToAccount map[string]string
	managedSelfIDs   map[string]struct{}
	db               map[string]map[string][]Message
	cache            map[string]map[string]string
	active           map[string]map[string]ConversationKind
	tasks            map[string]taskState
	adapterCalls     []AdapterCall
	chatGate         ChatGate
}

func New(bindings []Binding) (*Runtime, error) { return NewWithChatGate(bindings, nil) }

func NewWithChatGate(bindings []Binding, gate ChatGate) (*Runtime, error) {
	r := &Runtime{
		bindings: make(map[string]Binding, len(bindings)), runtimeToAccount: make(map[string]string, len(bindings)),
		managedSelfIDs: make(map[string]struct{}, len(bindings)), db: make(map[string]map[string][]Message, len(bindings)),
		cache: make(map[string]map[string]string, len(bindings)), active: make(map[string]map[string]ConversationKind, len(bindings)),
		tasks: make(map[string]taskState), chatGate: gate,
	}
	seen := make(map[string]string, len(bindings)*6)
	for _, binding := range bindings {
		if err := validateBinding(binding); err != nil {
			return nil, err
		}
		accountKey := scoped(binding.WorkspaceID, binding.AccountID)
		if _, exists := r.bindings[accountKey]; exists {
			return nil, &ScopeError{Code: ErrDuplicateScope, Err: fmt.Errorf("account %q is registered more than once in workspace %q", binding.AccountID, binding.WorkspaceID)}
		}
		for _, resource := range []struct{ kind, id string }{
			{"runtime", binding.RuntimeID}, {"adapter instance", binding.AdapterInstanceID},
			{"Character runtime", binding.CharacterRuntimeID}, {"Character DB", binding.CharacterDBID},
			{"Character cache", binding.CharacterCacheNamespace}, {"QQ self id", binding.QQSelfID},
		} {
			key := scoped(binding.WorkspaceID, resource.id)
			if owner, exists := seen[key]; exists {
				return nil, &ScopeError{Code: ErrDuplicateScope, Err: fmt.Errorf("%s resource %q is already used by %s", resource.kind, resource.id, owner)}
			}
			seen[key] = resource.kind + " for " + binding.AccountID
		}
		r.bindings[accountKey] = binding
		r.runtimeToAccount[scoped(binding.WorkspaceID, binding.RuntimeID)] = binding.AccountID
		r.managedSelfIDs[scoped(binding.WorkspaceID, binding.QQSelfID)] = struct{}{}
		r.db[scoped(binding.WorkspaceID, binding.CharacterDBID)] = make(map[string][]Message)
		r.cache[scoped(binding.WorkspaceID, binding.CharacterCacheNamespace)] = make(map[string]string)
		r.active[scoped(binding.WorkspaceID, binding.CharacterRuntimeID)] = make(map[string]ConversationKind)
	}
	return r, nil
}

func (r *Runtime) Receive(context EventContext, kind ConversationKind, targetID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, err := r.validateContext(context)
	if err != nil {
		return err
	}
	if _, managed := r.managedSelfIDs[scoped(context.WorkspaceID, context.SenderID)]; managed {
		return &ScopeError{Code: ErrManagedBotEvent, Err: errors.New("managed bot messages cannot trigger Character replies")}
	}
	if !validConversation(kind, targetID) {
		return &ScopeError{Code: ErrInvalidBinding, Err: errors.New("conversation kind and target are required")}
	}
	key := conversationKey(binding, kind, targetID)
	r.active[scoped(binding.WorkspaceID, binding.CharacterRuntimeID)][key] = kind
	db := r.db[scoped(binding.WorkspaceID, binding.CharacterDBID)]
	db[key] = append(db[key], Message{Role: "user", Text: text})
	return nil
}

func (r *Runtime) AppendAssistant(context EventContext, kind ConversationKind, targetID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, err := r.validateContext(context)
	if err != nil {
		return err
	}
	if !validConversation(kind, targetID) {
		return &ScopeError{Code: ErrInvalidBinding, Err: errors.New("conversation kind and target are required")}
	}
	key := conversationKey(binding, kind, targetID)
	if _, exists := r.active[scoped(binding.WorkspaceID, binding.CharacterRuntimeID)][key]; !exists {
		return &ScopeError{Code: ErrContextMismatch, Err: errors.New("conversation is not active in this Character runtime")}
	}
	db := r.db[scoped(binding.WorkspaceID, binding.CharacterDBID)]
	db[key] = append(db[key], Message{Role: "assistant", Text: text})
	return nil
}

func (r *Runtime) History(context EventContext, kind ConversationKind, targetID string) ([]Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, err := r.validateContext(context)
	if err != nil {
		return nil, err
	}
	if !validConversation(kind, targetID) {
		return nil, &ScopeError{Code: ErrInvalidBinding, Err: errors.New("conversation kind and target are required")}
	}
	messages := r.db[scoped(binding.WorkspaceID, binding.CharacterDBID)][conversationKey(binding, kind, targetID)]
	return append([]Message(nil), messages...), nil
}

func (r *Runtime) SetCache(context EventContext, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, err := r.validateContext(context)
	if err != nil {
		return err
	}
	r.cache[scoped(binding.WorkspaceID, binding.CharacterCacheNamespace)][key] = value
	return nil
}

func (r *Runtime) GetCache(context EventContext, key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, err := r.validateContext(context)
	if err != nil {
		return "", false, err
	}
	value, ok := r.cache[scoped(binding.WorkspaceID, binding.CharacterCacheNamespace)][key]
	return value, ok, nil
}

func (r *Runtime) StartTask(context EventContext, taskID, payload string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.validateContext(context); err != nil {
		return err
	}
	if strings.TrimSpace(taskID) == "" {
		return &ScopeError{Code: ErrInvalidBinding, Err: errors.New("task id is required")}
	}
	key := scoped(context.WorkspaceID, taskID)
	if _, exists := r.tasks[key]; exists {
		return &ScopeError{Code: ErrDuplicateScope, Err: fmt.Errorf("task %q already exists", taskID)}
	}
	r.tasks[key] = taskState{context: context, payload: payload}
	return nil
}

func (r *Runtime) SetRetry(context EventContext, taskID, retry string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.validateContext(context); err != nil {
		return err
	}
	key := scoped(context.WorkspaceID, taskID)
	task, exists := r.tasks[key]
	if !exists {
		return &ScopeError{Code: ErrTaskNotFound}
	}
	if task.context != context {
		return &ScopeError{Code: ErrContextMismatch, Err: errors.New("retry context does not match task owner")}
	}
	task.retry, task.hasRetry = retry, true
	r.tasks[key] = task
	return nil
}

func (r *Runtime) ConsumeRetry(context EventContext, taskID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.validateContext(context); err != nil {
		return "", err
	}
	key := scoped(context.WorkspaceID, taskID)
	task, exists := r.tasks[key]
	if !exists {
		return "", &ScopeError{Code: ErrTaskNotFound}
	}
	if task.context != context {
		return "", &ScopeError{Code: ErrContextMismatch, Err: errors.New("task retry belongs to another account/runtime/adapter/sender")}
	}
	if !task.hasRetry {
		return "", &ScopeError{Code: ErrTaskNotFound, Err: errors.New("task has no retry context")}
	}
	retry := task.retry
	task.retry, task.hasRetry = "", false
	r.tasks[key] = task
	return retry, nil
}

// SendText is a model/test contract: the injected final gateway owns execution.
// This runtime records acknowledged receipts in memory; it is NOT a network writer.
// Rich media and direct adapter APIs are not exposed.
func (r *Runtime) SendText(context EventContext, adapterInstanceID, permitID string, kind ConversationKind, targetID, text string) error {
	r.mu.Lock()
	binding, err := r.validateContext(context)
	if err == nil && adapterInstanceID != binding.AdapterInstanceID {
		err = &ScopeError{Code: ErrContextMismatch, Err: errors.New("sender context cannot invoke another account adapter instance")}
	}
	if err == nil && r.chatGate == nil {
		err = &ScopeError{Code: ErrChatGateMissing, Err: errors.New("chat send gate is not configured")}
	}
	if err == nil && (strings.TrimSpace(permitID) == "" || !validConversation(kind, targetID) || text == "") {
		err = &ScopeError{Code: ErrInvalidBinding, Err: errors.New("permit, conversation, and non-empty text are required")}
	}
	if err == nil {
		key := conversationKey(binding, kind, targetID)
		if _, exists := r.active[scoped(binding.WorkspaceID, binding.CharacterRuntimeID)][key]; !exists {
			err = &ScopeError{Code: ErrContextMismatch, Err: errors.New("outbound text requires an active account-scoped trigger")}
		}
	}
	if err != nil {
		r.mu.Unlock()
		return err
	}
	gate := r.chatGate
	r.mu.Unlock()
	receipt, err := gate.SendText(context, permitID, kind, targetID, text)
	if err != nil { return err }
	if receipt.State != "sent" || receipt.MessageID == "" { return &ScopeError{Code:ErrChatGateMissing, Err:errors.New("gateway terminal receipt unavailable")} }
	r.mu.Lock()
	r.adapterCalls = append(r.adapterCalls, AdapterCall{AccountID: context.AccountID, RuntimeID: context.RuntimeID, AdapterID: binding.AdapterID, SenderID: context.SenderID, Kind: string(kind), TargetID: targetID, Text: text})
	r.mu.Unlock()
	return nil
}

// AdapterCalls is a legacy name for acknowledged model receipts, not network calls.
func (r *Runtime) AdapterCalls() []AdapterCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AdapterCall(nil), r.adapterCalls...)
}

func (r *Runtime) validateContext(context EventContext) (Binding, error) {
	binding, exists := r.bindings[scoped(context.WorkspaceID, context.AccountID)]
	if !exists {
		return Binding{}, &ScopeError{Code: ErrContextMismatch, Err: errors.New("account is not registered")}
	}
	if binding.WorkspaceID != context.WorkspaceID || binding.Platform != context.Platform || binding.QQSelfID != context.QQSelfID || binding.PersonaID != context.PersonaID || binding.RuntimeID != context.RuntimeID || binding.AdapterID != context.AdapterID || binding.AdapterInstanceID != context.AdapterInstanceID || context.SenderID == "" {
		return Binding{}, &ScopeError{Code: ErrContextMismatch, Err: errors.New("event context does not match registered account binding")}
	}
	if accountID, exists := r.runtimeToAccount[scoped(context.WorkspaceID, context.RuntimeID)]; !exists || accountID != context.AccountID {
		return Binding{}, &ScopeError{Code: ErrContextMismatch, Err: errors.New("runtime is not owned by account")}
	}
	return binding, nil
}

func validateBinding(binding Binding) error {
	for label, value := range map[string]string{
		"workspace_id": binding.WorkspaceID, "platform": binding.Platform, "account_id": binding.AccountID,
		"qq_self_id": binding.QQSelfID, "runtime_id": binding.RuntimeID, "adapter_id": binding.AdapterID,
		"adapter_instance_id": binding.AdapterInstanceID, "character_runtime_id": binding.CharacterRuntimeID,
		"character_db_id": binding.CharacterDBID, "character_cache_namespace": binding.CharacterCacheNamespace,
		"persona_id": binding.PersonaID,
	} {
		if strings.TrimSpace(value) == "" {
			return &ScopeError{Code: ErrInvalidBinding, Err: fmt.Errorf("%s is required", label)}
		}
	}
	if !qqIDPattern.MatchString(binding.QQSelfID) {
		return &ScopeError{Code: ErrInvalidBinding, Err: errors.New("qq_self_id must be a canonical decimal id")}
	}
	return nil
}

func validConversation(kind ConversationKind, targetID string) bool {
	return (kind == ConversationGroup || kind == ConversationPrivate) && strings.TrimSpace(targetID) != ""
}

func conversationKey(binding Binding, kind ConversationKind, targetID string) string {
	parts := []string{binding.WorkspaceID, binding.Platform, binding.QQSelfID, string(kind), targetID, binding.PersonaID}
	var key strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&key, "%d:%s|", len(part), part)
	}
	return key.String()
}

func scoped(workspaceID, id string) string {
	return fmt.Sprintf("%d:%s|%d:%s", len(workspaceID), workspaceID, len(id), id)
}
