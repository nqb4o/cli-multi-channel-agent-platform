package orchestrator

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleFlightDoCollapsesConcurrentCalls(t *testing.T) {
	sf := NewSingleFlight[int]()
	var counter int32
	var wg sync.WaitGroup
	results := make([]int, 10)
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := sf.Do("k", func() (int, error) {
				atomic.AddInt32(&counter, 1)
				time.Sleep(50 * time.Millisecond)
				return 42, nil
			})
			results[i] = r
			errs[i] = err
		}(i)
	}
	wg.Wait()
	if atomic.LoadInt32(&counter) != 1 {
		t.Errorf("expected fn called once, got %d", counter)
	}
	for i, r := range results {
		if r != 42 {
			t.Errorf("result %d = %d", i, r)
		}
		if errs[i] != nil {
			t.Errorf("err %d = %v", i, errs[i])
		}
	}
}

func TestSingleFlightDifferentKeysDoNotShare(t *testing.T) {
	sf := NewSingleFlight[string]()
	a, _ := sf.Do("a", func() (string, error) { return "A", nil })
	b, _ := sf.Do("b", func() (string, error) { return "B", nil })
	if a != "A" || b != "B" {
		t.Errorf("a=%q b=%q", a, b)
	}
}

func TestSingleFlightErrorPropagates(t *testing.T) {
	sf := NewSingleFlight[int]()
	want := errors.New("boom")
	_, got := sf.Do("k", func() (int, error) { return 0, want })
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}

	// Slot freed: next call runs again.
	count := 0
	_, _ = sf.Do("k", func() (int, error) {
		count++
		return 1, nil
	})
	if count != 1 {
		t.Errorf("expected fresh call after error, got count=%d", count)
	}
}

func TestSingleFlightInFlightAndKeys(t *testing.T) {
	sf := NewSingleFlight[int]()
	if sf.InFlight("k") {
		t.Error("expected not in-flight")
	}
	if len(sf.Keys()) != 0 {
		t.Errorf("expected empty keys, got %v", sf.Keys())
	}

	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = sf.Do("k", func() (int, error) {
			close(start)
			<-done
			return 1, nil
		})
	}()
	<-start
	if !sf.InFlight("k") {
		t.Error("expected in-flight after start")
	}
	keys := sf.Keys()
	if len(keys) != 1 || keys[0] != "k" {
		t.Errorf("keys = %v", keys)
	}
	close(done)
}
