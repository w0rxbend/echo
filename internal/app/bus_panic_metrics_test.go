package app

import (
	"context"
	"testing"

	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/metrics"
)

// Readiness already counted the bus's recovered callback panics, but a readiness
// poll is a point-in-time answer: it cannot be graphed or alerted on, so an
// observer that panics on every call still trended nowhere. This covers the
// Prometheus counter that closes that gap.
func TestEventBusObservabilityCallbackPanicsAreScraped(t *testing.T) {
	registry, err := metrics.New()
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	bus, err := events.NewBus(1)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	for _, cb := range busObservabilityCallbackNames() {
		cb := cb
		if err := registry.RegisterEventObservabilityCallbackPanics(cb, func() float64 {
			return float64(bus.ObservabilityCallbackPanicCounts()[cb])
		}); err != nil {
			t.Fatalf("register %s: %v", cb, err)
		}
	}

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

	// SubscribeWithOptions seeds the observer with the initial depth of zero and
	// Publish reports it again once the event is queued: two synchronous calls,
	// two recovered panics, no sleeping and no race.
	const wantPanics = 2

	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "matrix_proxy_event_observability_callback_panics_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := metric.GetLabel()
			// The bus is shared, so the series must carry the callback name alone --
			// a device label here would mean the counter was registered per device.
			if len(labels) != 1 || labels[0].GetName() != "callback" {
				t.Fatalf("bus panic metric labels = %v, want single callback label", labels)
			}
			values[labels[0].GetValue()] = metric.GetCounter().GetValue()
		}
	}

	if len(values) != len(busObservabilityCallbackNames()) {
		t.Fatalf("bus panic metric series = %v, want one per callback name %v", values, busObservabilityCallbackNames())
	}
	if got := values[events.ObservabilityCallbackDepthChange]; got != wantPanics {
		t.Fatalf("%s panics = %g, want %d", events.ObservabilityCallbackDepthChange, got, wantPanics)
	}
	for _, cb := range []string{
		events.ObservabilityCallbackPublishBackpressureWait,
		events.ObservabilityCallbackPublishBackpressureTimeout,
	} {
		if got := values[cb]; got != 0 {
			t.Fatalf("%s panics = %g, want 0", cb, got)
		}
	}
}

// The Prometheus series are registered by iterating this helper, so if it ever
// stops tracking the bus's own list a name can be recorded, counted in
// readiness, and have no series to graph or alert on. The bus package guards
// its list against its Run/RecoverFrom sites; this guards the hand-off.
func TestBusObservabilityCallbackNamesMatchEvents(t *testing.T) {
	got := busObservabilityCallbackNames()
	want := events.ObservabilityCallbackNames()
	if len(got) != len(want) {
		t.Fatalf("busObservabilityCallbackNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("busObservabilityCallbackNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Registering the same global counter twice must fail rather than silently
// shadow the first: that is what would happen if this ever moved into the
// per-device build path.
func TestEventBusObservabilityCallbackPanicMetricRejectsDuplicateRegistration(t *testing.T) {
	registry, err := metrics.New()
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	value := func() float64 { return 0 }
	if err := registry.RegisterEventObservabilityCallbackPanics(events.ObservabilityCallbackDepthChange, value); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := registry.RegisterEventObservabilityCallbackPanics(events.ObservabilityCallbackDepthChange, value); err == nil {
		t.Fatal("duplicate registration succeeded, want error")
	}
}
