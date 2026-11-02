package identity

import (
	"context"
	"testing"
	"time"
)

type blockingReadback struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingReadback) ReadLoginInfo(ctx context.Context, _ Binding) (LoginInfo, error) {
	close(r.started)
	select {
	case <-r.release:
		return LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}, nil
	case <-ctx.Done():
		return LoginInfo{}, ctx.Err()
	}
}

func TestRuntimeRestartRejectsInFlightReadback(t *testing.T) {
	for _, writer := range []bool{false, true} {
		name := "verify"
		if writer {
			name = "writer"
		}
		t.Run(name, func(t *testing.T) {
			registry, _ := openTestRegistry(t)
			binding := bindingFor("a")
			verifyTestBinding(t, registry, binding, "10001")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reader := &blockingReadback{started: make(chan struct{}), release: make(chan struct{})}
			result := make(chan error, 1)
			go func() {
				if writer {
					invocation := WriterInvocation{AccountID: binding.AccountID, ActionFamily: "message.send", RuntimeID: binding.RuntimeID, AdapterID: binding.AdapterID, AdapterInstanceID: binding.AdapterInstanceID, ExpectedSelfID: "10001"}
					result <- registry.CheckWriter(ctx, invocation, reader, time.Now())
					return
				}
				result <- registry.Verify(ctx, binding.AccountID, binding.RuntimeID, reader, time.Now())
			}()
			select {
			case <-reader.started:
			case <-ctx.Done():
				t.Fatal("self-info read-back never started")
			}
			if err := registry.ActivateRuntime(binding.RuntimeID); err != nil {
				t.Fatal(err)
			}
			fresh := &fakeReader{info: LoginInfo{SelfID: "10001", Source: SourceGetLoginInfo}}
			if writer {
				if err := registry.Verify(ctx, binding.AccountID, binding.RuntimeID, fresh, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			close(reader.release)
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("old runtime read-back authorized the new generation")
				}
			case <-ctx.Done():
				t.Fatal("guard did not return after read-back")
			}
			state, err := registry.GetByAccount(binding.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if !writer && state.Status != StatusStale {
				t.Fatalf("old read-back verified restarted runtime: %+v", state)
			}
		})
	}
}

func TestAccountIdentityIsolation(t *testing.T) {
	registry, _ := openTestRegistry(t)
	a, b := bindingFor("a"), bindingFor("b")
	verifyTestBinding(t, registry, a, "10001")
	verifyTestBinding(t, registry, b, "10002")
	reader := &fakeReader{info: LoginInfo{SelfID: "10002", Source: SourceGetLoginInfo}}
	invocation := WriterInvocation{AccountID: b.AccountID, ActionFamily: "message.send", RuntimeID: b.RuntimeID, AdapterID: b.AdapterID, AdapterInstanceID: b.AdapterInstanceID, ExpectedSelfID: "10002"}
	registry.DeactivateRuntime(a.RuntimeID)
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); err != nil {
		t.Fatalf("deactivating A disabled B: %v", err)
	}
	if err := registry.ActivateRuntime(a.RuntimeID); err != nil {
		t.Fatal(err)
	}
	aState, err := registry.GetByAccount(a.AccountID)
	if err != nil || aState.Status != StatusStale {
		t.Fatalf("A restart status = %+v, %v", aState, err)
	}
	bState, err := registry.GetByQQSelfID("10002")
	if err != nil || bState.AccountID != b.AccountID || bState.Status != StatusVerified {
		t.Fatalf("A restart changed B identity = %+v, %v", bState, err)
	}
	crossed := invocation
	crossed.AccountID = a.AccountID
	if err := registry.CheckWriter(context.Background(), crossed, reader, time.Now()); err == nil {
		t.Fatal("B connection authorized an A invocation")
	}
	if err := registry.CheckWriter(context.Background(), invocation, reader, time.Now()); err != nil {
		t.Fatalf("rejecting crossed account disabled B: %v", err)
	}
}
