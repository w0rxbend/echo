package app

import (
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/worxbend/echo/internal/matrix"
	"github.com/worxbend/echo/internal/metrics"
)

// A restore that climbs out of a retry loop is the transition worth alerting on,
// and it is invisible in a counter that only records attempts and failures --
// success had to be inferred by subtraction, which says nothing about when it
// happened. The outcome label separates the recovery from an ordinary first-try
// apply.
func TestBackgroundRestoreSuccessOutcomeSeparatesRecoveryFromFirstTry(t *testing.T) {
	tests := []struct {
		name        string
		event       matrix.BackgroundRestoreEvent
		wantOutcome string
	}{
		{
			name: "first try",
			event: matrix.BackgroundRestoreEvent{
				Kind:      matrix.BackgroundKindRenderable,
				State:     matrix.BackgroundConvergenceConverged,
				ErrorKind: matrix.ErrorKindNone,
			},
			wantOutcome: backgroundRestoreOutcomeConverged,
		},
		{
			name: "after failures",
			event: matrix.BackgroundRestoreEvent{
				Kind:         matrix.BackgroundKindRenderable,
				State:        matrix.BackgroundConvergenceConverged,
				ErrorKind:    matrix.ErrorKindNone,
				FailureCount: 2,
			},
			wantOutcome: backgroundRestoreOutcomeRecovered,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := metrics.New()
			if err != nil {
				t.Fatalf("new registry: %v", err)
			}

			recordBackgroundRestoreMetric(registry, "matrix-a", tt.event)

			values := gatherBackgroundRestoreSuccesses(t, registry)
			wantKey := "matrix-a|" + publicKindLabel(t, tt.event.Kind) + "|" + tt.wantOutcome
			if len(values) != 1 || values[wantKey] != 1 {
				t.Fatalf("success series = %v, want %s = 1", values, wantKey)
			}
		})
	}
}

// The counter only earns its keep if the three series reconcile: every attempt
// ends in exactly one failure or one success, so a gap between them means a
// restore path stopped reporting rather than that nothing happened.
func TestBackgroundRestoreAttemptsEqualFailuresPlusSuccesses(t *testing.T) {
	registry, err := metrics.New()
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	// Two restores: the first fails twice before converging, the second converges
	// on its first pass.
	events := []matrix.BackgroundRestoreEvent{
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceAttempting},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceRetrying, ErrorKind: matrix.ErrorKindRetryable, FailureCount: 1},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceAttempting},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceRetrying, ErrorKind: matrix.ErrorKindRetryable, FailureCount: 2},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceAttempting},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceConverged, ErrorKind: matrix.ErrorKindNone, FailureCount: 2},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceAttempting},
		{Kind: matrix.BackgroundKindRenderable, State: matrix.BackgroundConvergenceConverged, ErrorKind: matrix.ErrorKindNone},
	}
	for _, event := range events {
		recordBackgroundRestoreMetric(registry, "matrix-a", event)
	}

	totals := map[string]float64{}
	for name, values := range map[string]map[string]float64{
		"matrix_proxy_background_restore_attempts_total":  gatherCounter(t, registry, "matrix_proxy_background_restore_attempts_total"),
		"matrix_proxy_background_restore_failures_total":  gatherCounter(t, registry, "matrix_proxy_background_restore_failures_total"),
		"matrix_proxy_background_restore_successes_total": gatherCounter(t, registry, "matrix_proxy_background_restore_successes_total"),
	} {
		for _, value := range values {
			totals[name] += value
		}
	}

	attempts := totals["matrix_proxy_background_restore_attempts_total"]
	failures := totals["matrix_proxy_background_restore_failures_total"]
	successes := totals["matrix_proxy_background_restore_successes_total"]
	if attempts != failures+successes {
		t.Fatalf("attempts = %g, failures + successes = %g; every attempt must end in exactly one terminal event", attempts, failures+successes)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %g, want 4", attempts)
	}

	kind := publicKindLabel(t, matrix.BackgroundKindRenderable)
	successValues := gatherBackgroundRestoreSuccesses(t, registry)
	if got := successValues["matrix-a|"+kind+"|"+backgroundRestoreOutcomeRecovered]; got != 1 {
		t.Fatalf("recovered successes = %g, want 1", got)
	}
	if got := successValues["matrix-a|"+kind+"|"+backgroundRestoreOutcomeConverged]; got != 1 {
		t.Fatalf("converged successes = %g, want 1", got)
	}
}

// The series carries the projected public kind, not the matrix vocabulary, so
// the expectation is derived through the same projection the handler uses --
// hardcoding the matrix spelling would assert a label that is never emitted.
func publicKindLabel(t *testing.T, kind matrix.BackgroundKind) string {
	t.Helper()
	label, ok := publicBackgroundKindLabel(kind)
	if !ok {
		t.Fatalf("background kind %s has no public label", kind)
	}
	return label
}

func gatherBackgroundRestoreSuccesses(t *testing.T, registry *metrics.Registry) map[string]float64 {
	t.Helper()
	return gatherCounter(t, registry, "matrix_proxy_background_restore_successes_total")
}

// Keys join the label values in declaration order so a series that grows or
// loses a label shows up as a missing key rather than a silent match.
func gatherCounter(t *testing.T, registry *metrics.Registry, name string) map[string]float64 {
	t.Helper()
	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			values[labelKey(metric.GetLabel())] = metric.GetCounter().GetValue()
		}
	}
	return values
}

func labelKey(labels []*dto.LabelPair) string {
	key := ""
	for i, label := range labels {
		if i > 0 {
			key += "|"
		}
		key += label.GetValue()
	}
	return key
}
