package matrix

import (
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

// applyDesiredBackground reports an attempt for every restore and a failure for
// every failure, so without a convergence event the two counters could never be
// reconciled and the log fell silent at the one moment worth alerting on: the
// pass that climbs out of a retry loop. The failure count is captured before the
// retry state is cleared, which is what makes a recovery distinguishable from an
// ordinary first-try apply -- reading it afterwards would report zero every time.
func TestBackgroundRestoreConvergedCarriesFailuresEnduredBeforeSuccess(t *testing.T) {
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "background", testAnimation(1, time.Millisecond, 70))

	backgrounds := newBackgroundRestoreRecorder()
	scheduler := newTestScheduler(t, newFakeMatrixClient(), registry, SchedulerOptions{
		Background:          BackgroundConfig{AnimationID: "background"},
		OnBackgroundRestore: backgrounds.record,
	})

	scheduler.mu.Lock()
	scheduler.backgroundRetryFailureCount = 2
	scheduler.backgroundRetryLastErrorClass = ErrorKindRetryable
	scheduler.backgroundNextRestoreAttempt = scheduler.now().Add(time.Minute)
	scheduler.mu.Unlock()

	scheduler.markBackgroundRestoreConverged()

	if got := backgrounds.countState(BackgroundConvergenceConverged); got != 1 {
		t.Fatalf("converged events = %d, want 1", got)
	}
	events := backgrounds.snapshot()
	if len(events) != 1 {
		t.Fatalf("restore events = %d, want 1", len(events))
	}
	event := events[0]
	if event.State != BackgroundConvergenceConverged {
		t.Fatalf("state = %s, want %s", event.State, BackgroundConvergenceConverged)
	}
	if event.FailureCount != 2 {
		t.Fatalf("failure count = %d, want 2; convergence must report the failures it recovered from, not the reset value", event.FailureCount)
	}
	if event.ErrorKind != ErrorKindNone {
		t.Fatalf("error kind = %s, want %s", event.ErrorKind, ErrorKindNone)
	}
	if event.Error != "" {
		t.Fatalf("error = %q, want empty", event.Error)
	}
	if event.NextRetry != nil {
		t.Fatalf("next retry = %v, want nil; a converged restore has nothing scheduled", event.NextRetry)
	}
	if event.AnimationID != "background" {
		t.Fatalf("background id = %q, want %q", event.AnimationID, "background")
	}

	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.backgroundRetryFailureCount != 0 {
		t.Fatalf("retry failure count = %d, want 0 after convergence", scheduler.backgroundRetryFailureCount)
	}
	if scheduler.backgroundLastRestoreSuccess.IsZero() {
		t.Fatal("last restore success is zero, want the convergence timestamp")
	}
}

// Disabling the background converges the scheduler without restoring anything,
// so it must stay out of the restore stream: reporting it would break the
// attempts = failures + successes invariant that the convergence event exists to
// provide. This guards the emit site -- moving it down into
// markDesiredBackgroundConvergedLocked would fire here.
func TestSetBackgroundDisableReportsNoRestore(t *testing.T) {
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "background", testAnimation(1, time.Millisecond, 70))

	backgrounds := newBackgroundRestoreRecorder()
	scheduler := newTestScheduler(t, newFakeMatrixClient(), registry, SchedulerOptions{
		Background:          BackgroundConfig{AnimationID: "background"},
		OnBackgroundRestore: backgrounds.record,
	})

	if err := scheduler.SetBackground(BackgroundConfig{}); err != nil {
		t.Fatalf("disable background: %v", err)
	}

	if got := backgrounds.countState(BackgroundConvergenceConverged); got != 0 {
		t.Fatalf("converged events = %d, want 0; disabling the background is not a restore", got)
	}
	if got := len(backgrounds.snapshot()); got != 0 {
		t.Fatalf("restore events = %d, want 0", got)
	}
}
