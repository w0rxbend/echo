package app

import (
	"testing"

	"github.com/worxbend/echo/internal/matrix"
	"github.com/worxbend/echo/internal/metrics"
)

// buildDevice registers one panic counter per name in
// schedulerObservabilityCallbackNames, so a name the scheduler records but the
// list omits is counted in Health() and readiness yet absent from /metrics.
// That is exactly what had happened to queue_depth_change, animation_rendered
// and item_outcome while the list lived here instead of beside the Run call
// sites. This pins the registration to the matrix-owned list.
func TestSchedulerObservabilityCallbackPanicsAreScraped(t *testing.T) {
	registry, err := metrics.New()
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	const deviceID = "matrix-a"
	source := string(matrix.ReconnectSourceSchedulerBackoff)

	// The same loop buildDevice runs, against a counts map that stands in for a
	// scheduler whose observers have all blown up once.
	counts := map[string]uint64{}
	for i, cb := range schedulerObservabilityCallbackNames() {
		counts[cb] = uint64(i + 1)
	}
	for _, cb := range schedulerObservabilityCallbackNames() {
		cb := cb
		if err := registry.RegisterMatrixObservabilityCallbackPanics(deviceID, source, cb, func() float64 {
			return float64(counts[cb])
		}); err != nil {
			t.Fatalf("register %s: %v", cb, err)
		}
	}

	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "matrix_proxy_matrix_observability_callback_panics_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labelValues := map[string]string{}
			for _, label := range metric.GetLabel() {
				labelValues[label.GetName()] = label.GetValue()
			}
			if len(labelValues) != 3 || labelValues["device"] != deviceID || labelValues["source"] != source {
				t.Fatalf("scheduler panic metric labels = %v, want device/source/callback for %s/%s", labelValues, deviceID, source)
			}
			values[labelValues["callback"]] = metric.GetCounter().GetValue()
		}
	}

	if len(values) != len(schedulerObservabilityCallbackNames()) {
		t.Fatalf("scheduler panic metric series = %v, want one per callback name %v", values, schedulerObservabilityCallbackNames())
	}
	for _, cb := range schedulerObservabilityCallbackNames() {
		got, ok := values[cb]
		if !ok {
			t.Fatalf("no scheduler panic series for %s", cb)
		}
		if want := float64(counts[cb]); got != want {
			t.Fatalf("%s panics = %g, want %g", cb, got, want)
		}
	}

	// The three that the drifted list left unscraped; named explicitly so a
	// future trim of the list fails here with the reason rather than a count.
	for _, cb := range []string{
		matrix.ObservabilityCallbackQueueDepthChange,
		matrix.ObservabilityCallbackAnimationRendered,
		matrix.ObservabilityCallbackItemOutcome,
	} {
		if _, ok := values[cb]; !ok {
			t.Fatalf("%s has no /metrics series; it is counted in Health() and invisible to alerting", cb)
		}
	}
}

// The app-side helper must stay a pass-through: a local literal here is how the
// list drifted out of step with the scheduler in the first place.
func TestSchedulerObservabilityCallbackNamesMatchMatrix(t *testing.T) {
	got := schedulerObservabilityCallbackNames()
	want := matrix.SchedulerObservabilityCallbackNames()
	if len(got) != len(want) {
		t.Fatalf("app names = %v, matrix names = %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("app names = %v, matrix names = %v", got, want)
		}
	}
}
