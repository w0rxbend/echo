package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

type Registry struct {
	registry *prometheus.Registry

	EventsTotal                     *prometheus.CounterVec
	EventsDroppedTotal              *prometheus.CounterVec
	PlayItemsTotal                  *prometheus.CounterVec
	PlayItemOutcomesDropped         prometheus.CounterFunc
	PlayItemOutcomeRecordingPanics  prometheus.CounterFunc
	TCPReconnectLogEventsDropped    prometheus.CounterFunc
	PlayQueueDepth                  prometheus.Gauge
	EventQueueDepth                 prometheus.Gauge
	EventWorkerInflight             prometheus.Gauge
	EventPublishBackpressureWait    prometheus.Histogram
	EventPublishBackpressureTimeout prometheus.Counter
	MatrixCommandsTotal             *prometheus.CounterVec
	MatrixCommandDuration           *prometheus.HistogramVec
	MatrixReconnectsTotal           *prometheus.CounterVec
	MatrixReconnectDelay            *prometheus.HistogramVec
	MatrixReconnectRecoveriesTotal  *prometheus.CounterVec
	MatrixReconnectFailuresTotal    *prometheus.CounterVec
	MatrixProbeFailuresTotal        *prometheus.CounterVec
	BackgroundRestoreAttemptsTotal  *prometheus.CounterVec
	BackgroundRestoreFailuresTotal  *prometheus.CounterVec
	BackgroundDirty                 *prometheus.GaugeVec
	BackgroundConverged             *prometheus.GaugeVec
	BackgroundNextRetrySeconds      *prometheus.GaugeVec
	BackgroundState                 *prometheus.GaugeVec
	MatrixObservabilityPanicsTotal  []prometheus.CounterFunc
	MatrixConnected                 prometheus.Gauge
	AnimationRenderDuration         *prometheus.HistogramVec
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
	r.PlayItemsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_play_items_total",
		Help: "Total matrix play items by item kind, item name, and terminal outcome.",
	}, []string{"item_kind", "item", "outcome"})
	r.PlayQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_proxy_play_queue_depth",
		Help: "Current number of matrix play items waiting to run.",
	})
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
	r.MatrixCommandsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_commands_total",
		Help: "Total TCP command frame attempts sent to the matrix controller by command name and status; this is not a count of logical scheduler commands.",
	}, []string{"command", "status"})
	r.MatrixCommandDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matrix_proxy_matrix_command_duration_seconds",
		Help:    "Matrix command round-trip duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"command"})
	r.MatrixReconnectsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_reconnects_total",
		Help: "Total matrix reconnect attempts started by bounded source and error kind. Sources include tcp_immediate socket-error reconnects and scheduler_backoff delayed reconnect loops.",
	}, []string{"source", "error_kind"})
	r.MatrixReconnectDelay = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matrix_proxy_matrix_reconnect_delay_seconds",
		Help:    "Matrix scheduler reconnect backoff delay in seconds by bounded reconnect source; tcp_immediate attempts do not wait on this delay.",
		Buckets: prometheus.DefBuckets,
	}, []string{"source"})
	r.MatrixReconnectRecoveriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_reconnect_recoveries_total",
		Help: "Total matrix reconnect attempts that reached firmware-verified replacement connectivity by bounded source and resulting scheduler state. For non-ping tcp_immediate commands, a later retried-command transport failure is command telemetry, not proof that reconnect verification failed.",
	}, []string{"source", "state"})
	r.MatrixReconnectFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_reconnect_failures_total",
		Help: "Total terminal matrix reconnect failures before firmware-verified replacement connectivity by bounded source, error kind, and outcome. outcome=verification_failed means a replacement connection opened but firmware status/protocol/validation verification failed; outcome=failed with error_kind=retryable covers reconnect transport or dial failure.",
	}, []string{"source", "error_kind", "outcome"})
	r.MatrixProbeFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_matrix_probe_failures_total",
		Help: "Total matrix probe failures by error kind and bounded failure reason.",
	}, []string{"error_kind", "reason"})
	r.BackgroundRestoreAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_background_restore_attempts_total",
		Help: "Total scheduler-owned desired-background restore attempts by bounded background kind.",
	}, []string{"kind"})
	r.BackgroundRestoreFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_proxy_background_restore_failures_total",
		Help: "Total scheduler-owned desired-background restore failures by bounded background kind and error class.",
	}, []string{"kind", "error_class"})
	r.BackgroundDirty = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_dirty",
		Help: "Whether the configured background is currently dirty by bounded background kind: 1 dirty, 0 clean.",
	}, []string{"kind"})
	r.BackgroundConverged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_converged",
		Help: "Whether the configured background is known converged by bounded background kind: 1 converged, 0 not converged.",
	}, []string{"kind"})
	r.BackgroundNextRetrySeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_next_retry_seconds",
		Help: "Seconds until the next configured background retry by bounded background kind, or 0 when no retry is pending.",
	}, []string{"kind"})
	r.BackgroundState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "matrix_proxy_background_state",
		Help: "One-hot current configured background convergence state by bounded background kind and state.",
	}, []string{"kind", "state"})
	r.MatrixConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_proxy_matrix_connected",
		Help: "Whether the matrix controller is currently connected: 1 connected, 0 disconnected.",
	})
	r.AnimationRenderDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matrix_proxy_animation_render_duration_seconds",
		Help:    "Animation render duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"animation"})

	if err := r.registry.Register(r.EventsTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.EventsDroppedTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.PlayItemsTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.PlayQueueDepth); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.EventQueueDepth); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.EventWorkerInflight); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.EventPublishBackpressureWait); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.EventPublishBackpressureTimeout); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixCommandsTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixCommandDuration); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixReconnectsTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixReconnectDelay); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixReconnectRecoveriesTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixReconnectFailuresTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixProbeFailuresTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.BackgroundRestoreAttemptsTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.BackgroundRestoreFailuresTotal); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.BackgroundDirty); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.BackgroundConverged); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.BackgroundNextRetrySeconds); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.BackgroundState); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.MatrixConnected); err != nil {
		return nil, err
	}
	if err := r.registry.Register(r.AnimationRenderDuration); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Registry) Gatherer() prometheus.Gatherer {
	return r.registry
}

func (r *Registry) RegisterPlayItemOutcomesDropped(value func() float64) error {
	r.PlayItemOutcomesDropped = prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "matrix_proxy_play_item_outcomes_dropped_total",
		Help: "Total terminal matrix play item outcome reports dropped before external observer delivery.",
	}, value)
	return r.registry.Register(r.PlayItemOutcomesDropped)
}

func (r *Registry) RegisterPlayItemOutcomeRecordingPanics(value func() float64) error {
	r.PlayItemOutcomeRecordingPanics = prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "matrix_proxy_play_item_outcome_recording_panics_total",
		Help: "Total panics recovered while recording terminal matrix play item outcomes through the reliable scheduler sink, distinct from best-effort observer delivery drops.",
	}, value)
	return r.registry.Register(r.PlayItemOutcomeRecordingPanics)
}

func (r *Registry) RegisterTCPReconnectLogEventsDropped(value func() float64) error {
	r.TCPReconnectLogEventsDropped = prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "matrix_proxy_tcp_reconnect_log_events_dropped_total",
		Help: "Total best-effort TCP reconnect log events dropped before slog handling because the dispatcher queue was full or closed.",
	}, value)
	return r.registry.Register(r.TCPReconnectLogEventsDropped)
}

func (r *Registry) RegisterMatrixObservabilityCallbackPanics(source, callback string, value func() float64) error {
	counter := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "matrix_proxy_matrix_observability_callback_panics_total",
		Help:        "Total panics recovered from matrix observability callbacks by bounded source and callback name.",
		ConstLabels: prometheus.Labels{"source": source, "callback": callback},
	}, value)
	if err := r.registry.Register(counter); err != nil {
		return err
	}
	r.MatrixObservabilityPanicsTotal = append(r.MatrixObservabilityPanicsTotal, counter)
	return nil
}
