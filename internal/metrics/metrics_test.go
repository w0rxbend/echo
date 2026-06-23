package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestEventPublishBackpressureMetricsAreTotalOnlyAndStableFamilies(t *testing.T) {
	registry, err := New()
	if err != nil {
		t.Fatal(err)
	}

	registry.EventPublishBackpressureWait.Observe(0.01)
	registry.EventPublishBackpressureTimeout.Inc()

	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := metricFamiliesByName(families)

	assertMetricFamilyHasNoLabels(t, byName, "matrix_proxy_event_publish_backpressure_duration_seconds")
	assertMetricFamilyHasNoLabels(t, byName, "matrix_proxy_event_publish_backpressure_timeouts_total")
	if _, ok := byName["matrix_proxy_event_publish_backpressure_wait_seconds"]; ok {
		t.Fatal("obsolete matrix_proxy_event_publish_backpressure_wait_seconds family is registered")
	}
}

func TestBackgroundRestoreMetricsUseBoundedLabelsAndSeparateFamilies(t *testing.T) {
	registry, err := New()
	if err != nil {
		t.Fatal(err)
	}

	registry.BackgroundRestoreAttemptsTotal.WithLabelValues("firmware_preset").Inc()
	registry.BackgroundRestoreFailuresTotal.WithLabelValues("generated", "retryable").Inc()
	registry.PlayItemsTotal.WithLabelValues("animation", "notification", "executed").Inc()

	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := metricFamiliesByName(families)

	assertMetricFamilyLabelNames(t, byName, "matrix_proxy_background_restore_attempts_total", []string{"kind"})
	assertMetricFamilyLabelNames(t, byName, "matrix_proxy_background_restore_failures_total", []string{"error_class", "kind"})
	assertMetricFamilyHasNoLabelValue(t, byName, "matrix_proxy_background_restore_attempts_total", "kind", "renderable")
	assertMetricFamilyHasNoLabelValue(t, byName, "matrix_proxy_background_restore_failures_total", "kind", "renderable")
	if family := byName["matrix_proxy_play_items_total"]; family == nil {
		t.Fatal("matrix_proxy_play_items_total is not registered")
	} else if got := len(family.GetMetric()); got != 1 {
		t.Fatalf("play item metric series = %d, want only explicitly recorded play item series", got)
	}
}

func TestBackgroundStateGaugesUseBoundedKindOnly(t *testing.T) {
	registry, err := New()
	if err != nil {
		t.Fatal(err)
	}

	registry.BackgroundDirty.WithLabelValues("firmware_preset").Set(1)
	registry.BackgroundConverged.WithLabelValues("generated").Set(0)
	registry.BackgroundNextRetrySeconds.WithLabelValues("firmware_preset").Set(12)
	for _, state := range []string{"unknown", "dirty", "attempting", "converged", "failed", "retrying"} {
		value := 0.0
		if state == "retrying" {
			value = 1
		}
		registry.BackgroundState.WithLabelValues("firmware_preset", state).Set(value)
	}

	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := metricFamiliesByName(families)

	assertMetricFamilyHelp(t, byName, "matrix_proxy_background_dirty",
		"Whether the configured background is currently dirty by bounded background kind: 1 dirty, 0 clean.")
	assertMetricFamilyHelp(t, byName, "matrix_proxy_background_converged",
		"Whether the configured background is known converged by bounded background kind: 1 converged, 0 not converged.")
	assertMetricFamilyHelp(t, byName, "matrix_proxy_background_next_retry_seconds",
		"Seconds until the next configured background retry by bounded background kind, or 0 when no retry is pending.")
	assertMetricFamilyHelp(t, byName, "matrix_proxy_background_state",
		"One-hot current configured background convergence state by bounded background kind and state.")
	assertMetricFamilyType(t, byName, "matrix_proxy_background_dirty", dto.MetricType_GAUGE)
	assertMetricFamilyType(t, byName, "matrix_proxy_background_converged", dto.MetricType_GAUGE)
	assertMetricFamilyType(t, byName, "matrix_proxy_background_next_retry_seconds", dto.MetricType_GAUGE)
	assertMetricFamilyType(t, byName, "matrix_proxy_background_state", dto.MetricType_GAUGE)
	assertMetricFamilyLabelNames(t, byName, "matrix_proxy_background_dirty", []string{"kind"})
	assertMetricFamilyLabelNames(t, byName, "matrix_proxy_background_converged", []string{"kind"})
	assertMetricFamilyLabelNames(t, byName, "matrix_proxy_background_next_retry_seconds", []string{"kind"})
	assertMetricFamilyLabelNames(t, byName, "matrix_proxy_background_state", []string{"kind", "state"})
	assertMetricFamilyHasNoLabelNames(t, byName, "matrix_proxy_background_dirty", "background_id", "animation", "id")
	assertMetricFamilyHasNoLabelNames(t, byName, "matrix_proxy_background_converged", "background_id", "animation", "id")
	assertMetricFamilyHasNoLabelNames(t, byName, "matrix_proxy_background_next_retry_seconds", "background_id", "animation", "id")
	assertMetricFamilyHasNoLabelNames(t, byName, "matrix_proxy_background_state", "background_id", "animation", "id")
	assertMetricFamilyHasNoLabelValue(t, byName, "matrix_proxy_background_dirty", "kind", "renderable")
	assertMetricFamilyHasNoLabelValue(t, byName, "matrix_proxy_background_converged", "kind", "renderable")
	assertMetricFamilyHasNoLabelValue(t, byName, "matrix_proxy_background_next_retry_seconds", "kind", "renderable")
	assertMetricFamilyHasNoLabelValue(t, byName, "matrix_proxy_background_state", "kind", "renderable")
	assertMetricFamilyGaugeValue(t, byName, "matrix_proxy_background_state", 1, map[string]string{
		"kind":  "firmware_preset",
		"state": "retrying",
	})
	for _, state := range []string{"unknown", "dirty", "attempting", "converged", "failed"} {
		assertMetricFamilyGaugeValue(t, byName, "matrix_proxy_background_state", 0, map[string]string{
			"kind":  "firmware_preset",
			"state": state,
		})
	}
}

func assertMetricFamilyHasNoLabelValue(t *testing.T, families map[string]*dto.MetricFamily, name, labelName, forbidden string) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	for _, metric := range family.GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == labelName && label.GetValue() == forbidden {
				t.Fatalf("metric family %q has forbidden label %s=%q in %v", name, labelName, forbidden, metric.GetLabel())
			}
		}
	}
}

func metricFamiliesByName(families []*dto.MetricFamily) map[string]*dto.MetricFamily {
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	return byName
}

func assertMetricFamilyHelp(t *testing.T, families map[string]*dto.MetricFamily, name, want string) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	if got := family.GetHelp(); got != want {
		t.Fatalf("metric family %q help = %q, want %q", name, got, want)
	}
}

func assertMetricFamilyType(t *testing.T, families map[string]*dto.MetricFamily, name string, want dto.MetricType) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	if got := family.GetType(); got != want {
		t.Fatalf("metric family %q type = %s, want %s", name, got, want)
	}
}

func assertMetricFamilyLabelNames(t *testing.T, families map[string]*dto.MetricFamily, name string, want []string) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	metrics := family.GetMetric()
	if len(metrics) == 0 {
		t.Fatalf("metric family %q has no samples", name)
	}
	gotLabels := metrics[0].GetLabel()
	if len(gotLabels) != len(want) {
		t.Fatalf("metric family %q labels = %v, want %v", name, gotLabels, want)
	}
	for i, label := range gotLabels {
		if label.GetName() != want[i] {
			t.Fatalf("metric family %q label names = %v, want %v", name, gotLabels, want)
		}
	}
}

func assertMetricFamilyHasNoLabelNames(t *testing.T, families map[string]*dto.MetricFamily, name string, forbidden ...string) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	forbiddenSet := make(map[string]struct{}, len(forbidden))
	for _, label := range forbidden {
		forbiddenSet[label] = struct{}{}
	}
	for _, metric := range family.GetMetric() {
		for _, label := range metric.GetLabel() {
			if _, ok := forbiddenSet[label.GetName()]; ok {
				t.Fatalf("metric family %q has forbidden label %q in %v", name, label.GetName(), metric.GetLabel())
			}
		}
	}
}

func assertMetricFamilyHasNoLabels(t *testing.T, families map[string]*dto.MetricFamily, name string) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q is not registered", name)
	}
	metrics := family.GetMetric()
	if len(metrics) == 0 {
		t.Fatalf("metric family %q has no samples", name)
	}
	for _, metric := range metrics {
		if labels := metric.GetLabel(); len(labels) != 0 {
			t.Fatalf("metric family %q labels = %v, want none", name, labels)
		}
	}
}

func assertMetricFamilyGaugeValue(
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
		if !metricHasLabels(metric, labels) {
			continue
		}
		if got := metric.GetGauge().GetValue(); got != want {
			t.Fatalf("metric family %q labels %v value = %g, want %g", name, labels, got, want)
		}
		return
	}
	t.Fatalf("metric family %q missing labels %v", name, labels)
}

func metricHasLabels(metric *dto.Metric, labels map[string]string) bool {
	for key, want := range labels {
		found := false
		for _, label := range metric.GetLabel() {
			if label.GetName() == key && label.GetValue() == want {
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
