package observability

import (
	"sync"
	"testing"
)

func TestCallbackPanicsZeroValueIsUsable(t *testing.T) {
	var p CallbackPanics

	if got := p.Total(); got != 0 {
		t.Errorf("Total() = %d, want 0", got)
	}
	if got := p.Counts(); got != nil {
		t.Errorf("Counts() = %v, want nil before any panic", got)
	}
}

func TestRunRecordsPanicsWithoutPropagating(t *testing.T) {
	var p CallbackPanics

	// The point of the type: a panicking observer must not unwind into the
	// caller's loop.
	p.Run("observer", func() { panic("observer blew up") })
	p.Run("observer", func() { panic("again") })
	p.Run("other", func() { panic("different callback") })

	if got := p.Total(); got != 3 {
		t.Errorf("Total() = %d, want 3", got)
	}
	counts := p.Counts()
	if counts["observer"] != 2 {
		t.Errorf("Counts()[observer] = %d, want 2", counts["observer"])
	}
	if counts["other"] != 1 {
		t.Errorf("Counts()[other] = %d, want 1", counts["other"])
	}
}

func TestRunLeavesWellBehavedCallbacksAlone(t *testing.T) {
	var p CallbackPanics
	called := false

	p.Run("fine", func() { called = true })
	p.Run("nil is a no-op", nil)

	if !called {
		t.Error("Run() did not invoke the callback")
	}
	if got := p.Total(); got != 0 {
		t.Errorf("Total() = %d, want 0 when nothing panicked", got)
	}
}

func TestCountsReturnsACopy(t *testing.T) {
	var p CallbackPanics
	p.Run("observer", func() { panic("boom") })

	counts := p.Counts()
	counts["observer"] = 99
	counts["injected"] = 1

	fresh := p.Counts()
	if fresh["observer"] != 1 {
		t.Errorf("Counts()[observer] = %d after caller mutation, want 1", fresh["observer"])
	}
	if _, ok := fresh["injected"]; ok {
		t.Error("caller mutated the recorder's map through the returned copy")
	}
}

func TestConcurrentRecordingIsSafe(t *testing.T) {
	var p CallbackPanics
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Run("racer", func() { panic("boom") })
		}()
	}
	wg.Wait()

	if got := p.Total(); got != 50 {
		t.Errorf("Total() = %d, want 50", got)
	}
	if got := p.Counts()["racer"]; got != 50 {
		t.Errorf("Counts()[racer] = %d, want 50", got)
	}
}

func TestRecoverFromRecordsAnUnwindingPanic(t *testing.T) {
	var p CallbackPanics

	func() {
		defer p.RecoverFrom("direct")
		panic("unwinding")
	}()

	// And records nothing when no panic is in flight.
	func() {
		defer p.RecoverFrom("quiet")
	}()

	if got := p.Total(); got != 1 {
		t.Errorf("Total() = %d, want 1", got)
	}
	if _, ok := p.Counts()["quiet"]; ok {
		t.Error("RecoverFrom() recorded a panic that never happened")
	}
}
