// Package infrastructure provides safe default adapters for external OIDC browser login.
package infrastructure

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
)

var errStateUnavailable = errors.New("external login state is unavailable")

// MemoryStateStore is a process-local, concurrency-safe state store suitable for a single-instance
// deployment or development. Multi-instance production deployments must inject a durable store with
// the same atomic consume semantics.
type MemoryStateStore struct {
	mutex  sync.Mutex
	states map[[32]byte]domain.State
}

// NewMemoryStateStore constructs an empty one-time state store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{states: make(map[[32]byte]domain.State)}
}

// Save stores one authorization attempt. Existing values for the same cryptographic hash are never
// overwritten, even though collisions are cryptographically infeasible.
func (store *MemoryStateStore) Save(_ context.Context, state domain.State) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if _, exists := store.states[state.StateHash]; exists {
		return errStateUnavailable
	}
	store.states[state.StateHash] = state
	return nil
}

// Consume removes the state before returning it, ensuring an authorization callback cannot be
// replayed when token validation or session issuance later fails.
func (store *MemoryStateStore) Consume(_ context.Context, hash [32]byte, now time.Time) (domain.State, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	state, exists := store.states[hash]
	if !exists {
		return domain.State{}, errStateUnavailable
	}
	delete(store.states, hash)
	if !state.ExpiresAt.After(now.UTC()) {
		return domain.State{}, errStateUnavailable
	}
	return state, nil
}
