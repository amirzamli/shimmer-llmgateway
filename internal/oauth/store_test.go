package oauth

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fixedClock returns a store clock pinned to a fixed instant.
func fixedClock() (func() time.Time, time.Time) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }, now
}

func TestStoreGenerateConsume(t *testing.T) {
	s := NewStateStore()
	st, err := s.Generate("inst-1", time.Minute)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := s.Consume(st.Value, "inst-1")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got.Value != st.Value || got.Verifier != st.Verifier || got.Challenge != st.Challenge {
		t.Errorf("consumed state differs from generated: %+v vs %+v", got, st)
	}
	if !got.Used {
		t.Error("consumed state not marked Used")
	}
	if !PKCEMatches(got.Verifier, got.Challenge) {
		t.Error("consumed state verifier/challenge mismatch")
	}
}

func TestStoreReplayRejected(t *testing.T) {
	s := NewStateStore()
	st, err := s.Generate("inst-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(st.Value, "inst-1"); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	// Replays stay distinguishable from unknown values until expiry.
	if _, err := s.Consume(st.Value, "inst-1"); !errors.Is(err, ErrStateUsed) {
		t.Errorf("replayed Consume err = %v, want ErrStateUsed", err)
	}
	if _, err := s.Consume(st.Value, "inst-1"); !errors.Is(err, ErrStateUsed) {
		t.Errorf("second replay err = %v, want ErrStateUsed", err)
	}
}

func TestStoreUnknownState(t *testing.T) {
	s := NewStateStore()
	if _, err := s.Consume("never-generated", "inst-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("err = %v, want ErrInvalidState", err)
	}
}

func TestStoreExpiry(t *testing.T) {
	s := NewStateStore()
	clock, now := fixedClock()
	s.now = clock

	st, err := s.Generate("inst-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Advance the store clock past the ttl.
	s.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := s.Consume(st.Value, "inst-1"); !errors.Is(err, ErrStateExpired) {
		t.Errorf("expired Consume err = %v, want ErrStateExpired", err)
	}
	// The expired state is retired: a second attempt is invalid, not
	// expired.
	if _, err := s.Consume(st.Value, "inst-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("err = %v, want ErrInvalidState after retirement", err)
	}
}

func TestStoreExpiryAtBoundary(t *testing.T) {
	s := NewStateStore()
	clock, now := fixedClock()
	s.now = clock
	st, err := s.Generate("inst-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly at ExpiresAt the state is still valid (Expired uses After).
	s.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := s.Consume(st.Value, "inst-1"); err != nil {
		t.Errorf("Consume exactly at expiry: %v, want nil", err)
	}
}

func TestStoreInstanceBinding(t *testing.T) {
	s := NewStateStore()
	st, err := s.Generate("inst-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(st.Value, "inst-2"); !errors.Is(err, ErrStateInstanceMismatch) {
		t.Errorf("wrong-instance Consume err = %v, want ErrStateInstanceMismatch", err)
	}
	// The mismatched consume burned the state (single-use, no retry).
	if _, err := s.Consume(st.Value, "inst-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("err = %v, want ErrInvalidState after burned attempt", err)
	}
}

func TestStoreUnboundInstance(t *testing.T) {
	s := NewStateStore()
	st, err := s.Generate("", time.Minute) // no binding
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(st.Value, "any-instance"); err != nil {
		t.Errorf("Consume with unbound state: %v, want nil", err)
	}
}

func TestStoreGenerateInvalidTTL(t *testing.T) {
	s := NewStateStore()
	if _, err := s.Generate("inst-1", 0); err == nil {
		t.Error("Generate with zero ttl: want error")
	}
	if _, err := s.Generate("inst-1", -time.Second); err == nil {
		t.Error("Generate with negative ttl: want error")
	}
}

func TestStorePurgeExpired(t *testing.T) {
	s := NewStateStore()
	clock, now := fixedClock()
	s.now = clock

	for i := 0; i < 3; i++ {
		if _, err := s.Generate("inst-1", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.PurgeExpired(now.Add(30 * time.Second)); n != 0 {
		t.Errorf("PurgeExpired before expiry removed %d, want 0", n)
	}
	if n := s.PurgeExpired(now.Add(2 * time.Minute)); n != 3 {
		t.Errorf("PurgeExpired after expiry removed %d, want 3", n)
	}
}

func TestStorePurgeInstance(t *testing.T) {
	s := NewStateStore()
	for _, inst := range []string{"inst-1", "inst-1", "inst-2"} {
		if _, err := s.Generate(inst, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	// Purging one instance leaves the other's states consumable.
	if n := s.PurgeInstance("inst-1"); n != 2 {
		t.Errorf("PurgeInstance removed %d, want 2", n)
	}
	if n := s.PurgeInstance("inst-1"); n != 0 {
		t.Errorf("second PurgeInstance removed %d, want 0", n)
	}
	st, err := s.Generate("inst-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A consumed state stays registered (replays remain distinguishable)
	// until expiry, so the purge also removes it.
	if _, err := s.Consume(st.Value, "inst-2"); err != nil {
		t.Errorf("Consume after purge of another instance: %v", err)
	}
	if n := s.PurgeInstance("inst-2"); n != 2 {
		t.Errorf("PurgeInstance inst-2 removed %d, want 2 (the pending and the consumed one)", n)
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStateStore()
	const n = 64
	states := make([]*State, n)
	for i := 0; i < n; i++ {
		st, err := s.Generate("inst-1", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		states[i] = st
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half the goroutines consume the same state twice to exercise
			// the single-use path under contention.
			if _, err := s.Consume(states[i].Value, "inst-1"); err != nil {
				errs <- err
			}
			if _, err := s.Consume(states[i].Value, "inst-1"); err != nil && !errors.Is(err, ErrStateUsed) && !errors.Is(err, ErrInvalidState) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Consume: %v", err)
	}
}
