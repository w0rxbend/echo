package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

type Registry struct {
	registry *prometheus.Registry

	// Event-bus metrics are global (no device label) because the bus is shared.
	EventsTotal                     *prometheus.CounterVec
	EventsDroppedTotal              *prometheus.CounterVec
	EventQueueDepth                 prometheus.Gauge
	EventWorkerInflight             prometheus.Gauge
	EventPublishBackpressureWait    prometheus.Histogram
	EventPublishBackpressureTimeout prometheus.Counter

	// Per-device metrics all carry a "device" label.
	PlayItemsTotal                 *prometheus.CounterVec
	PlayQueueDepth                 *prometheus.GaugeVec
	MatrixCommandsTotal            *prometheus.CounterVec
	MatrixCommandDuration          *prometheus.HistogramVec
	MatrixReconnectsTotal          *prometheus.CounterVec
	MatrixReconnectDelay           *prometheus.HistogramVec
	MatrixReconnectRecoveriesTotal *prometheus.CounterVec
	MatrixReconnectFailuresTotal   *prometheus.CounterVec
	MatrixProbeFailuresTotal       *prometheus.CounterVec
	BackgroundRestoreAttemptsTotal *prometheus.CounterVec
	BackgroundRestoreFailuresTotal *prometheus.CounterVec
	BackgroundDirty                *prometheus.GaugeVec
	BackgroundConverged            *prometheus.GaugeVec
	BackgroundNextRetrySeconds     *prometheus.GaugeVec
	BackgroundState                *prometheus.GaugeVec
	MatrixConnected                *prometheus.GaugeVec
	AnimationRenderDuration        *prometheus.HistogramVec

	// CounterFuncs registered per-device at wire-up time.
	MatrixObservabilityPanicsTotal []prometheus.CounterFunc
	perDeviceCounterFuncs          []prometheus.CounterFunc
}

func New() (*Registry, error) {
	r := &Registry{registry: prometheus.NewRegistry()}

	r.EventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_events_total",
		Help: "Total normalized events accepted by the proxy.",
	}, []string{"source", "type"})
	r.EventsDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_events_dropped_total",
		Help: "Total normalized events dropped by reason.",
	}, []string{"reason"})
	r.EventQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_proxy_event_queue_depth",
		Help: "Current number of normalized events waiting in the app event-worker subscriber channel.",
	})
	r.EventWorkerInflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_proxy_event_worker_inflight",
		Help: "Whether the app event worker is actively processing one received event: 1 active, 0 idle.",
	})
	r.EventPublishBackpressureWait = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "matrix_proxy_event_publish_backpressure_duration_seconds",
		Help:    "Total time event publishers spent blocked behind full subscriber channels while publishing normalized events.",
		Buckets: prometheus.DefBuckets,
	})
	r.EventPublishBackpressureTimeout = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "matrix_proxy_event_publish_backpressure_timeouts_total",
		Help: "Total event publish attempts that failed because the publish context expired while blocked behind subscriber backpressure.",
	})

	r.PlayItemsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_play_items_total",
		Help: "Total matrix play items by device, item kind, item name, and terminal outcome.",
	}, []string{"device", "item_kind", "item", "outcome"})
	r.PlayQueueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_play_queue_depth",
		Help: "Current number of matrix play items waiting to run, by device.",
	}, []string{"device"})
	r.MatrixCommandsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_commands_total",
		Help: "Total TCP command frame attempts sent to the matrix controller by device, command name, and status.",
	}, []string{"device", "command", "status"})
	r.MatrixCommandDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matrix_proxy_matrix_command_duration_seconds",
		Help:    "Matrix command round-trip duration in seconds by device.",
		Buckets: prometheus.DefBuckets,
	}, []string{"device", "command"})
	r.MatrixReconnectsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_reconnects_total",
		Help: "Total matrix reconnect attempts started by device, bounded source, and error kind.",
	}, []string{"device", "source", "error_kind"})
	r.MatrixReconnectDelay = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matrix_proxy_matrix_reconnect_delay_seconds",
		Help:    "Matrix scheduler reconnect backoff delay in seconds by device and bounded reconnect source.",
		Buckets: prometheus.DefBuckets,
	}, []string{"device", "source"})
	r.MatrixReconnectRecoveriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_reconnect_recoveries_total",
		Help: "Total matrix reconnect attempts that reached firmware-verified replacement connectivity by device, bounded source, and resulting scheduler state. For non-ping tcp_immediate commands, a later retried-command transport failure is command telemetry, not proof that reconnect verification failed.",
	}, []string{"device", "source", "state"})
	r.MatrixReconnectFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_reconnect_failures_total",
		Help: "Total terminal matrix reconnect failures before firmware-verified replacement connectivity by device, bounded source, error kind, and outcome. outcome=verification_failed means a replacement connection opened but firmware status/protocol/validation verification failed; outcome=failed with error_kind=retryable covers reconnect transport or dial failure.",
	}, []string{"device", "source", "error_kind", "outcome"})
	r.MatrixProbeFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_probe_failures_total",
		Help: "Total matrix probe failures by device, error kind, and bounded failure reason.",
	}, []string{"device", "error_kind", "reason"})
	r.BackgroundRestoreAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_background_restore_attempts_total",
		Help: "Total scheduler-owned desired-background restore attempts by device and bounded background kind.",
	}, []string{"device", "kind"})
	r.BackgroundRestoreFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_background_restore_failures_total",
		Help: "Total scheduler-owned desired-background restore failures by device, bounded background kind, and error class.",
	}, []string{"device", "kind", "error_class"})
	r.BackgroundDirty = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_dirty",
		Help: "Whether the configured background is currently dirty by device and bounded background kind: 1 dirty, 0 clean.",
	}, []string{"device", "kind"})
	r.BackgroundConverged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_converged",
		Help: "Whether the configured background is known converged by device and bounded background kind: 1 converged, 0 not converged.",
	}, []string{"device", "kind"})
	r.BackgroundNextRetrySeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_next_retry_seconds",
		Help: "Seconds until the next configured background retry by device and bounded background kind, or 0 when no retry is pending.",
	}, []string{"device", "kind"})
	r.BackgroundState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_state",
		Help: "One-hot current configured background convergence state by device, bounded background kind, and state.",
	}, []string{"device", "kind", "state"})
	r.MatrixConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_matrix_connected",
		Help: "Whether the matrix controller is currently connected by device: 1 connected, 0 disconnected.",
	}, []string{"device"})
	r.AnimationRenderDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matrix_proxy_animation_render_duration_seconds",
		Help:    "Animation render duration in seconds by device.",
		Buckets: prometheus.DefBuckets,
	}, []string{"device", "animation"})

	collectors := []prometheus.Collector{
		r.EventsTotal,
		r.EventsDroppedTotal,
		r.EventQueueDepth,
		r.EventWorkerInflight,
		r.EventPublishBackpressureWait,
		r.EventPublishBackpressureTimeout,
		r.PlayItemsTotal,
		r.PlayQueueDepth,
		r.MatrixCommandsTotal,
		r.MatrixCommandDuration,
		r.MatrixReconnectsTotal,
		r.MatrixReconnectDelay,
		r.MatrixReconnectRecoveriesTotal,
		r.MatrixReconnectFailuresTotal,
		r.MatrixProbeFailuresTotal,
		r.BackgroundRestoreAttemptsTotal,
		r.BackgroundRestoreFailuresTotal,
		r.BackgroundDirty,
		r.BackgroundConverged,
		r.BackgroundNextRetrySeconds,
		r.BackgroundState,
		r.MatrixConnected,
		r.AnimationRenderDuration,
	}
	for _, c := range collectors {
		if err := r.registry.Register(c); err != nil {
			return nil, err
		}
	}

	return r, nil
}

func (r *Registry) Gatherer() prometheus.Gatherer {
	return r.registry
}

func (r *Registry) RegisterPlayItemOutcomesDropped(deviceID string, value func() float64) error {
	counter := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "matrix_proxy_play_item_outcomes_dropped_total",
		Help:        "Total terminal matrix play item outcome reports dropped before external observer delivery.",
		ConstLabels: prometheus.Labels{"device": deviceID},
	}, value)
	if err := r.registry.Register(counter); err != nil {
		return err
	}
	r.perDeviceCounterFuncs = append(r.perDeviceCounterFuncs, counter)
	return nil
}

func (r *Registry) RegisterPlayItemOutcomeRecordingPanics(deviceID string, value func() float64) error {
	counter := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "matrix_proxy_play_item_outcome_recording_panics_total",
		Help:        "Total panics recovered while recording terminal matrix play item outcomes through the reliable scheduler sink.",
		ConstLabels: prometheus.Labels{"device": deviceID},
	}, value)
	if err := r.registry.Register(counter); err != nil {
		return err
	}
	r.perDeviceCounterFuncs = append(r.perDeviceCounterFuncs, counter)
	return nil
}

func (r *Registry) RegisterTCPReconnectLogEventsDropped(deviceID string, value func() float64) error {
	counter := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "matrix_proxy_tcp_reconnect_log_events_dropped_total",
		Help:        "Total best-effort TCP reconnect log events dropped before slog handling because the dispatcher queue was full or closed.",
		ConstLabels: prometheus.Labels{"device": deviceID},
	}, value)
	if err := r.registry.Register(counter); err != nil {
		return err
	}
	r.perDeviceCounterFuncs = append(r.perDeviceCounterFuncs, counter)
	return nil
}

func (r *Registry) RegisterMatrixObservabilityCallbackPanics(deviceID, source, callback string, value func() float64) error {
	counter := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "matrix_proxy_matrix_observability_callback_panics_total",
		Help:        "Total panics recovered from matrix observability callbacks by device, bounded source, and callback name.",
		ConstLabels: prometheus.Labels{"device": deviceID, "source": source, "callback": callback},
	}, value)
	if err := r.registry.Register(counter); err != nil {
		return err
	}
	r.MatrixObservabilityPanicsTotal = append(r.MatrixObservabilityPanicsTotal, counter)
	return nil
}
