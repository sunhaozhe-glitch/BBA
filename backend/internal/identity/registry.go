package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

type Status string

const (
	StatusUnverified   Status = "unverified"
	StatusVerified     Status = "verified"
	StatusStale        Status = "stale"
	StatusDrift        Status = "drift"
	SourceGetLoginInfo        = "napcat_get_login_info"
)

type ErrorCode string

const (
	ErrDuplicateSelfID     ErrorCode = "duplicate_self_id"
	ErrDuplicateRuntime    ErrorCode = "duplicate_runtime"
	ErrIdentityUnverified  ErrorCode = "identity_unverified"
	ErrIdentityDrift       ErrorCode = "identity_drift"
	ErrRuntimeUnavailable  ErrorCode = "runtime_unavailable"
	ErrAccountExists       ErrorCode = "account_exists"
	ErrIsolationConflict   ErrorCode = "character_isolation_conflict"
	ErrInvalidBinding      ErrorCode = "invalid_binding"
	ErrRegistryUnavailable ErrorCode = "registry_unavailable"
)

type RegistryError struct {
	Code ErrorCode
	Err  error
}

func (e *RegistryError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Err.Error()
}

func (e *RegistryError) Unwrap() error { return e.Err }

func hasCode(err error, code ErrorCode) bool {
	var registryErr *RegistryError
	return errors.As(err, &registryErr) && registryErr.Code == code
}

type Binding struct {
	AccountID               string `json:"account_id"`
	RuntimeID               string `json:"runtime_id"`
	AdapterID               string `json:"adapter_id"`
	AdapterInstanceID       string `json:"adapter_instance_id"`
	NapCatInstanceID        string `json:"napcat_instance_id"`
	CharacterRuntimeID      string `json:"character_runtime_id"`
	CharacterDBID           string `json:"character_db_id"`
	CharacterCacheNamespace string `json:"character_cache_namespace"`
	CapabilityProfileID     string `json:"capability_profile_id"`
}

type Identity struct {
	Binding
	QQSelfID           *string `json:"qq_self_id"`
	Status             Status  `json:"status"`
	IdentityVerifiedAt *string `json:"identity_verified_at"`
	IdentitySource     *string `json:"identity_source"`
	RuntimeGeneration  uint64  `json:"runtime_generation"`
}

// identityRecord keeps read-back audit evidence out of the public contract projection.
type identityRecord struct {
	Identity
	LastReadbackSelfID string `json:"last_readback_self_id,omitempty"`
	LastReadbackAt     string `json:"last_readback_at,omitempty"`
	LastReadbackSource string `json:"last_readback_source,omitempty"`
}

type LoginInfo struct {
	SelfID string
	Source string
}

type SelfInfoReader interface {
	ReadLoginInfo(context.Context, Binding) (LoginInfo, error)
}

type WriterInvocation struct {
	AccountID         string
	ActionFamily      string
	RuntimeID         string
	AdapterID         string
	AdapterInstanceID string
	ExpectedSelfID    string
}

type diskState struct {
	Version    int                       `json:"version"`
	Generation uint64                    `json:"generation"`
	Identities map[string]identityRecord `json:"identities"`
}

type Registry struct {
	path       string
	generation uint64
	mu         sync.Mutex
	active     map[string]uint64
}

// Open advances the durable registry generation and invalidates prior runtime liveness.
func Open(path string) (*Registry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, &RegistryError{Code: ErrRegistryUnavailable, Err: errors.New("empty registry path")}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, &RegistryError{Code: ErrRegistryUnavailable, Err: err}
	}
	var generation uint64
	if err := withDiskLock(path, func(state *diskState) (bool, error) {
		state.Generation++
		generation = state.Generation
		if state.Generation == 1 {
			state.Version = 1
		}
		for id, identity := range state.Identities {
			if identity.Status == StatusVerified {
				identity.Status = StatusStale
			}
			identity.RuntimeGeneration++
			state.Identities[id] = identity
		}
		return true, nil
	}); err != nil {
		return nil, &RegistryError{Code: ErrRegistryUnavailable, Err: err}
	}
	return &Registry{path: path, generation: generation, active: make(map[string]uint64)}, nil
}

func (r *Registry) Register(binding Binding) error {
	if err := validateBinding(binding); err != nil {
		return err
	}
	return r.update(func(state *diskState) (bool, error) {
		if _, exists := state.Identities[binding.AccountID]; exists {
			return false, &RegistryError{Code: ErrAccountExists}
		}
		for _, existing := range state.Identities {
			if existing.RuntimeID == binding.RuntimeID || existing.AdapterInstanceID == binding.AdapterInstanceID || existing.NapCatInstanceID == binding.NapCatInstanceID {
				return false, &RegistryError{Code: ErrDuplicateRuntime}
			}
			if existing.CharacterRuntimeID == binding.CharacterRuntimeID || existing.CharacterDBID == binding.CharacterDBID || existing.CharacterCacheNamespace == binding.CharacterCacheNamespace {
				return false, &RegistryError{Code: ErrIsolationConflict}
			}
		}
		state.Identities[binding.AccountID] = identityRecord{Identity: Identity{Binding: binding, Status: StatusUnverified, RuntimeGeneration: state.Generation}}
		return true, nil
	})
}

func (r *Registry) ActivateRuntime(runtimeID string) error {
	if strings.TrimSpace(runtimeID) == "" {
		return &RegistryError{Code: ErrRuntimeUnavailable}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var generation uint64
	if err := r.updateLocked(func(state *diskState) (bool, error) {
		for accountID, identity := range state.Identities {
			if identity.RuntimeID != runtimeID {
				continue
			}
			identity.RuntimeGeneration++
			if identity.Status == StatusVerified {
				identity.Status = StatusStale
			}
			generation = identity.RuntimeGeneration
			state.Identities[accountID] = identity
			return true, nil
		}
		return false, &RegistryError{Code: ErrRuntimeUnavailable}
	}); err != nil {
		return err
	}
	r.active[runtimeID] = generation
	return nil
}

func (r *Registry) DeactivateRuntime(runtimeID string) {
	r.mu.Lock()
	delete(r.active, runtimeID)
	r.mu.Unlock()
}

func (r *Registry) Verify(ctx context.Context, accountID, runtimeID string, reader SelfInfoReader, observedAt time.Time) error {
	if reader == nil {
		return &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("nil self-info reader")}
	}
	identity, err := r.GetByAccount(accountID)
	if err != nil {
		return err
	}
	if identity.RuntimeID != runtimeID {
		return &RegistryError{Code: ErrIdentityDrift, Err: errors.New("runtime/account binding mismatch")}
	}
	if identity.RuntimeGeneration != r.activeGeneration(runtimeID) {
		return &RegistryError{Code: ErrRuntimeUnavailable}
	}
	if identity.Status == StatusDrift {
		return &RegistryError{Code: ErrIdentityDrift, Err: errors.New("binding requires explicit reconciliation")}
	}
	info, err := reader.ReadLoginInfo(ctx, identity.Binding)
	if err != nil {
		return &RegistryError{Code: ErrRuntimeUnavailable, Err: err}
	}
	return r.recordReadback(accountID, runtimeID, identity.RuntimeGeneration, info, observedAt)
}

func (r *Registry) recordReadback(accountID, runtimeID string, generation uint64, info LoginInfo, observedAt time.Time) error {
	if !validQQID(info.SelfID) || info.Source != SourceGetLoginInfo || observedAt.IsZero() {
		return &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("incomplete or unsupported self-info read-back")}
	}
	stamp := observedAt.UTC().Format(time.RFC3339Nano)
	return r.update(func(state *diskState) (bool, error) {
		identity, ok := state.Identities[accountID]
		if !ok {
			return false, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("account is not registered")}
		}
		if identity.RuntimeID != runtimeID || identity.RuntimeGeneration != generation || generation != r.active[runtimeID] || state.Generation != r.generation {
			return false, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("runtime generation changed or runtime is inactive")}
		}
		if identity.Status == StatusDrift {
			return false, &RegistryError{Code: ErrIdentityDrift}
		}
		identity.LastReadbackSelfID = info.SelfID
		identity.LastReadbackAt = stamp
		identity.LastReadbackSource = info.Source
		if identity.QQSelfID != nil && *identity.QQSelfID != info.SelfID {
			identity.Status = StatusDrift
			state.Identities[accountID] = identity
			return true, &RegistryError{Code: ErrIdentityDrift, Err: errors.New("runtime self-id differs from registered identity")}
		}
		for otherAccount, other := range state.Identities {
			if otherAccount != accountID && other.QQSelfID != nil && *other.QQSelfID == info.SelfID {
				identity.Status = StatusDrift
				state.Identities[accountID] = identity
				return true, &RegistryError{Code: ErrDuplicateSelfID, Err: errors.New("self-id already belongs to another account")}
			}
		}
		selfID, verifiedAt, source := info.SelfID, stamp, info.Source
		identity.QQSelfID = &selfID
		identity.IdentityVerifiedAt = &verifiedAt
		identity.IdentitySource = &source
		identity.Status = StatusVerified
		state.Identities[accountID] = identity
		return true, nil
	})
}

func (r *Registry) CheckWriter(ctx context.Context, invocation WriterInvocation, reader SelfInfoReader, observedAt time.Time) error {
	if strings.TrimSpace(invocation.ActionFamily) == "" || !validQQID(invocation.ExpectedSelfID) {
		return &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("action family and expected self-id are required")}
	}
	identity, err := r.GetByAccount(invocation.AccountID)
	if err != nil {
		return err
	}
	if !r.isActive(invocation.RuntimeID) {
		return &RegistryError{Code: ErrRuntimeUnavailable}
	}
	if identity.Status != StatusVerified || identity.RuntimeGeneration != r.activeGeneration(invocation.RuntimeID) || identity.QQSelfID == nil {
		return &RegistryError{Code: ErrIdentityUnverified}
	}
	if identity.RuntimeID != invocation.RuntimeID || identity.AdapterID != invocation.AdapterID || identity.AdapterInstanceID != invocation.AdapterInstanceID || *identity.QQSelfID != invocation.ExpectedSelfID {
		return &RegistryError{Code: ErrIdentityDrift, Err: errors.New("writer invocation does not match account binding")}
	}
	if reader == nil {
		return &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("nil self-info reader")}
	}
	info, err := reader.ReadLoginInfo(ctx, identity.Binding)
	if err != nil {
		return &RegistryError{Code: ErrRuntimeUnavailable, Err: err}
	}
	return r.recordReadback(invocation.AccountID, invocation.RuntimeID, identity.RuntimeGeneration, info, observedAt)
}

func (r *Registry) GetByAccount(accountID string) (Identity, error) {
	var result Identity
	err := r.read(func(state *diskState) error {
		identity, ok := state.Identities[accountID]
		if !ok {
			return &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("account is not registered")}
		}
		result = identity.Identity
		return nil
	})
	return result, err
}

func (r *Registry) GetByQQSelfID(selfID string) (Identity, error) {
	var result Identity
	err := r.read(func(state *diskState) error {
		for _, identity := range state.Identities {
			if identity.QQSelfID != nil && *identity.QQSelfID == selfID && selfID != "" {
				result = identity.Identity
				return nil
			}
		}
		return &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-id is not bound")}
	})
	return result, err
}

func (r *Registry) List() ([]Identity, error) {
	result := make([]Identity, 0)
	err := r.read(func(state *diskState) error {
		for _, identity := range state.Identities {
			result = append(result, identity.Identity)
		}
		return nil
	})
	return result, err
}

func (r *Registry) isActive(runtimeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[runtimeID] != 0
}

func (r *Registry) activeGeneration(runtimeID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[runtimeID]
}

func (r *Registry) update(fn func(*diskState) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updateLocked(fn)
}

// updateLocked keeps durable state and process-local liveness under one mutex.
func (r *Registry) updateLocked(fn func(*diskState) (bool, error)) error {
	return withDiskLock(r.path, func(state *diskState) (bool, error) {
		if state.Generation != r.generation {
			return false, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("registry generation changed; reopen required")}
		}
		return fn(state)
	})
}

func (r *Registry) read(fn func(*diskState) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return withDiskLock(r.path, func(state *diskState) (bool, error) {
		if state.Generation != r.generation {
			return false, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("registry generation changed; reopen required")}
		}
		return false, fn(state)
	})
}

func withDiskLock(path string, fn func(*diskState) (bool, error)) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state, err := readStateFile(path)
	if err != nil {
		return err
	}
	changed, operationErr := fn(&state)
	if changed {
		if err := writeStateFile(path, state); err != nil {
			return err
		}
	}
	return operationErr
}

func readStateFile(path string) (diskState, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return diskState{Version: 1, Identities: make(map[string]identityRecord)}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	decoder.DisallowUnknownFields()
	var state diskState
	if err := decoder.Decode(&state); err != nil {
		return diskState{}, fmt.Errorf("decode registry: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return diskState{}, errors.New("registry contains trailing content")
	}
	if state.Version != 1 || state.Generation == 0 || state.Identities == nil {
		return diskState{}, errors.New("invalid registry header")
	}
	return state, nil
}

func writeStateFile(path string, state diskState) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".identity-registry-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateBinding(binding Binding) error {
	for name, value := range map[string]string{
		"account_id": binding.AccountID, "runtime_id": binding.RuntimeID,
		"adapter_id": binding.AdapterID, "adapter_instance_id": binding.AdapterInstanceID,
		"napcat_instance_id": binding.NapCatInstanceID, "character_runtime_id": binding.CharacterRuntimeID,
		"character_db_id": binding.CharacterDBID, "character_cache_namespace": binding.CharacterCacheNamespace,
		"capability_profile_id": binding.CapabilityProfileID,
	} {
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 128 {
			return &RegistryError{Code: ErrInvalidBinding, Err: fmt.Errorf("%s must be nonblank UTF-8 with at most 128 characters", name)}
		}
	}
	return nil
}

func validQQID(value string) bool {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
