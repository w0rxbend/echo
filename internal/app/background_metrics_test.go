package app

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/worxbend/echo/internal/matrix"
	"github.com/worxbend/echo/internal/metrics"
)

func TestBackgroundHealthMetricsAgreeWithReadyProjection(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	futureRetry := now.Add(time.Minute)
	dueRetry := now.Add(-time.Second)

	tests := []struct {
		name      string
		health    matrix.Health
		wantState matrix.BackgroundConvergenceState
		wantDirty bool
		wantClean bool
	}{
		{
			name: "dirty",
			health: matrix.Health{
				BackgroundKind:             matrix.BackgroundKindRenderable,
				BackgroundConvergenceState: matrix.BackgroundConvergenceDirty,
				BackgroundDirty:            true,
			},
			wantState: matrix.BackgroundConvergenceDirty,
			wantDirty: true,
		},
		{
			name: "attempting",
			health: matrix.Health{
				BackgroundKind:             matrix.BackgroundKindRenderable,
				BackgroundConvergenceState: matrix.BackgroundConvergenceAttempting,
				BackgroundDirty:            true,
			},
			wantState: matrix.BackgroundConvergenceAttempting,
			wantDirty: true,
		},
		{
			name: "converged",
			health: matrix.Health{
				BackgroundKind:             matrix.BackgroundKindRenderable,
				BackgroundConvergenceState: matrix.BackgroundConvergenceConverged,
			},
			wantState: matrix.BackgroundConvergenceConverged,
			wantClean: true,
		},
		{
			name: "failed due retry",
			health: matrix.Health{
				BackgroundKind:                  matrix.BackgroundKindRenderable,
				BackgroundConvergenceState:      matrix.BackgroundConvergenceRetrying,
				BackgroundDirty:                 true,
				BackgroundLastRestoreError:      "restore failed",
				BackgroundLastRestoreErrorClass: matrix.ErrorKindRetryable,
				BackgroundNextRetry:             &dueRetry,
				BackgroundRetryFailureCount:     1,
			},
			wantState: matrix.BackgroundConvergenceFailed,
			wantDirty: true,
		},
		{
			name: "retrying before retry deadline",
			health: matrix.Health{
				BackgroundKind:                  matrix.BackgroundKindRenderable,
				BackgroundConvergenceState:      matrix.BackgroundConvergenceRetrying,
				BackgroundDirty:                 true,
				BackgroundLastRestoreError:      "restore failed",
				BackgroundLastRestoreErrorClass: matrix.ErrorKindRetryable,
				BackgroundNextRetry:             &futureRetry,
				BackgroundRetryFailureCount:     1,
			},
			wantState: matrix.BackgroundConvergenceRetrying,
			wantDirty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := metrics.New()
			if err != nil {
				t.Fatal(err)
			}

			readyBackground := backgroundConvergenceProjectionForApp(tc.health, now)
			if readyBackground.State != tc.wantState || readyBackground.Dirty != tc.wantDirty || readyBackground.Converged != tc.wantClean {
				t.Fatalf("ready projection = %+v, want state=%q dirty=%v converged=%v",
					readyBackground, tc.wantState, tc.wantDirty, tc.wantClean)
			}

			recordBackgroundHealthMetrics(registry, tc.health, now)
			byName := appMetricFamiliesByName(t, registry)

			for _, state := range matrix.BackgroundConvergenceV1States() {
				want := 0.0
				if state == readyBackground.State {
					want = 1
				}
				assertAppMetricGaugeValue(t, byName, "matrix_proxy_background_state", want, map[string]string{
					"kind":  "generated",
					"state": string(state),
				})
			}
			assertAppMetricGaugeValue(t, byName, "matrix_proxy_background_dirty", boolGaugeValue(readyBackground.Dirty), map[string]string{
				"kind": "generated",
			})
			assertAppMetricGaugeValue(t, byName, "matrix_proxy_background_converged", boolGaugeValue(readyBackground.Converged), map[string]string{
				"kind": "generated",
			})
		})
	}
}

func TestBackgroundRestoreMetricIncrementsCountersOnly(t *testing.T) {
	now := time.Now().UTC()
	futureRetry := now.Add(time.Minute)
	dueRetry := now.Add(-time.Second)

	tests := []struct {
		name             string
		event            matrix.BackgroundRestoreEvent
		wantAttempts     float64
		wantFailures     float64
		wantNoGaugeWrite bool
	}{
		{
			name: "attempting increments attempts counter",
			event: matrix.BackgroundRestoreEvent{
				Kind:         matrix.BackgroundKindRenderable,
				State:        matrix.BackgroundConvergenceAttempting,
				ErrorKind:    matrix.ErrorKindNone,
				NextRetry:    &futureRetry,
				FailureCount: 1,
			},
			wantAttempts:     1,
			wantFailures:     0,
			wantNoGaugeWrite: true,
		},
		{
			name: "retrying increments failures counter",
			event: matrix.BackgroundRestoreEvent{
				Kind:         matrix.BackgroundKindRenderable,
				State:        matrix.BackgroundConvergenceRetrying,
				ErrorKind:    matrix.ErrorKindRetryable,
				Error:        "restore failed",
				NextRetry:    &futureRetry,
				FailureCount: 1,
			},
			wantAttempts:     0,
			wantFailures:     1,
			wantNoGaugeWrite: true,
		},
		{
			name: "failed when retry is due increments failures counter",
			event: matrix.BackgroundRestoreEvent{
				Kind:         matrix.BackgroundKindRenderable,
				State:        matrix.BackgroundConvergenceRetrying,
				ErrorKind:    matrix.ErrorKindRetryable,
				Error:        "restore failed",
				NextRetry:    &dueRetry,
				FailureCount: 1,
			},
			wantAttempts:     0,
			wantFailures:     1,
			wantNoGaugeWrite: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := metrics.New()
			if err != nil {
				t.Fatal(err)
			}

			recordBackgroundRestoreMetric(registry, tc.event)
			byName := appMetricFamiliesByName(t, registry)

			assertAppMetricCounterValue(t, byName, "matrix_proxy_background_restore_attempts_total", tc.wantAttempts, map[string]string{
				"kind": "generated",
			})
			assertAppMetricCounterValue(t, byName, "matrix_proxy_background_restore_failures_total", tc.wantFailures, map[string]string{
				"kind":        "generated",
				"error_class": string(tc.event.ErrorKind),
			})

			if tc.wantNoGaugeWrite {
				if family, ok := byName["matrix_proxy_background_state"]; ok {
					for _, m := range family.GetMetric() {
						if v := m.GetGauge().GetValue(); v != 0 {
							t.Fatalf("restore event callback must not write background_state gauge; got non-zero value %g", v)
						}
					}
				}
			}
		})
	}
}

func boolGaugeValue(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func appMetricFamiliesByName(t *testing.T, registry *metrics.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	return byName
}

func assertAppMetricGaugeValue(
	t *testing.T,
	families map[string]*dto.MetricFamily,
	name string,
	want float64,
	labels map[string]string,
) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	for _, metric := range family.GetMetric() {
		if !appMetricHasLabels(metric, labels) {
			continue
		}
		if got := metric.GetGauge().GetValue(); got != want {
			t.Fatalf("metric family %q labels %v value = %g, want %g", name, labels, got, want)
		}
		return
	}
	t.Fatalf("metric family %q missing labels %v", name, labels)
}

func assertAppMetricCounterValue(
	t *testing.T,
	families map[string]*dto.MetricFamily,
	name string,
	want float64,
	labels map[string]string,
) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		if want == 0 {
			return
		}
		t.Fatalf("metric family %q is not registered, want %g", name, want)
		return
	}
	for _, metric := range family.GetMetric() {
		if !appMetricHasLabels(metric, labels) {
			continue
		}
		if got := metric.GetCounter().GetValue(); got != want {
			t.Fatalf("metric family %q labels %v value = %g, want %g", name, labels, got, want)
		}
		return
	}
	if want == 0 {
		return
	}
	t.Fatalf("metric family %q missing labels %v", name, labels)
}

func appMetricHasLabels(metric *dto.Metric, labels map[string]string) bool {
	for wantName, wantValue := range labels {
		found := false
		for _, label := range metric.GetLabel() {
			if label.GetName() == wantName && label.GetValue() == wantValue {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
