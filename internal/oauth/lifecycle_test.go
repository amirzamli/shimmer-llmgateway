package oauth

import (
	"errors"
	"testing"
	"time"
)

func TestLifecycleBeginSupersedesPreviousGeneration(t *testing.T) {
	life := NewLifecycle()
	first := life.Begin("chatgpt")
	second := life.Begin("chatgpt")
	if first == second || life.Current("chatgpt", first) {
		t.Fatalf("generations = %d/%d, want only the newest generation current", first, second)
	}
	if !life.Current("chatgpt", second) {
		t.Fatal("newest generation is not current")
	}
}

func TestLifecycleIfCurrentFencesMutation(t *testing.T) {
	life := NewLifecycle()
	generation := life.Begin("chatgpt")
	called := false
	life.Begin("chatgpt")
	current, err := life.IfCurrent("chatgpt", generation, func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if current || called {
		t.Fatalf("IfCurrent = (%v, %v), want false and no mutation", current, called)
	}
}

func TestLifecycleWithTransitionOnlyCommitsGenerationOnSuccess(t *testing.T) {
	life := NewLifecycle()
	first := life.Begin("chatgpt")
	failed := errors.New("config write failed")
	if err := life.WithTransition([]string{"chatgpt"}, func() (bool, error) {
		return false, failed
	}); !errors.Is(err, failed) {
		t.Fatalf("failed transition error = %v, want %v", err, failed)
	}
	if got := life.Generation("chatgpt"); got != first {
		t.Fatalf("failed transition generation = %d, want %d", got, first)
	}

	if err := life.WithTransition([]string{"chatgpt", "chatgpt"}, func() (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := life.Generation("chatgpt"); got == first {
		t.Fatalf("committed transition generation = %d, want advancement from %d", got, first)
	}
}

func TestLifecycleWithTransitionBlocksConcurrentBegin(t *testing.T) {
	life := NewLifecycle()
	started := make(chan struct{})
	release := make(chan struct{})
	transitionDone := make(chan struct{})
	go func() {
		_ = life.WithTransition([]string{"chatgpt"}, func() (bool, error) {
			close(started)
			<-release
			return true, nil
		})
		close(transitionDone)
	}()
	<-started

	beginDone := make(chan struct{})
	go func() {
		life.Begin("chatgpt")
		close(beginDone)
	}()
	select {
	case <-beginDone:
		t.Fatal("Begin completed while transition was holding the lifecycle lock")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-transitionDone:
	case <-time.After(time.Second):
		t.Fatal("transition did not complete")
	}
	select {
	case <-beginDone:
	case <-time.After(time.Second):
		t.Fatal("Begin did not complete after transition")
	}
}
