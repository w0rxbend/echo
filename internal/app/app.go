package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/config"
	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/integrations/httpapi"
	"github.com/worxbend/echo/internal/matrix"
	"github.com/worxbend/echo/internal/metrics"
	"github.com/worxbend/echo/internal/rules"
)

type App struct {
	cfg       config.Config
	logger    *slog.Logger
	metrics   *metrics.Registry
	bus       *events.Bus
	rules     eventMapper
	registry  *animations.Registry
	matrix    matrixClientCloser
	scheduler *matrix.Scheduler
	httpAPI   *httpapi.Server
	lifecycle appLifecycle

	eventWorker eventWorkerDiagnostics

	tcpReconnectLogs *tcpReconnectLogDispatcher
}

type eventMapper interface {
	Map(events.Event) (animations.AnimationRequest, bool)
}

type matrixClientCloser interface {
	matrix.Client
	Close() error
}

type matrixObservabilityPanicCounter interface {
	ObservabilityCallbackPanics() uint64
	ObservabilityCallbackPanicCounts() map[string]uint64
}

type appNewOptions struct {
	wrapReliableOutcomeSink func(func(matrix.OutcomeReport)) func(matrix.OutcomeReport)
}

type appNewOption interface {
	applyAppNewOption(*appNewOptions)
}

type appNewOptionFunc func(*appNewOptions)

func (f appNewOptionFunc) applyAppNewOption(options *appNewOptions) {
	f(options)
}

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	return newWithOptions(cfg, logger)
}

func newWithOptions(cfg config.Config, logger *slog.Logger, options ...appNewOption) (_ *App, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	newOptions := appNewOptions{}
	for _, option := range options {
		if option != nil {
			option.applyAppNewOption(&newOptions)
		}
	}

	var partial *App
	defer func() {
		if err == nil || partial == nil {
			return
		}
		err = errors.Join(err, partial.Close())
	}()

	registry, err := metrics.New()
	if err != nil {
		return nil, err
	}
	partial = &App{
		cfg:     cfg,
		logger:  logger,
		metrics: registry,
	}

	bus, err := events.NewBusWithOptions(cfg.Queue.EventsBuffer, events.BusOptions{
		OnPublishBackpressureWait: func(duration time.Duration) {
			registry.EventPublishBackpressureWait.Observe(duration.Seconds())
		},
		OnPublishBackpressureTimeout: func() {
			registry.EventPublishBackpressureTimeout.Inc()
		},
	})
	if err != nil {
		return nil, err
	}
	partial.bus = bus

	ruleEngine, err := rules.LoadFile(cfg.RulesFile)
	if err != nil {
		return nil, err
	}
	partial.rules = ruleEngine

	animationRegistry := cfg.AnimationRegistry
	if animationRegistry == nil {
		animationRegistry, err = animations.NewDefaultRegistry()
		if err != nil {
			return nil, err
		}
	}
	partial.registry = animationRegistry

	layout, err := animations.NewLayout(
		cfg.Matrix.Layout.Width,
		cfg.Matrix.Layout.Height,
		cfg.Matrix.Layout.Wiring,
		cfg.Matrix.Layout.OddRowDisplayFlip,
	)
	if err != nil {
		return nil, err
	}
	packer, err := animations.NewPacker(layout)
	if err != nil {
		return nil, err
	}

	tcpReconnectLogs := newTCPReconnectLogDispatcher(logger, 64)
	partial.tcpReconnectLogs = tcpReconnectLogs

	matrixClient, err := matrix.NewTCPClient(matrix.ClientOptions{
		Host:            cfg.Matrix.Host,
		Port:            cfg.Matrix.Port,
		ConnectTimeout:  cfg.Matrix.ConnectTimeout,
		ResponseTimeout: cfg.Matrix.ResponseTimeout,
		OnCommandDone: func(result matrix.CommandResult) {
			registry.MatrixCommandsTotal.WithLabelValues(result.Command, result.Status).Inc()
			registry.MatrixCommandDuration.WithLabelValues(result.Command).Observe(result.Duration.Seconds())
		},
		// TCP client observability callbacks run while its one-command-in-flight
		// mutex is held. Keep synchronous work to fast in-memory metrics; send
		// structured reconnect logs through the bounded dispatcher below.
		OnReconnectAttempt: func(attempt matrix.ReconnectAttempt) {
			recordReconnectAttempt(registry, attempt, false)
			tcpReconnectLogs.LogReconnectAttempt(attempt)
		},
		OnReconnectRecovered: func(recovery matrix.ReconnectRecovery) {
			recordReconnectRecovery(registry, recovery)
			tcpReconnectLogs.LogReconnectRecovered(recovery)
		},
		OnReconnectFailure: func(failure matrix.ReconnectFailure) {
			recordReconnectFailure(registry, failure)
			tcpReconnectLogs.LogReconnectFailure(failure)
		},
	})
	if err != nil {
		return nil, err
	}
	partial.matrix = matrixClient

	recordReliableOutcome := func(report matrix.OutcomeReport) {
		recordItemOutcomeMetric(registry, report)
	}
	if newOptions.wrapReliableOutcomeSink != nil {
		if wrapped := newOptions.wrapReliableOutcomeSink(recordReliableOutcome); wrapped != nil {
			recordReliableOutcome = wrapped
		}
	}

	scheduler, err := matrix.NewSchedulerWithReliableAppOutcomeRecorder(matrix.SchedulerOptions{
		Client:            matrixClient,
		Registry:          animationRegistry,
		Packer:            packer,
		QueueCapacity:     cfg.Queue.PlayBuffer,
		Background:        backgroundConfig(cfg),
		ReconnectMinDelay: cfg.Matrix.ReconnectMinDelay,
		ReconnectMaxDelay: cfg.Matrix.ReconnectMaxDelay,
		HeartbeatInterval: cfg.Matrix.HeartbeatInterval,
		ProbeTimeout:      cfg.Matrix.ProbeTimeout,
		OnReconnectDelay: func(attempt matrix.ReconnectAttempt) {
			recordReconnectAttempt(registry, attempt, true)
			logReconnectAttempt(logger, attempt)
		},
		OnReconnectRecovered: func(recovery matrix.ReconnectRecovery) {
			recordReconnectRecovery(registry, recovery)
			logReconnectRecovered(logger, recovery)
		},
		OnReconnectFailure: func(failure matrix.ReconnectFailure) {
			recordReconnectFailure(registry, failure)
			logReconnectFailure(logger, failure)
		},
		OnProbeFailure: func(failure matrix.ProbeFailure) {
			recordProbeFailure(registry, failure)
			logProbeFailure(logger, failure)
		},
		OnMatrixConnectedChange: func(connected bool) {
			setMatrixConnectedMetric(registry, connected)
		},
		OnAnimationRendered: func(result matrix.AnimationRenderResult) {
			registry.AnimationRenderDuration.WithLabelValues(result.AnimationID).Observe(result.Duration.Seconds())
		},
		OnBackgroundRestore: func(event matrix.BackgroundRestoreEvent) {
			recordBackgroundRestoreMetric(registry, event)
			logBackgroundRestore(logger, event)
		},
		OnItemOutcome: func(report matrix.OutcomeReport) {
			logItemOutcome(logger, report)
		},
		OnQueueDepthChange: func(depth int) {
			registry.PlayQueueDepth.Set(float64(depth))
		},
	}, recordReliableOutcome)
	if err != nil {
		return nil, err
	}
	partial.scheduler = scheduler
	if err := registry.RegisterPlayItemOutcomesDropped(func() float64 {
		return float64(scheduler.OutcomeReportsDropped())
	}); err != nil {
		return nil, err
	}
	if err := registry.RegisterPlayItemOutcomeRecordingPanics(func() float64 {
		return float64(scheduler.OutcomeRecordingPanics())
	}); err != nil {
		return nil, err
	}
	if err := registry.RegisterTCPReconnectLogEventsDropped(func() float64 {
		return float64(tcpReconnectLogs.EventsDropped())
	}); err != nil {
		return nil, err
	}
	for _, callback := range schedulerObservabilityCallbackNames() {
		callback := callback
		if err := registry.RegisterMatrixObservabilityCallbackPanics(string(matrix.ReconnectSourceSchedulerBackoff), callback, func() float64 {
			return float64(scheduler.ObservabilityCallbackPanicCounts()[callback])
		}); err != nil {
			return nil, err
		}
	}
	for _, callback := range tcpObservabilityCallbackNames() {
		callback := callback
		if err := registry.RegisterMatrixObservabilityCallbackPanics(string(matrix.ReconnectSourceTCPImmediate), callback, func() float64 {
			return float64(
				observabilityCallbackPanicCount(matrixClient, callback) +
					observabilityCallbackPanicCount(tcpReconnectLogs, callback),
			)
		}); err != nil {
			return nil, err
		}
	}

	httpAPI, err := httpapi.New(httpapi.Options{
		Logger:        logger,
		Bus:           bus,
		Scheduler:     scheduler,
		Registry:      animationRegistry,
		ServerAddr:    cfg.Server.Addr,
		AdminTokenEnv: cfg.Server.AdminTokenEnv,
	})
	if err != nil {
		return nil, err
	}
	partial.httpAPI = httpAPI

	return partial, nil
}

func (a *App) Handler() http.Handler {
	return a.router()
}

func (a *App) router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", a.handleHealth)
	r.Get("/readyz", a.handleReady)
	r.Handle("/metrics", http.HandlerFunc(a.handleMetrics))
	r.Mount("/api/v1", a.httpAPI.Router())

	return r
}

func (a *App) mapAndEnqueue(ctx context.Context, event events.Event) error {
	request, ok := a.rules.Map(event)
	if !ok {
		return errNoRuleMatch
	}
	applyEventOverrides(&request, event)
	return a.scheduler.EnqueueRequest(ctx, request)
}

func applyEventOverrides(request *animations.AnimationRequest, event events.Event) {
	if request == nil || len(event.Attributes) == 0 {
		return
	}
	if animationID := event.Attributes["animation"]; animationID != "" {
		request.AnimationID = animationID
	}
	if restore := event.Attributes["restore"]; restore != "" {
		request.RestorePolicy = animations.RestorePolicy(restore)
	}
	if duration := event.Attributes["duration"]; duration != "" {
		if parsed, err := time.ParseDuration(duration); err == nil {
			request.MaxDuration = parsed
		}
	}
	if event.Priority != 0 {
		request.Priority = event.Priority
	}
	if request.Params == nil {
		request.Params = animations.Params{}
	}
	for key, value := range event.Attributes {
		if name, ok := strings.CutPrefix(key, "param."); ok {
			request.Params[name] = value
		}
	}
}

var errNoRuleMatch = errors.New("no matching rule")

func backgroundConfig(cfg config.Config) matrix.BackgroundConfig {
	if cfg.Background.Animation == "" || !cfg.Background.RestoreOnIdle {
		return matrix.BackgroundConfig{}
	}
	return matrix.BackgroundConfig{AnimationID: cfg.Background.Animation}
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) handleReady(w http.ResponseWriter, r *http.Request) {
	body, ready := a.readiness()
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}

	writeJSON(w, http.StatusOK, body)
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	a.refreshBackgroundStateMetrics()
	promhttp.HandlerFor(a.metrics.Gatherer(), promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

func (a *App) isReady() bool {
	_, ready := a.readiness()
	return ready
}

type readyResponse struct {
	Status                       string            `json:"status"`
	WorkersRunning               bool              `json:"workers_running"`
	Draining                     bool              `json:"draining"`
	SchedulerState               matrix.State      `json:"scheduler_state"`
	MatrixConnected              bool              `json:"matrix_connected"`
	Background                   backgroundReady   `json:"background"`
	EventWorker                  eventWorkerReady  `json:"event_worker"`
	OutcomesDropped              uint64            `json:"outcome_reports_dropped"`
	OutcomeRecordingPanics       uint64            `json:"outcome_recording_panics"`
	TCPReconnectLogEventsDropped uint64            `json:"tcp_reconnect_log_events_dropped"`
	ObservabilityCallbackPanics  uint64            `json:"observability_callback_panics"`
	ObservabilityCallbackCounts  map[string]uint64 `json:"observability_callback_panic_counts,omitempty"`
	LastSuccess                  *time.Time        `json:"last_success,omitempty"`
	LastFailure                  *time.Time        `json:"last_failure,omitempty"`
}

type backgroundReady struct {
	ConfiguredID   string                            `json:"configured_id,omitempty"`
	Kind           matrix.BackgroundKind             `json:"kind,omitempty"`
	State          matrix.BackgroundConvergenceState `json:"state"`
	Dirty          bool                              `json:"dirty"`
	Converged      bool                              `json:"converged"`
	LastAttempt    *time.Time                        `json:"last_attempt,omitempty"`
	LastSuccess    *time.Time                        `json:"last_success,omitempty"`
	NextRetry      *time.Time                        `json:"next_retry,omitempty"`
	FailureCount   int                               `json:"failure_count"`
	LastError      string                            `json:"last_error,omitempty"`
	LastErrorClass matrix.ErrorKind                  `json:"last_error_class,omitempty"`
}

func (a *App) readiness() (readyResponse, bool) {
	lifecycle := a.lifecycle.snapshot()
	workersRunning := lifecycle.workersRunning
	draining := lifecycle.draining
	health := a.scheduler.Health()
	now := time.Now()
	background := backgroundConvergenceProjectionForApp(health, now)
	recordBackgroundHealthMetrics(a.metrics, health, now)

	ready := workersRunning &&
		!draining &&
		health.MatrixConnected &&
		health.State != matrix.StateDisconnected &&
		health.State != matrix.StateDraining

	status := "not_ready"
	if ready {
		status = "ready"
	}
	return readyResponse{
		Status:          status,
		WorkersRunning:  workersRunning,
		Draining:        draining,
		SchedulerState:  health.State,
		MatrixConnected: health.MatrixConnected,
		Background: backgroundReady{
			ConfiguredID:   health.BackgroundID,
			Kind:           health.BackgroundKind,
			State:          background.State,
			Dirty:          background.Dirty,
			Converged:      background.Converged,
			LastAttempt:    health.BackgroundLastRestoreAttempt,
			LastSuccess:    health.BackgroundLastRestoreSuccess,
			NextRetry:      health.BackgroundNextRetry,
			FailureCount:   health.BackgroundRetryFailureCount,
			LastError:      health.BackgroundLastRestoreError,
			LastErrorClass: health.BackgroundLastRestoreErrorClass,
		},
		EventWorker:                  a.eventWorker.snapshot(time.Now()),
		OutcomesDropped:              health.OutcomeReportsDropped,
		OutcomeRecordingPanics:       health.OutcomeRecordingPanics,
		TCPReconnectLogEventsDropped: tcpReconnectLogEventsDropped(a.tcpReconnectLogs),
		ObservabilityCallbackPanics: health.ObservabilityCallbackPanics +
			observabilityCallbackPanics(a.matrix) +
			observabilityCallbackPanics(a.tcpReconnectLogs),
		ObservabilityCallbackCounts: applicationObservabilityCallbackPanicCounts(a.scheduler, a.matrix, a.tcpReconnectLogs),
		LastSuccess:                 health.LastSuccess,
		LastFailure:                 health.LastFailure,
	}, ready
}

func tcpReconnectLogEventsDropped(dispatcher *tcpReconnectLogDispatcher) uint64 {
	if dispatcher == nil {
		return 0
	}
	return dispatcher.EventsDropped()
}

func observabilityCallbackPanics(counter any) uint64 {
	if counter, ok := counter.(matrixObservabilityPanicCounter); ok {
		return counter.ObservabilityCallbackPanics()
	}
	return 0
}

func observabilityCallbackPanicCount(counter any, callback string) uint64 {
	if counter, ok := counter.(matrixObservabilityPanicCounter); ok {
		return counter.ObservabilityCallbackPanicCounts()[callback]
	}
	return 0
}

func applicationObservabilityCallbackPanicCounts(
	scheduler *matrix.Scheduler,
	matrixClient any,
	tcpReconnectLogs any,
) map[string]uint64 {
	var counts map[string]uint64
	if scheduler != nil {
		counts = mergeObservabilityCallbackPanicCounts(counts, scheduler.ObservabilityCallbackPanicCounts())
	}
	if counter, ok := matrixClient.(matrixObservabilityPanicCounter); ok {
		counts = mergeObservabilityCallbackPanicCounts(counts, counter.ObservabilityCallbackPanicCounts())
	}
	if counter, ok := tcpReconnectLogs.(matrixObservabilityPanicCounter); ok {
		counts = mergeObservabilityCallbackPanicCounts(counts, counter.ObservabilityCallbackPanicCounts())
	}
	return counts
}

func mergeObservabilityCallbackPanicCounts(dst, src map[string]uint64) map[string]uint64 {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]uint64, len(src))
	}
	for callback, count := range src {
		dst[callback] += count
	}
	return dst
}

func (a *App) setMatrixConnectedMetric(connected bool) {
	setMatrixConnectedMetric(a.metrics, connected)
}

func setMatrixConnectedMetric(registry *metrics.Registry, connected bool) {
	if connected {
		registry.MatrixConnected.Set(1)
		return
	}
	registry.MatrixConnected.Set(0)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func logEnqueueError(logger *slog.Logger, event events.Event, err error) {
	if errors.Is(err, errNoRuleMatch) {
		logger.Debug("event did not match any rule", "event_id", event.ID, "source", event.Source, "type", event.Type)
		return
	}
	logger.Warn("enqueue event animation", "event_id", event.ID, "source", event.Source, "type", event.Type, "error", err)
}

func recordItemOutcomeMetric(registry *metrics.Registry, report matrix.OutcomeReport) {
	registry.PlayItemsTotal.WithLabelValues(
		string(report.ItemKind),
		outcomeMetricItem(report),
		string(report.Outcome),
	).Inc()
}

func recordReconnectAttempt(registry *metrics.Registry, attempt matrix.ReconnectAttempt, observeDelay bool) {
	registry.MatrixReconnectsTotal.WithLabelValues(string(attempt.Source), string(attempt.ErrorKind)).Inc()
	if observeDelay {
		registry.MatrixReconnectDelay.WithLabelValues(string(attempt.Source)).Observe(attempt.Delay.Seconds())
	}
}

func recordReconnectRecovery(registry *metrics.Registry, recovery matrix.ReconnectRecovery) {
	registry.MatrixReconnectRecoveriesTotal.WithLabelValues(string(recovery.Source), string(recovery.State)).Inc()
}

func recordReconnectFailure(registry *metrics.Registry, failure matrix.ReconnectFailure) {
	registry.MatrixReconnectFailuresTotal.WithLabelValues(
		string(failure.Source),
		string(failure.ErrorKind),
		string(failure.Outcome),
	).Inc()
}

func recordProbeFailure(registry *metrics.Registry, failure matrix.ProbeFailure) {
	registry.MatrixProbeFailuresTotal.WithLabelValues(string(failure.ErrorKind), string(failure.Reason)).Inc()
}

func recordBackgroundRestoreMetric(registry *metrics.Registry, event matrix.BackgroundRestoreEvent) {
	switch event.State {
	case matrix.BackgroundConvergenceAttempting:
		registry.BackgroundRestoreAttemptsTotal.WithLabelValues(string(event.Kind)).Inc()
		setBackgroundGaugeMetrics(registry, event.Kind, matrix.BackgroundConvergenceAttempting, true, false)
	case matrix.BackgroundConvergenceFailed, matrix.BackgroundConvergenceRetrying:
		registry.BackgroundRestoreFailuresTotal.WithLabelValues(string(event.Kind), string(event.ErrorKind)).Inc()
		setBackgroundGaugeMetrics(registry, event.Kind, event.State, true, false)
	}
}

func (a *App) refreshBackgroundStateMetrics() {
	if a == nil || a.scheduler == nil {
		return
	}
	recordBackgroundHealthMetrics(a.metrics, a.scheduler.Health(), time.Now())
}

func recordBackgroundHealthMetrics(registry *metrics.Registry, health matrix.Health, now time.Time) {
	background := backgroundConvergenceProjectionForApp(health, now)
	setBackgroundStateMetrics(
		registry,
		health.BackgroundKind,
		background.State,
		background.Dirty,
		background.Converged,
		health.BackgroundNextRetry,
		now,
	)
}

func backgroundConvergenceProjectionForApp(health matrix.Health, now time.Time) matrix.BackgroundConvergenceProjection {
	return matrix.ProjectBackgroundConvergence(matrix.BackgroundConvergenceProjectionInput{
		State:                 health.BackgroundConvergenceState,
		Dirty:                 health.BackgroundDirty,
		LastRestoreError:      health.BackgroundLastRestoreError,
		LastRestoreErrorClass: health.BackgroundLastRestoreErrorClass,
		NextRetry:             health.BackgroundNextRetry,
		FailureCount:          health.BackgroundRetryFailureCount,
	}, now)
}

var backgroundMetricStates = []matrix.BackgroundConvergenceState{
	matrix.BackgroundConvergenceUnknown,
	matrix.BackgroundConvergenceDirty,
	matrix.BackgroundConvergenceAttempting,
	matrix.BackgroundConvergenceConverged,
	matrix.BackgroundConvergenceFailed,
	matrix.BackgroundConvergenceRetrying,
}

func setBackgroundStateMetrics(
	registry *metrics.Registry,
	kind matrix.BackgroundKind,
	state matrix.BackgroundConvergenceState,
	dirty bool,
	converged bool,
	nextRetry *time.Time,
	now time.Time,
) {
	setBackgroundGaugeMetrics(registry, kind, state, dirty, converged)
	setBackgroundNextRetryMetric(registry, kind, nextRetry, now)
}

func setBackgroundGaugeMetrics(
	registry *metrics.Registry,
	kind matrix.BackgroundKind,
	state matrix.BackgroundConvergenceState,
	dirty bool,
	converged bool,
) {
	if registry == nil || kind == "" {
		return
	}
	if state == "" {
		state = matrix.BackgroundConvergenceUnknown
	}
	dirtyValue := 0.0
	if dirty {
		dirtyValue = 1
	}
	convergedValue := 0.0
	if converged {
		convergedValue = 1
	}
	registry.BackgroundDirty.WithLabelValues(string(kind)).Set(dirtyValue)
	registry.BackgroundConverged.WithLabelValues(string(kind)).Set(convergedValue)
	for _, metricState := range backgroundMetricStates {
		value := 0.0
		if state == metricState {
			value = 1
		}
		registry.BackgroundState.WithLabelValues(string(kind), string(metricState)).Set(value)
	}
}

func setBackgroundNextRetryMetric(registry *metrics.Registry, kind matrix.BackgroundKind, nextRetry *time.Time, now time.Time) {
	if registry == nil || kind == "" {
		return
	}
	nextRetrySeconds := 0.0
	if nextRetry != nil {
		nextRetrySeconds = nextRetry.Sub(now).Seconds()
		if nextRetrySeconds < 0 {
			nextRetrySeconds = 0
		}
	}
	registry.BackgroundNextRetrySeconds.WithLabelValues(string(kind)).Set(nextRetrySeconds)
}

func logItemOutcome(logger *slog.Logger, report matrix.OutcomeReport) {
	logger.Info("matrix item outcome",
		"outcome", report.Outcome,
		"item_kind", report.ItemKind,
		"item_id", report.ItemID,
		"event_id", report.EventID,
		"animation_id", report.AnimationID,
		"control_kind", report.ControlKind,
		"priority", report.Priority,
		"queue_depth_before_clear", report.QueueDepthBeforeClear,
		"queue_depth_at_admission", report.QueueDepthAtAdmission,
		"queue_depth_at_removal", report.QueueDepthAtRemoval,
		"reason", report.Reason,
		"error_class", report.ErrorClass,
		"timestamp", report.Timestamp,
	)
}

func logReconnectAttempt(logger *slog.Logger, attempt matrix.ReconnectAttempt) {
	attrs := []any{
		"source", attempt.Source,
		"attempt", attempt.Attempt,
		"error_kind", attempt.ErrorKind,
	}
	if attempt.BaseDelay > 0 {
		attrs = append(attrs, "base_delay", attempt.BaseDelay)
	}
	if attempt.Delay > 0 {
		attrs = append(attrs, "jittered_delay", attempt.Delay)
	}
	if attempt.DeadlineCapped {
		attrs = append(attrs, "deadline_capped", attempt.DeadlineCapped)
	}
	if attempt.Error != "" {
		attrs = append(attrs, "error", attempt.Error)
	}
	logger.Warn("matrix reconnect attempt", attrs...)
}

func logReconnectRecovered(logger *slog.Logger, recovery matrix.ReconnectRecovery) {
	logger.Info("matrix reconnect recovered",
		"source", recovery.Source,
		"attempt", recovery.Attempt,
		"state", recovery.State,
		"outcome", "connected",
	)
}

func logReconnectFailure(logger *slog.Logger, failure matrix.ReconnectFailure) {
	attrs := []any{
		"source", failure.Source,
		"attempt", failure.Attempt,
		"outcome", failure.Outcome,
		"error_kind", failure.ErrorKind,
	}
	if failure.Error != "" {
		attrs = append(attrs, "error", failure.Error)
	}
	logger.Warn("matrix reconnect failure", attrs...)
}

func logProbeFailure(logger *slog.Logger, failure matrix.ProbeFailure) {
	attrs := []any{
		"error_kind", failure.ErrorKind,
		"reason", failure.Reason,
	}
	if failure.Error != "" {
		attrs = append(attrs, "error", failure.Error)
	}
	logger.Warn("matrix probe failure", attrs...)
}

func logBackgroundRestore(logger *slog.Logger, event matrix.BackgroundRestoreEvent) {
	attrs := []any{
		"background_id", event.AnimationID,
		"kind", event.Kind,
		"state", event.State,
		"state_transition", event.State,
		"error_class", event.ErrorKind,
	}
	if event.Error != "" {
		attrs = append(attrs,
			"error", event.Error,
			"retry", "dirty background remains scheduled for retry",
		)
	}
	if event.State == matrix.BackgroundConvergenceAttempting {
		logger.Info("matrix background restore attempt", attrs...)
		return
	}
	logger.Warn("matrix background restore failure", attrs...)
}

func schedulerObservabilityCallbackNames() []string {
	return []string{
		matrix.ObservabilityCallbackReconnectDelay,
		matrix.ObservabilityCallbackReconnectRecovered,
		matrix.ObservabilityCallbackReconnectFailure,
		matrix.ObservabilityCallbackProbeFailure,
		matrix.ObservabilityCallbackMatrixConnectedChange,
		matrix.ObservabilityCallbackBackgroundRestore,
	}
}

func tcpObservabilityCallbackNames() []string {
	return []string{
		matrix.ObservabilityCallbackCommandDone,
		matrix.ObservabilityCallbackReconnectAttempt,
		matrix.ObservabilityCallbackReconnectRecovered,
		matrix.ObservabilityCallbackReconnectFailure,
	}
}

func outcomeMetricItem(report matrix.OutcomeReport) string {
	if report.ItemKind == matrix.QueueItemControl {
		if report.ControlKind != "" {
			return string(report.ControlKind)
		}
		return report.ItemID
	}
	if report.AnimationID != "" {
		return report.AnimationID
	}
	return report.ItemID
}

type tcpReconnectLogDispatcher struct {
	logger *slog.Logger
	events chan tcpReconnectLogEvent
	stop   chan struct{}

	closeOnce     sync.Once
	admissionMu   sync.RWMutex
	closed        bool
	eventsDropped atomic.Uint64

	observabilityMu                  sync.Mutex
	observabilityCallbackPanicCounts map[string]uint64
	observabilityCallbackPanics      atomic.Uint64
}

type tcpReconnectLogEvent struct {
	callback string
	log      func(*slog.Logger)
}

func newTCPReconnectLogDispatcher(logger *slog.Logger, capacity int) *tcpReconnectLogDispatcher {
	if capacity <= 0 {
		capacity = 1
	}
	dispatcher := &tcpReconnectLogDispatcher{
		logger: logger,
		events: make(chan tcpReconnectLogEvent, capacity),
		stop:   make(chan struct{}),
	}
	go dispatcher.run()
	return dispatcher
}

func (d *tcpReconnectLogDispatcher) LogReconnectAttempt(attempt matrix.ReconnectAttempt) {
	d.enqueue(tcpReconnectLogEvent{
		callback: matrix.ObservabilityCallbackReconnectAttempt,
		log: func(logger *slog.Logger) {
			logReconnectAttempt(logger, attempt)
		},
	})
}

func (d *tcpReconnectLogDispatcher) LogReconnectRecovered(recovery matrix.ReconnectRecovery) {
	d.enqueue(tcpReconnectLogEvent{
		callback: matrix.ObservabilityCallbackReconnectRecovered,
		log: func(logger *slog.Logger) {
			logReconnectRecovered(logger, recovery)
		},
	})
}

func (d *tcpReconnectLogDispatcher) LogReconnectFailure(failure matrix.ReconnectFailure) {
	d.enqueue(tcpReconnectLogEvent{
		callback: matrix.ObservabilityCallbackReconnectFailure,
		log: func(logger *slog.Logger) {
			logReconnectFailure(logger, failure)
		},
	})
}

func (d *tcpReconnectLogDispatcher) enqueue(event tcpReconnectLogEvent) {
	if d == nil || event.log == nil {
		return
	}
	if !d.admissionMu.TryRLock() {
		d.eventsDropped.Add(1)
		return
	}
	defer d.admissionMu.RUnlock()
	if d.closed {
		d.eventsDropped.Add(1)
		return
	}
	select {
	case d.events <- event:
	default:
		d.eventsDropped.Add(1)
	}
}

func (d *tcpReconnectLogDispatcher) run() {
	for {
		select {
		case <-d.stop:
			return
		case event := <-d.events:
			d.runEvent(event)
		}
	}
}

func (d *tcpReconnectLogDispatcher) runEvent(event tcpReconnectLogEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			d.recordObservabilityCallbackPanic(event.callback)
		}
	}()
	event.log(d.logger)
}

func (d *tcpReconnectLogDispatcher) Close() {
	if d == nil {
		return
	}
	// Close stops new admissions, but it cannot preempt a logger already
	// blocked inside slog.Handler.Handle on the dispatcher goroutine.
	d.closeOnce.Do(func() {
		d.admissionMu.Lock()
		defer d.admissionMu.Unlock()
		d.closed = true
		close(d.stop)
	})
}

func (d *tcpReconnectLogDispatcher) EventsDropped() uint64 {
	if d == nil {
		return 0
	}
	return d.eventsDropped.Load()
}

func (d *tcpReconnectLogDispatcher) ObservabilityCallbackPanics() uint64 {
	if d == nil {
		return 0
	}
	return d.observabilityCallbackPanics.Load()
}

func (d *tcpReconnectLogDispatcher) ObservabilityCallbackPanicCounts() map[string]uint64 {
	if d == nil {
		return nil
	}
	d.observabilityMu.Lock()
	defer d.observabilityMu.Unlock()
	if len(d.observabilityCallbackPanicCounts) == 0 {
		return nil
	}
	counts := make(map[string]uint64, len(d.observabilityCallbackPanicCounts))
	for name, count := range d.observabilityCallbackPanicCounts {
		counts[name] = count
	}
	return counts
}

func (d *tcpReconnectLogDispatcher) recordObservabilityCallbackPanic(name string) {
	d.observabilityCallbackPanics.Add(1)
	d.observabilityMu.Lock()
	defer d.observabilityMu.Unlock()
	if d.observabilityCallbackPanicCounts == nil {
		d.observabilityCallbackPanicCounts = make(map[string]uint64)
	}
	d.observabilityCallbackPanicCounts[name]++
}
