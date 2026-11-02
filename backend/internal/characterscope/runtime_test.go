package characterscope

import (
	"errors"
	"sync"
	"testing"
)

func testBindings() (Binding, Binding) {
	return Binding{
			WorkspaceID: "workspace-a", Platform: "qq", AccountID: "account-a", QQSelfID: "10001", PersonaID: "persona-a",
			RuntimeID: "runtime-a", AdapterID: "onebot", AdapterInstanceID: "adapter-a",
			CharacterRuntimeID: "character-a", CharacterDBID: "character-db-a", CharacterCacheNamespace: "cache-a",
		}, Binding{
			WorkspaceID: "workspace-a", Platform: "qq", AccountID: "account-b", QQSelfID: "10002", PersonaID: "persona-b",
			RuntimeID: "runtime-b", AdapterID: "onebot", AdapterInstanceID: "adapter-b",
			CharacterRuntimeID: "character-b", CharacterDBID: "character-db-b", CharacterCacheNamespace: "cache-b",
		}
}

func event(binding Binding, sender string) EventContext {
	return EventContext{
		WorkspaceID: binding.WorkspaceID, Platform: binding.Platform, QQSelfID: binding.QQSelfID, PersonaID: binding.PersonaID,
		AccountID: binding.AccountID, RuntimeID: binding.RuntimeID, AdapterID: binding.AdapterID,
		AdapterInstanceID: binding.AdapterInstanceID, SenderID: sender,
	}
}

func newTestRuntime(t *testing.T) (*Runtime, Binding, Binding) {
	t.Helper()
	a, b := testBindings()
	runtime, err := New([]Binding{a, b})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, a, b
}

func TestCharacterIsolation(t *testing.T) {
	runtime, a, b := newTestRuntime(t)
	aContext, bContext := event(a, "human-a"), event(b, "human-b")
	if err := runtime.Receive(aContext, ConversationGroup, "42", "A input"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AppendAssistant(aContext, ConversationGroup, "42", "A answer"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AppendAssistant(bContext, ConversationGroup, "42", "cross-account"); !hasCode(err, ErrContextMismatch) {
		t.Fatalf("cross-runtime history write=%v", err)
	}
	if err := runtime.Receive(bContext, ConversationGroup, "42", "B input"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetCache(aContext, "memory", "A"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetCache(bContext, "memory", "B"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ctx  EventContext
		want string
	}{{aContext, "A"}, {bContext, "B"}} {
		got, ok, err := runtime.GetCache(tc.ctx, "memory")
		if err != nil || !ok || got != tc.want {
			t.Fatalf("cache %s=%q,%v,%v", tc.ctx.AccountID, got, ok, err)
		}
	}
	aHistory, err := runtime.History(aContext, ConversationGroup, "42")
	if err != nil || len(aHistory) != 2 || aHistory[0].Text != "A input" || aHistory[1].Text != "A answer" {
		t.Fatalf("A history=%#v err=%v", aHistory, err)
	}
	bHistory, err := runtime.History(bContext, ConversationGroup, "42")
	if err != nil || len(bHistory) != 1 || bHistory[0].Text != "B input" {
		t.Fatalf("B history=%#v err=%v", bHistory, err)
	}
	if _, err := New([]Binding{a, func() Binding { duplicate := b; duplicate.CharacterDBID = a.CharacterDBID; return duplicate }()}); !hasCode(err, ErrDuplicateScope) {
		t.Fatalf("shared Character DB accepted: %v", err)
	}
	wrong := aContext
	wrong.PersonaID = b.PersonaID
	if err := runtime.Receive(wrong, ConversationGroup, "42", "wrong persona"); !hasCode(err, ErrContextMismatch) {
		t.Fatalf("cross-persona event=%v", err)
	}
}

func TestManagedBotsNoReplyLoop(t *testing.T) {
	runtime, a, _ := newTestRuntime(t)
	for _, sender := range []string{a.QQSelfID, "10002"} {
		if err := runtime.Receive(event(a, sender), ConversationGroup, "42", "bot message"); !hasCode(err, ErrManagedBotEvent) {
			t.Fatalf("managed sender %s accepted: %v", sender, err)
		}
	}
	history, err := runtime.History(event(a, "human"), ConversationGroup, "42")
	if err != nil || len(history) != 0 {
		t.Fatalf("managed events persisted: %#v err=%v", history, err)
	}
}

func TestAllEnabledSendPathsGuarded(t *testing.T) {
	a, b := testBindings()
	gate := &permitGate{reject: true}
	runtime, err := NewWithChatGate([]Binding{a, b}, gate)
	if err != nil {
		t.Fatal(err)
	}
	context := event(a, "human")
	if err := runtime.Receive(context, ConversationPrivate, "20001", "question"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SendText(context, a.AdapterInstanceID, "permit-rejected", ConversationPrivate, "20001", "answer"); err == nil {
		t.Fatal("rejected permit reached adapter")
	}
	if got := runtime.AdapterCalls(); len(got) != 0 {
		t.Fatalf("adapter calls after rejected permit=%#v", got)
	}
	gate.reject = false
	if err := runtime.SendText(context, b.AdapterInstanceID, "permit-cross-account", ConversationPrivate, "20001", "answer"); !hasCode(err, ErrContextMismatch) {
		t.Fatalf("cross-account adapter route=%v", err)
	}
	if err := runtime.SendText(context, a.AdapterInstanceID, "permit-ok", ConversationPrivate, "20001", "answer"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SendText(context, a.AdapterInstanceID, "", ConversationPrivate, "20001", "bypass"); !hasCode(err, ErrInvalidBinding) {
		t.Fatalf("missing permit=%v", err)
	}
	if got := runtime.AdapterCalls(); len(got) != 1 || got[0].Text != "answer" {
		t.Fatalf("adapter calls=%#v", got)
	}
	if gate.calls != 2 {
		t.Fatalf("gate calls=%d, want failed+successful permit only", gate.calls)
	}
}

type permitGate struct {
	mu     sync.Mutex
	reject bool
	calls  int
}

func (g *permitGate) SendText(_ EventContext, permitID string, kind ConversationKind, targetID, text string) (GatewayReceipt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if permitID == "" || (kind != ConversationGroup && kind != ConversationPrivate) || targetID == "" || text == "" {
		return GatewayReceipt{}, errors.New("permit request is incomplete")
	}
	if g.reject {
		return GatewayReceipt{}, errors.New("permit rejected")
	}
	return GatewayReceipt{State:"sent", MessageID:"model-receipt"}, nil
}

func TestTaskAndRetryContextCannotCrossAccount(t *testing.T) {
	runtime, a, b := newTestRuntime(t)
	aContext, bContext := event(a, "human-a"), event(b, "human-b")
	if err := runtime.StartTask(aContext, "task-1", "payload"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetRetry(aContext, "task-1", "retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ConsumeRetry(bContext, "task-1"); !hasCode(err, ErrContextMismatch) {
		t.Fatalf("cross-account retry=%v", err)
	}
	if retry, err := runtime.ConsumeRetry(aContext, "task-1"); err != nil || retry != "retry" {
		t.Fatalf("owner retry=%q err=%v", retry, err)
	}
}

func TestConversationKindAndBindingResourcesStayDistinct(t *testing.T) {
	a, b := testBindings()
	runtime, err := New([]Binding{a, b})
	if err != nil {
		t.Fatal(err)
	}
	context := event(a, "human")
	if err := runtime.Receive(context, ConversationGroup, "42", "group"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Receive(context, ConversationPrivate, "42", "private"); err != nil {
		t.Fatal(err)
	}
	group, _ := runtime.History(context, ConversationGroup, "42")
	private, _ := runtime.History(context, ConversationPrivate, "42")
	if len(group) != 1 || group[0].Text != "group" || len(private) != 1 || private[0].Text != "private" {
		t.Fatalf("history group=%#v private=%#v", group, private)
	}
	duplicate := b
	duplicate.QQSelfID = a.QQSelfID
	if _, err := New([]Binding{a, duplicate}); !hasCode(err, ErrDuplicateScope) {
		t.Fatalf("duplicate managed self-id=%v", err)
	}
	wrong := context
	wrong.RuntimeID = b.RuntimeID
	if _, err := runtime.History(wrong, ConversationGroup, "42"); !hasCode(err, ErrContextMismatch) {
		t.Fatalf("cross-runtime lookup=%v", err)
	}
}
