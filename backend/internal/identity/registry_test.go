package identity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeReader struct {
	info  LoginInfo
	err   error
	calls int
}

func (f *fakeReader) ReadLoginInfo(_ context.Context, _ Binding) (LoginInfo, error) {
	f.calls++
	return f.info, f.err
}

func bindingFor(suffix string) Binding {
	return Binding{
		AccountID: "acc_" + suffix, RuntimeID: "runtime_" + suffix,
		AdapterID: "adapter", AdapterInstanceID: "adapter_instance_" + suffix,
		NapCatInstanceID: "napcat_" + suffix, CharacterRuntimeID: "character_runtime_" + suffix,
		CharacterDBID: "character_db_" + suffix, CharacterCacheNamespace: "character_cache_" + suffix,
		CapabilityProfileID: "profile_" + suffix,
	}
}

func openTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.json")
	registry, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return registry, path
}

func verifyTestBinding(t *testing.T, registry *Registry, binding Binding, selfID string) *fakeReader {
	t.Helper()
	if err := registry.Register(binding); err != nil {
		t.Fatal(err)
	}
	if err := registry.ActivateRuntime(binding.RuntimeID); err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{info: LoginInfo{SelfID: selfID, Source: SourceGetLoginInfo}}
	if err := registry.Verify(context.Background(), binding.AccountID, binding.RuntimeID, reader, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestRegistryPersistsBidirectionalBindingAndRejectsDuplicateRuntime(t *testing.T) {
	registry, path := openTestRegistry(t)
	binding := bindingFor("a")
	verifyTestBinding(t, registry, binding, "10001")
	byAccount, err := registry.GetByAccount(binding.AccountID)
	if err != nil || byAccount.QQSelfID == nil || *byAccount.QQSelfID != "10001" || byAccount.Status != StatusVerified {
		t.Fatalf("account read-back = %+v, %v", byAccount, err)
	}
	bySelfID, err := registry.GetByQQSelfID("10001")
	if err != nil || bySelfID.AccountID != binding.AccountID {
		t.Fatalf("self-id lookup = %+v, %v", bySelfID, err)
	}
	duplicateRuntime := bindingFor("b")
	duplicateRuntime.RuntimeID = binding.RuntimeID
	if err := registry.Register(duplicateRuntime); !hasCode(err, ErrDuplicateRuntime) {
		t.Fatalf("duplicate runtime error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := reopened.GetByAccount(binding.AccountID)
	if err != nil || stale.Status != StatusStale {
		t.Fatalf("restart status = %+v, %v", stale, err)
	}
	if err := reopened.ActivateRuntime(binding.RuntimeID); err != nil {
		t.Fatal(err)
	}
	invocation := WriterInvocation{AccountID: binding.AccountID, ActionFamily: "message.send", RuntimeID: binding.RuntimeID, AdapterID: binding.AdapterID, AdapterInstanceID: binding.AdapterInstanceID, ExpectedSelfID: "10001"}
	if err := reopened.CheckWriter(context.Background(), invocation, &fakeReader{info: LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}}, time.Now()); !hasCode(err, ErrIdentityUnverified) {
		t.Fatalf("writer after restart error = %v", err)
	}
}

func TestDuplicateSelfIDAndIdentityDriftFailClosed(t *testing.T) {
	registry, _ := openTestRegistry(t)
	first := bindingFor("a")
	verifyTestBinding(t, registry, first, "10001")
	second := bindingFor("b")
	if err := registry.Register(second); err != nil {
		t.Fatal(err)
	}
	if err := registry.ActivateRuntime(second.RuntimeID); err != nil {
		t.Fatal(err)
	}
	reader := &fakeReader{info: LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}}
	if err := registry.Verify(context.Background(), second.AccountID, second.RuntimeID, reader, time.Now()); !hasCode(err, ErrDuplicateSelfID) {
		t.Fatalf("duplicate self-id error = %v", err)
	}
	secondState, err := registry.GetByAccount(second.AccountID)
	if err != nil || secondState.Status != StatusDrift || secondState.IdentityVerifiedAt != nil {
		t.Fatalf("duplicate self-id state = %+v, %v", secondState, err)
	}

	driftRegistry, _ := openTestRegistry(t)
	driftBinding := bindingFor("drift")
	verifyTestBinding(t, driftRegistry, driftBinding, "10002")
	driftReader := &fakeReader{info: LoginInfo{SelfID: "10003", Source: SourceGetLoginInfo}}
	invocation := WriterInvocation{AccountID: driftBinding.AccountID, ActionFamily: "send", RuntimeID: driftBinding.RuntimeID, AdapterID: driftBinding.AdapterID, AdapterInstanceID: driftBinding.AdapterInstanceID, ExpectedSelfID: "10002"}
	if err := driftRegistry.CheckWriter(context.Background(), invocation, driftReader, time.Now()); !hasCode(err, ErrIdentityDrift) {
		t.Fatalf("drift guard error = %v", err)
	}
	driftState, err := driftRegistry.GetByAccount(driftBinding.AccountID)
	if err != nil || driftState.Status != StatusDrift {
		t.Fatalf("persisted drift = %+v, %v", driftState, err)
	}
}

func TestWriterGuardRequiresVerifiedActiveExactBindingAndFreshReadback(t *testing.T) {
	registry, _ := openTestRegistry(t)
	binding := bindingFor("a")
	if err := registry.Register(binding); err != nil {
		t.Fatal(err)
	}
	if err := registry.ActivateRuntime(binding.RuntimeID); err != nil {
		t.Fatal(err)
	}
	invocation := WriterInvocation{AccountID: binding.AccountID, ActionFamily: "message.send", RuntimeID: binding.RuntimeID, AdapterID: binding.AdapterID, AdapterInstanceID: binding.AdapterInstanceID, ExpectedSelfID: "10001"}
	reader := &fakeReader{info: LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}}
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); !hasCode(err, ErrIdentityUnverified) {
		t.Fatalf("unverified writer error = %v", err)
	}
	if err := registry.Verify(context.Background(), binding.AccountID, binding.RuntimeID, &fakeReader{info: LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}}, time.Now()); err != nil {
		t.Fatalf("verify initial binding: %v", err)
	}
	reader = &fakeReader{info: LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}}
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); err != nil {
		t.Fatalf("valid guard error = %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("fresh read-back calls = %d", reader.calls)
	}

	wrong := invocation
	wrong.RuntimeID = "runtime_other"
	if err := registry.CheckWriter(context.Background(), wrong, reader, time.Now()); !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("wrong runtime error = %v", err)
	}
	wrong = invocation
	wrong.AdapterInstanceID = "adapter_instance_other"
	if err := registry.CheckWriter(context.Background(), wrong, reader, time.Now()); !hasCode(err, ErrIdentityDrift) {
		t.Fatalf("wrong adapter instance error = %v", err)
	}
	wrong = invocation
	wrong.ExpectedSelfID = "10009"
	if err := registry.CheckWriter(context.Background(), wrong, reader, time.Now()); !hasCode(err, ErrIdentityDrift) {
		t.Fatalf("wrong self-id error = %v", err)
	}
	registry.DeactivateRuntime(binding.RuntimeID)
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("inactive runtime error = %v", err)
	}
}

func TestRuntimeRestartRequiresFreshIdentityReadback(t *testing.T) {
	registry, _ := openTestRegistry(t)
	binding := bindingFor("restart")
	verifyTestBinding(t, registry, binding, "10011")
	registry.DeactivateRuntime(binding.RuntimeID)
	if err := registry.ActivateRuntime(binding.RuntimeID); err != nil {
		t.Fatal(err)
	}
	stale, err := registry.GetByAccount(binding.AccountID)
	if err != nil || stale.Status != StatusStale {
		t.Fatalf("runtime restart status = %+v, %v", stale, err)
	}
	invocation := WriterInvocation{AccountID: binding.AccountID, ActionFamily: "message.send", RuntimeID: binding.RuntimeID, AdapterID: binding.AdapterID, AdapterInstanceID: binding.AdapterInstanceID, ExpectedSelfID: "10011"}
	reader := &fakeReader{info: LoginInfo{SelfID: "10011", Source: SourceGetLoginInfo}}
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); !hasCode(err, ErrIdentityUnverified) {
		t.Fatalf("writer before reverify = %v", err)
	}
	if err := registry.Verify(context.Background(), binding.AccountID, binding.RuntimeID, reader, time.Now()); err != nil {
		t.Fatalf("reverify after runtime restart: %v", err)
	}
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); err != nil {
		t.Fatalf("writer after reverify = %v", err)
	}
}

func TestRegistryAtomicUpdateAndCorruptionFailClosed(t *testing.T) {
	registry, path := openTestRegistry(t)
	if err := registry.Register(bindingFor("a")); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(bindingFor("stale")); !hasCode(err, ErrRuntimeUnavailable) {
		t.Fatalf("old instance error = %v", err)
	}
	if err := other.Register(bindingFor("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := other.GetByAccount("acc_a"); err != nil {
		t.Fatalf("first account was lost on atomic update: %v", err)
	}
	if _, err := other.GetByAccount("acc_b"); err != nil {
		t.Fatalf("second account was lost on atomic update: %v", err)
	}

	corruptPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(corruptPath, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(corruptPath); !hasCode(err, ErrRegistryUnavailable) {
		t.Fatalf("corrupt registry did not fail closed: %v", err)
	}
}

func TestBindingRequiresExplicitCharacterPartitions(t *testing.T) {
	registry, _ := openTestRegistry(t)
	first := bindingFor("a")
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	second := bindingFor("b")
	second.CharacterDBID = first.CharacterDBID
	if err := registry.Register(second); !hasCode(err, ErrIsolationConflict) {
		t.Fatalf("shared DB error = %v", err)
	}
	missing := bindingFor("c")
	missing.CharacterCacheNamespace = " "
	if err := registry.Register(missing); !hasCode(err, ErrInvalidBinding) {
		t.Fatalf("missing cache namespace error = %v", err)
	}
}

func TestBindingIDContractLengthBoundary(t *testing.T) {
	registry, _ := openTestRegistry(t)
	boundary := bindingFor("boundary")
	boundary.AccountID = strings.Repeat("界", 128)
	if err := registry.Register(boundary); err != nil {
		t.Fatalf("128-character account ID rejected: %v", err)
	}
	for _, row := range []struct {
		name string
		set  func(*Binding, string)
	}{
		{"account", func(b *Binding, value string) { b.AccountID = value }},
		{"runtime", func(b *Binding, value string) { b.RuntimeID = value }},
		{"adapter", func(b *Binding, value string) { b.AdapterID = value }},
		{"adapter-instance", func(b *Binding, value string) { b.AdapterInstanceID = value }},
		{"napcat", func(b *Binding, value string) { b.NapCatInstanceID = value }},
		{"character-runtime", func(b *Binding, value string) { b.CharacterRuntimeID = value }},
		{"character-db", func(b *Binding, value string) { b.CharacterDBID = value }},
		{"character-cache", func(b *Binding, value string) { b.CharacterCacheNamespace = value }},
		{"capability-profile", func(b *Binding, value string) { b.CapabilityProfileID = value }},
	} {
		t.Run(row.name, func(t *testing.T) {
			candidate := bindingFor(row.name)
			row.set(&candidate, strings.Repeat("界", 129))
			if err := registry.Register(candidate); !hasCode(err, ErrInvalidBinding) {
				t.Fatalf("oversized %s ID accepted: %v", row.name, err)
			}
		})
	}
	identities, err := registry.List()
	if err != nil || len(identities) != 1 || identities[0].AccountID != boundary.AccountID {
		t.Fatalf("rejected bindings mutated registry: %+v, %v", identities, err)
	}
}
