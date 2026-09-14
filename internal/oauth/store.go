package oauth

import (
	"sync"
	"time"
)

// StateStore is an in-memory, mutex-guarded registry of pending OAuth
// authorization transactions. It implements the server-side state policy
// from the plan: states are short-lived, single-use, and bound to the target
// gateway instance. It is a protocol-level component — no persistence — and
// is safe for concurrent use. A gateway restart discards all pending states,
// which the plan assumes invalidates in-flight transactions anyway.
type StateStore struct {
	mu     sync.Mutex
	states map[string]*State
	// now is the clock used for expiry; tests replace it directly.
	now func() time.Time
}

// NewStateStore returns an empty store.
func NewStateStore() *StateStore {
	return &StateStore{
		states: make(map[string]*State),
		now:    time.Now,
	}
}

// Generate creates a fresh state bound to instanceID, valid for ttl, and
// registers it. The returned *State carries the PKCE verifier the callback
// exchange needs; the caller must keep it server-side.
func (s *StateStore) Generate(instanceID string, ttl time.Duration) (*State, error) {
	return s.GenerateWithGeneration(instanceID, ttl, 0)
}

// GenerateWithGeneration creates and registers a state bound to instanceID
// and an OAuth lifecycle generation. The generation lets callback persistence
// reject a flow superseded while its provider exchange is in flight.
func (s *StateStore) GenerateWithGeneration(instanceID string, ttl time.Duration, generation uint64) (*State, error) {
	st, err := newStateAt(instanceID, ttl, s.now())
	if err != nil {
		return nil, err
	}
	st.Generation = generation
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[st.Value] = st
	return st, nil
}

// Consume atomically validates and retires the state identified by value for
// instanceID. Outcomes, all errors.Is-comparable:
//
//   - ErrInvalidState: unknown state value.
//   - ErrStateUsed: the state was already consumed (replay) — the state stays
//     registered until it expires so replays stay distinguishable from
//     unknown values.
//   - ErrStateExpired: past its expiry; the state is removed on access.
//   - ErrStateInstanceMismatch: consumed for a different instance; the state
//     is burned (removed) so a failed attempt is not retryable.
//
// On success the state is marked Used and returned with its verifier for the
// code exchange. State values are single-use: no successful consume can
// happen twice.
func (s *StateStore) Consume(value, instanceID string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.states[value]
	if !ok {
		return nil, errf("state.consume", ErrInvalidState, "state not found")
	}
	if st.Used {
		return nil, errf("state.consume", ErrStateUsed, "state was already consumed")
	}
	if st.Expired(s.now()) {
		delete(s.states, value)
		return nil, errf("state.consume", ErrStateExpired, "state has expired")
	}
	if instanceID != "" && st.InstanceID != "" && instanceID != st.InstanceID {
		delete(s.states, value)
		return nil, errf("state.consume", ErrStateInstanceMismatch, "state is bound to a different instance")
	}
	st.Used = true
	return st, nil
}

// PurgeInstance removes every pending state bound to instanceID and returns
// how many were removed. It keeps at most one active authorization flow per
// gateway instance: a superseded sign-in (e.g. a reconnect started while an
// older browser tab is still open) can never complete and clobber the newer
// flow's credential.
func (s *StateStore) PurgeInstance(instanceID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for value, st := range s.states {
		if st.InstanceID == instanceID {
			delete(s.states, value)
			removed++
		}
	}
	return removed
}

// PurgeExpired removes every state expired at or before now and returns how
// many were removed. It is a maintenance hook for periodic cleanup.
func (s *StateStore) PurgeExpired(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for value, st := range s.states {
		if st.Expired(now) {
			delete(s.states, value)
			removed++
		}
	}
	return removed
}
