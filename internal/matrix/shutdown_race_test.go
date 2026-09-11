package matrix

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

// cancelOnProbeClient fails every Ping with a retryable transport error, which
// is what an unreachable device looks like.
type cancelOnProbeClient struct {
	*fakeMatrixClient
}

func (c *cancelOnProbeClient) Ping(context.Context) error {
	return &net0pErr{}
}

// net0pErr is a plain ECONNREFUSED: retryable while the context is live, and
// classified permanent the instant it is not.
type net0pErr struct{}

func (e *net0pErr) Error() string { return "dial tcp 127.0.0.1:1: connect: connection refused" }
func (e *net0pErr) Unwrap() error { return syscall.ECONNREFUSED }

// TestRunTreatsCancellationDuringProbeFailureAsShutdown pins the window that
// made App.Shutdown report a connection error for a device the caller had just
// deliberately stopped.
//
// heartbeat checks the context, reports the probe failure, then classifies the
// error -- and the classifiers call anything permanent once the context is
// done. Cancelling between the check and the classification therefore turned an
// ordinary "connection refused" into a permanent failure that ended Run.
//
// The OnProbeFailure observer runs exactly in that window, so cancelling from it
// reproduces the race deterministically rather than by timing luck.
func TestRunTreatsCancellationDuringProbeFailureAsShutdown(t *testing.T) {
	registry := animations.NewRegistry()
	client := &cancelOnProbeClient{fakeMatrixClient: newFakeMatrixClient()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler, err := NewScheduler(SchedulerOptions{
		Client:            client,
		Registry:          registry,
		HeartbeatInterval: time.Millisecond,
		ProbeTimeout:      50 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		// Cancel from inside the window between heartbeat's context check and
		// its classification of the probe error.
		OnProbeFailure: func(ProbeFailure) { cancel() },
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- scheduler.Run(ctx) }()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil: a probe that raced cancellation is a "+
				"shutdown, not a device failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

// TestCancellationOrPrefersTheContextError covers the helper directly, including
// the nil-context case the scheduler's own call sites never produce.
func TestCancellationOrPrefersTheContextError(t *testing.T) {
	refused := &net0pErr{}

	if got := cancellationOr(nil, refused); !errors.Is(got, syscall.ECONNREFUSED) {
		t.Errorf("cancellationOr(nil, err) = %v, want the original error", got)
	}

	live := context.Background()
	if got := cancellationOr(live, refused); !errors.Is(got, syscall.ECONNREFUSED) {
		t.Errorf("cancellationOr(live, err) = %v, want the original error", got)
	}

	done, cancel := context.WithCancel(context.Background())
	cancel()
	if got := cancellationOr(done, refused); !errors.Is(got, context.Canceled) {
		t.Errorf("cancellationOr(cancelled, err) = %v, want context.Canceled", got)
	}
}
