package oauth

import (
	"errors"
	"sync"
)

// ErrOperationStale reports an OAuth operation that lost a lifecycle race with
// a newer login, disconnect, rename, or delete for the same instance.
var ErrOperationStale = errors.New("oauth operation was superseded")

// Lifecycle tracks the current operation generation for each instance. It is
// shared by the API callback path and the gateway refresh resolver so neither
// can persist credentials after the instance has moved to a newer lifecycle.
type Lifecycle struct {
	mu          sync.Mutex
	generations map[string]uint64
}

// NewLifecycle returns an empty lifecycle registry.
func NewLifecycle() *Lifecycle {
	return &Lifecycle{generations: make(map[string]uint64)}
}

// Begin retires all operations from the previous generation and returns the
// new generation for an instance.
func (l *Lifecycle) Begin(instanceID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.beginLocked(instanceID)
}

func (l *Lifecycle) beginLocked(instanceID string) uint64 {
	if l.generations == nil {
		l.generations = make(map[string]uint64)
	}
	generation := l.generations[instanceID] + 1
	if generation == 0 { // Keep zero reserved for the initial generation.
		generation = 1
	}
	l.generations[instanceID] = generation
	return generation
}

// WithTransition runs a config/credential mutation while blocking lifecycle
// readers and writers for the affected instances. fn reports whether the
// config change committed; when it did, all ids advance to a new generation
// after fn returns. A failed config change therefore leaves pending OAuth work
// valid, while a committed change fences callbacks before they can persist.
// fn must not call methods on l because the lifecycle lock is held.
func (l *Lifecycle) WithTransition(instanceIDs []string, fn func() (committed bool, err error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	committed, err := fn()
	if committed {
		seen := make(map[string]struct{}, len(instanceIDs))
		for _, instanceID := range instanceIDs {
			if _, ok := seen[instanceID]; ok {
				continue
			}
			seen[instanceID] = struct{}{}
			l.beginLocked(instanceID)
		}
	}
	return err
}

// Generation returns the current generation for an instance. A newly seen
// instance starts at generation zero and remains valid until Begin is called.
func (l *Lifecycle) Generation(instanceID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generations[instanceID]
}

// Current reports whether generation is still current for instanceID.
func (l *Lifecycle) Current(instanceID string, generation uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generations[instanceID] == generation
}

// IfCurrent runs fn while holding the lifecycle lock when generation is still
// current. This makes the final credential persistence or deletion atomic with
// respect to a lifecycle transition.
func (l *Lifecycle) IfCurrent(instanceID string, generation uint64, fn func() error) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.generations[instanceID] != generation {
		return false, nil
	}
	return true, fn()
}
