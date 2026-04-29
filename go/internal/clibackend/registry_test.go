package clibackend

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeBackend struct{ id string }

func (f *fakeBackend) ID() string                                          { return f.id }
func (f *fakeBackend) DefaultCommand() []string                            { return nil }
func (f *fakeBackend) SupportsResumeInStream() bool                        { return false }
func (f *fakeBackend) Turn(context.Context, CliTurnInput) (*CliTurnOutput, *CliTurnError) {
	return &CliTurnOutput{Text: "from " + f.id}, nil
}

func TestRegisterAndGet(t *testing.T) {
	r := NewBackendRegistry()
	if err := r.Register(&fakeBackend{id: "codex-cli"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("codex-cli")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != "codex-cli" {
		t.Fatalf("got %q", got.ID())
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	r := NewBackendRegistry()
	if err := r.Register(&fakeBackend{id: "x"}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(&fakeBackend{id: "x"})
	if !errors.Is(err, ErrDuplicateBackend) {
		t.Fatalf("expected ErrDuplicateBackend, got %v", err)
	}
}

func TestRegisterRejectsEmptyID(t *testing.T) {
	r := NewBackendRegistry()
	if err := r.Register(&fakeBackend{id: ""}); err == nil {
		t.Fatal("expected empty-id rejection")
	}
}

func TestRegisterRejectsNil(t *testing.T) {
	r := NewBackendRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("expected nil rejection")
	}
}

func TestGetUnknown(t *testing.T) {
	r := NewBackendRegistry()
	if _, err := r.Get("nope"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("expected ErrUnknownBackend, got %v", err)
	}
}

func TestRegisterLazyHappyPath(t *testing.T) {
	r := NewBackendRegistry()
	calls := atomic.Int32{}
	err := r.RegisterLazy("lazy-1", func() (CliBackend, error) {
		calls.Add(1)
		return &fakeBackend{id: "lazy-1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("factory ran before Get")
	}

	got, err := r.Get("lazy-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != "lazy-1" {
		t.Fatalf("got %q", got.ID())
	}
	if calls.Load() != 1 {
		t.Fatalf("factory ran %d times", calls.Load())
	}

	// Second Get reuses the cached instance (factory not re-invoked).
	got2, err := r.Get("lazy-1")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("factory re-ran: %d", calls.Load())
	}
	if !reflect.DeepEqual(got, got2) {
		t.Fatal("lazy backend not cached")
	}
}

func TestLazyFactoryErrorDropsEntry(t *testing.T) {
	r := NewBackendRegistry()
	tries := atomic.Int32{}
	r.RegisterLazy("flaky", func() (CliBackend, error) {
		n := tries.Add(1)
		if n < 2 {
			return nil, errors.New("not yet")
		}
		return &fakeBackend{id: "flaky"}, nil
	})

	// First Get fails and the entry is dropped.
	if _, err := r.Get("flaky"); err == nil {
		t.Fatal("expected first Get to fail")
	}
	if r.Has("flaky") {
		t.Fatal("failed lazy entry should have been removed")
	}

	// Re-register and prove the factory now succeeds.
	r.RegisterLazy("flaky", func() (CliBackend, error) {
		return &fakeBackend{id: "flaky"}, nil
	})
	got, err := r.Get("flaky")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != "flaky" {
		t.Fatalf("got %q", got.ID())
	}
}

func TestRegisterLazyRejectsEmptyOrNil(t *testing.T) {
	r := NewBackendRegistry()
	if err := r.RegisterLazy("", func() (CliBackend, error) { return nil, nil }); err == nil {
		t.Fatal("expected empty-id rejection")
	}
	if err := r.RegisterLazy("x", nil); err == nil {
		t.Fatal("expected nil-factory rejection")
	}
}

func TestIDs(t *testing.T) {
	r := NewBackendRegistry()
	_ = r.Register(&fakeBackend{id: "claude-cli"})
	_ = r.Register(&fakeBackend{id: "codex-cli"})
	_ = r.RegisterLazy("google-gemini-cli", func() (CliBackend, error) {
		return &fakeBackend{id: "google-gemini-cli"}, nil
	})
	got := r.IDs()
	want := []string{"claude-cli", "codex-cli", "google-gemini-cli"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs: got %v want %v", got, want)
	}
}

func TestUnregister(t *testing.T) {
	r := NewBackendRegistry()
	_ = r.Register(&fakeBackend{id: "x"})
	if !r.Unregister("x") {
		t.Fatal("expected true on existing")
	}
	if r.Unregister("x") {
		t.Fatal("expected false on missing")
	}
	if r.Has("x") {
		t.Fatal("entry should be gone")
	}
}

func TestConcurrentRegistration(t *testing.T) {
	// Stress the lock; one of the registrations wins, rest see
	// ErrDuplicateBackend.
	r := NewBackendRegistry()
	const N = 32
	var wg sync.WaitGroup
	wins := atomic.Int32{}
	dups := atomic.Int32{}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Register(&fakeBackend{id: "concurrent"})
			if err == nil {
				wins.Add(1)
			} else if errors.Is(err, ErrDuplicateBackend) {
				dups.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", wins.Load())
	}
	if dups.Load() != N-1 {
		t.Fatalf("expected %d dup errors, got %d", N-1, dups.Load())
	}
}

func TestLazyConcurrentGetRunsFactoryOnce(t *testing.T) {
	r := NewBackendRegistry()
	calls := atomic.Int32{}
	_ = r.RegisterLazy("lazy", func() (CliBackend, error) {
		calls.Add(1)
		return &fakeBackend{id: "lazy"}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Get("lazy"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("factory ran %d times — expected exactly 1", got)
	}
}
