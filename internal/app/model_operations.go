package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
	"math"
	"sync"
	"time"
)

var errModelOperation = errors.New("model_operation_unavailable")

type modelOperations struct {
	mu       sync.Mutex
	stopping bool
	active   string
	cancel   context.CancelFunc
	done     chan struct{}
	states   map[string]localui.ModelState
	terminal []localui.ModelOperation
}

func (m *modelOperations) stop(ctx context.Context) error {
	m.mu.Lock()
	m.stopping = true
	if m.cancel != nil {
		m.cancel()
	}
	done := m.done
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (a *Application) ModelState(ctx context.Context) (localui.ModelStateSnapshot, error) {
	profiles, err := a.Models(ctx)
	if err != nil {
		return localui.ModelStateSnapshot{}, err
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil {
		return localui.ModelStateSnapshot{}, errModelOperation
	}
	m := &a.resources.modelOperations
	m.mu.Lock()
	defer m.mu.Unlock()
	result := localui.ModelStateSnapshot{Items: make([]localui.ModelState, 0, len(profiles))}
	for _, profile := range profiles {
		state := m.states[profile.ID]
		if state.ProfileID == "" {
			state = localui.ModelState{ProfileID: profile.ID, InstallationState: "unknown", Code: "not_inspected"}
		}
		if state.Operation != nil {
			operation := *state.Operation
			state.Operation = &operation
		}
		if state.VerifiedAt != nil {
			verified := *state.VerifiedAt
			state.VerifiedAt = &verified
		}
		result.Items = append(result.Items, state)
	}
	return result, nil
}
func (a *Application) StartModelOperation(ctx context.Context, request localui.ModelOperationRequest) (localui.ModelOperation, error) {
	if a.snapshot != nil {
		return localui.ModelOperation{}, archive.ErrHistoricalSnapshotReadOnly
	}
	if err := ctx.Err(); err != nil {
		return localui.ModelOperation{}, err
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil || a.resources.models == nil {
		return localui.ModelOperation{}, errModelOperation
	}
	if a.resources.recoveryPending.Load() {
		return localui.ModelOperation{}, errModelOperation
	}
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		return localui.ModelOperation{}, errModelOperation
	}
	known := false
	var total int64
	for _, profile := range catalog.Profiles {
		if profile.ID != request.ProfileID {
			continue
		}
		known = true
		for _, component := range profile.Components {
			if component.Bytes < 0 || component.Bytes > math.MaxInt64-total {
				return localui.ModelOperation{}, errModelOperation
			}
			total += component.Bytes
		}
		break
	}
	if request.Kind == "remove" {
		total = 0
	}
	if !known || (request.Kind != "install" && request.Kind != "verify" && request.Kind != "remove") {
		return localui.ModelOperation{}, errModelOperation
	}
	id, err := nonce128()
	if err != nil {
		return localui.ModelOperation{}, errModelOperation
	}
	m := &a.resources.modelOperations
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return localui.ModelOperation{}, errModelOperation
	}
	if m.active != "" {
		return localui.ModelOperation{}, localui.ErrModelOperationBusy
	}
	operation := localui.ModelOperation{ID: id, ProfileID: request.ProfileID, Kind: request.Kind, State: "queued", Phase: "queued", Code: "queued", TotalBytes: total}
	// Detach from the short HTTP request, while retaining app/resource ownership.
	work, cancel := context.WithCancel(a.queryContext)
	m.active = id
	m.cancel = cancel
	m.done = make(chan struct{})
	if m.states == nil {
		m.states = make(map[string]localui.ModelState)
	}
	state := m.states[request.ProfileID]
	state.ProfileID = request.ProfileID
	if state.InstallationState == "" {
		state.InstallationState = "unknown"
	}
	state.Operation = &operation
	m.states[request.ProfileID] = state
	store := a.resources.models
	go m.run(work, request, operation, store)
	return operation, nil
}
func (a *Application) CancelModelOperation(ctx context.Context, id string) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil {
		return errModelOperation
	}
	m := &a.resources.modelOperations
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != id || m.cancel == nil {
		return errModelOperation
	}
	m.cancel()
	return nil
}
func (m *modelOperations) run(ctx context.Context, request localui.ModelOperationRequest, operation localui.ModelOperation, store *modelstore.Store) {
	m.update(request.ProfileID, func(state *localui.ModelState) {
		operation.State = "running"
		operation.Phase = request.Kind
		operation.Code = "running"
		state.Operation = &operation
	})
	progress := func(value modelstore.Progress) {
		m.update(request.ProfileID, func(state *localui.ModelState) {
			updated := *state.Operation
			switch value.Phase {
			case "download", "downloading", "verify", "verifying", "activate", "activating", "installed", "complete":
				updated.Phase = value.Phase
			default:
				updated.Phase = request.Kind
			}
			if value.CompletedBytes >= 0 {
				updated.CompletedBytes = value.CompletedBytes
			}
			state.Operation = &updated
		})
	}
	var err error
	switch request.Kind {
	case "install":
		err = store.Install(ctx, request.ProfileID, progress)
	case "verify":
		_, err = store.Verify(ctx, request.ProfileID)
	case "remove":
		err = store.Remove(ctx, request.ProfileID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[request.ProfileID]
	operation = *state.Operation
	if err == nil {
		operation.State = "completed"
		operation.Code = "model_verified"
		operation.Phase = "complete"
		operation.CompletedBytes = operation.TotalBytes
		state.Code = operation.Code
		switch request.Kind {
		case "install":
			state.InstallationState = "present"
			state.VerifiedAt = nil
		case "verify":
			state.InstallationState = "verified"
			now := time.Now().UTC()
			state.VerifiedAt = &now
		case "remove":
			operation.Code = "model_removed"
			state.Code = operation.Code
			state.InstallationState = "absent"
			state.VerifiedAt = nil
		}
	} else {
		operation.State = "failed"
		operation.Code = modelOperationCode(err)
		if ctx.Err() != nil {
			operation.State = "cancelled"
			operation.Code = "model_operation_cancelled"
		}
		var space *modelstore.SpaceError
		if errors.As(err, &space) {
			operation.Code = "model_space_required"
			operation.AdditionalBytes = space.AdditionalBytes
		}
		state.Code = operation.Code
		invalidateModelVerification(&state, err)
	}
	state.Operation = &operation
	m.states[request.ProfileID] = state
	m.terminal = append(m.terminal, operation)
	if len(m.terminal) > 32 {
		m.terminal = append([]localui.ModelOperation(nil), m.terminal[len(m.terminal)-32:]...)
	}
	m.active = ""
	if m.cancel != nil {
		m.cancel()
	}
	m.cancel = nil
	close(m.done)
	m.done = nil
}
func (m *modelOperations) update(profile string, use func(*localui.ModelState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[profile]
	use(&state)
	m.states[profile] = state
}

var _ localui.ModelApplication = (*Application)(nil)

// Map only fixed public codes; native URLs, filesystem paths and raw errors
// never cross the operation snapshot.
func modelOperationCode(err error) string {
	switch {
	case errors.Is(err, modelstore.ErrNotInstalled):
		return "model_not_installed"
	case errors.Is(err, modelstore.ErrHashMismatch):
		return "model_hash_mismatch"
	case errors.Is(err, modelstore.ErrSizeMismatch):
		return "model_size_mismatch"
	case errors.Is(err, modelstore.ErrPlatform):
		return "model_platform_unavailable"
	case errors.Is(err, modelstore.ErrOfflineOnly):
		return "model_offline_required"
	case errors.Is(err, modelstore.ErrActiveLease):
		return "model_active"
	case errors.Is(err, diskspace.ErrInsufficientSpace):
		return "model_space_required"
	default:
		return "model_operation_failed"
	}
}

func invalidateModelVerification(state *localui.ModelState, err error) {
	state.VerifiedAt = nil
	state.InstallationState = "unknown"
	if errors.Is(err, modelstore.ErrNotInstalled) {
		state.InstallationState = "absent"
	}
	if errors.Is(err, modelstore.ErrPlatform) {
		state.InstallationState = "unavailable"
	}
}
