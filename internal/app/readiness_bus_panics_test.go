package app

import (
	"context"
	"testing"

	"github.com/worxbend/echo/internal/events"
)

// The bus is the one component with recovered observability callbacks that is
// not owned by a device, so it was the one whose panics readiness never saw.
func TestReadinessReportsEventBusCallbackPanics(t *testing.T) {
	bus, err := events.NewBus(1)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsubscribe := bus.SubscribeWithOptions(ctx, events.SubscriptionOptions{
		OnDepthChange: func(int) { panic("depth observer blew up") },
	})
	defer unsubscribe()

	if err := bus.Publish(ctx, events.Event{ID: "e1", Source: events.SourceHTTP, Type: "test"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	<-ch

	// Two observations, both synchronous and both counted: SubscribeWithOptions
	// seeds the observer with the initial depth of zero, and Publish reports the
	// depth again once the event is queued. Nothing here sleeps or races.
	const wantPanics = 2

	if got := bus.ObservabilityCallbackPanics(); got != wantPanics {
		t.Fatalf("bus recovered panics = %d, want %d", got, wantPanics)
	}

	app := &App{bus: bus}
	body, _ := app.readiness()

	if body.ObservabilityCallbackPanics != wantPanics {
		t.Fatalf("readiness observability_callback_panics = %d, want %d", body.ObservabilityCallbackPanics, wantPanics)
	}
	if got := body.ObservabilityCallbackCounts[events.ObservabilityCallbackDepthChange]; got != wantPanics {
		t.Fatalf("readiness %s count = %d, want %d", events.ObservabilityCallbackDepthChange, got, wantPanics)
	}
}

// A bus that never panics must not add an empty breakdown map to the payload.
func TestReadinessOmitsEventBusPanicCountsWhenQuiet(t *testing.T) {
	bus, err := events.NewBus(1)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	app := &App{bus: bus}
	body, _ := app.readiness()

	if body.ObservabilityCallbackPanics != 0 {
		t.Fatalf("observability_callback_panics = %d, want 0", body.ObservabilityCallbackPanics)
	}
	if body.ObservabilityCallbackCounts != nil {
		t.Fatalf("observability_callback_panic_counts = %v, want nil", body.ObservabilityCallbackCounts)
	}
}
