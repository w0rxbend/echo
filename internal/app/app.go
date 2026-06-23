package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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

// appDevice holds all per-device runtime state: TCP client, scheduler, and reconnect log dispatcher.
type appDevice struct {
	id        string
	client    matrixClientCloser
	scheduler *matrix.Scheduler
	tcpLogs   *tcpReconnectLogDispatcher
}

type App struct {
	cfg       config.Config
	logger    *slog.Logger
	metrics   *metrics.Registry
	bus       *events.Bus
	rules     eventMapper
	registry  *animations.Registry
	devices   []*appDevice // ordered by device ID for deterministic iteration
	httpAPI   *httpapi.Server
	lifecycle appLifecycle

	eventWorker eventWorkerDiagnostics
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

	// Build per-device shards in sorted order for deterministic metrics/logging.
	deviceIDs := make([]string, 0, len(cfg.Devices))
	for id := range cfg.Devices {
		deviceIDs = append(deviceIDs, id)
	}
	sort.Strings(deviceIDs)

	schedulers := make(map[string]*matrix.Scheduler, len(deviceIDs))

	for _, id := range deviceIDs {
		devCfg := cfg.Devices[id]

		layout, err := animations.NewLayout(
			devCfg.Layout.Width,
			devCfg.Layout.Height,
			devCfg.Layout.Wiring,
			devCfg.Layout.OddRowDisplayFlip,
		)
		if err != nil {
			return nil, err
		}
		packer, err := animations.NewPacker(layout)
		if err != nil {
			return nil, err
		}

		tcpLogs := newTCPReconnectLogDispatcher(logger, 64)

		deviceID := id // capture for closures
		matrixClient, err := matrix.NewTCPClient(matrix.ClientOptions{
			Host:            devCfg.Host,
			Port:            devCfg.Port,
			ConnectTimeout:  devCfg.ConnectTimeout,
			ResponseTimeout: devCfg.ResponseTimeout,
			OnCommandDone: func(result matrix.CommandResult) {
				registry.MatrixCommandsTotal.WithLabelValues(deviceID, result.Command, result.Status).Inc()
				registry.MatrixCommandDuration.WithLabelValues(deviceID, result.Command).Observe(result.Duration.Seconds())
			},
			OnReconnectAttempt: func(attempt matrix.ReconnectAttempt) {
				recordReconnectAttempt(registry, deviceID, attempt, false)
				tcpLogs.LogReconnectAttempt(attempt)
			},
			OnReconnectRecovered: func(recovery matrix.ReconnectRecovery) {
				recordReconnectRecovery(registry, deviceID, recovery)
				tcpLogs.LogReconnectRecovered(recovery)
			},
			OnReconnectFailure: func(failure matrix.ReconnectFailure) {
				recordReconnectFailure(registry, deviceID, failure)
				tcpLogs.LogReconnectFailure(failure)
			},
		})
		if err != nil {
			return nil, err
		}

		recordReliableOutcome := func(report matrix.OutcomeReport) {
			recordItemOutcomeMetric(registry, deviceID, report)
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
			Background:        backgroundConfig(*devCfg),
			ReconnectMinDelay: devCfg.ReconnectMinDelay,
			ReconnectMaxDelay: devCfg.ReconnectMaxDelay,
			HeartbeatInterval: devCfg.HeartbeatInterval,
			ProbeTimeout:      devCfg.ProbeTimeout,
			OnReconnectDelay: func(attempt matrix.ReconnectAttempt) {
				recordReconnectAttempt(registry, deviceID, attempt, true)
				logReconnectAttempt(logger, deviceID, attempt)
			},
			OnReconnectRecovered: func(recovery matrix.ReconnectRecovery) {
				recordReconnectRecovery(registry, deviceID, recovery)
				logReconnectRecovered(logger, deviceID, recovery)
			},
			OnReconnectFailure: func(failure matrix.ReconnectFailure) {
				recordReconnectFailure(registry, deviceID, failure)
				logReconnectFailure(logger, deviceID, failure)
			},
			OnProbeFailure: func(failure matrix.ProbeFailure) {
				recordProbeFailure(registry, deviceID, failure)
				logProbeFailure(logger, deviceID, failure)
			},
			OnMatrixConnectedChange: func(connected bool) {
				setMatrixConnectedMetric(registry, deviceID, connected)
			},
			OnAnimationRendered: func(result matrix.AnimationRenderResult) {
				registry.AnimationRenderDuration.WithLabelValues(deviceID, result.AnimationID).Observe(result.Duration.Seconds())
			},
			OnBackgroundRestore: func(event matrix.BackgroundRestoreEvent) {
				recordBackgroundRestoreMetric(registry, deviceID, event)
				logBackgroundRestore(logger, deviceID, event)
			},
			OnItemOutcome: func(report matrix.OutcomeReport) {
				logItemOutcome(logger, deviceID, report)
			},
			OnQueueDepthChange: func(depth int) {
				registry.PlayQueueDepth.WithLabelValues(deviceID).Set(float64(depth))
			},
		}, recordReliableOutcome)
		if err != nil {
			return nil, err
		}

		// Initialize per-device gauge series so they appear in /metrics from the start.
		registry.PlayQueueDepth.WithLabelValues(id).Set(0)
		setMatrixConnectedMetric(registry, id, false)

		if err := registry.RegisterPlayItemOutcomesDropped(id, func() float64 {
			return float64(scheduler.OutcomeReportsDropped())
		}); err != nil {
			return nil, err
		}
		if err := registry.RegisterPlayItemOutcomeRecordingPanics(id, func() float64 {
			return float64(scheduler.OutcomeRecordingPanics())
		}); err != nil {
			return nil, err
		}
		if err := registry.RegisterTCPReconnectLogEventsDropped(id, func() float64 {
			return float64(tcpLogs.EventsDropped())
		}); err != nil {
			return nil, err
		}
		for _, cb := range schedulerObservabilityCallbackNames() {
			cb := cb
			if err := registry.RegisterMatrixObservabilityCallbackPanics(id, string(matrix.ReconnectSourceSchedulerBackoff), cb, func() float64 {
				return float64(scheduler.ObservabilityCallbackPanicCounts()[cb])
			}); err != nil {
				return nil, err
			}
		}
		for _, cb := range tcpObservabilityCallbackNames() {
			cb := cb
			if err := registry.RegisterMatrixObservabilityCallbackPanics(id, string(matrix.ReconnectSourceTCPImmediate), cb, func() float64 {
				return float64(
					observabilityCallbackPanicCount(matrixClient, cb) +
						observabilityCallbackPanicCount(tcpLogs, cb),
				)
			}); err != nil {
				return nil, err
			}
		}

		partial.devices = append(partial.devices, &appDevice{
			id:        id,
			client:    matrixClient,
			scheduler: scheduler,
			tcpLogs:   tcpLogs,
		})
		schedulers[id] = scheduler
	}

	httpAPI, err := httpapi.New(httpapi.Options{
		Logger:        logger,
		Bus:           bus,
		Schedulers:    schedulers,
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
	r.Get("/openapi.json", a.httpAPI.HandleOpenAPI)
	r.Mount("/api/v1", a.httpAPI.Router())

	return r
}

func (a *App) mapAndEnqueue(ctx context.Context, event events.Event) error {
	request, ok := a.rules.Map(event)
	if !ok {
		return errNoRuleMatch
	}
	applyEventOverrides(&request, event)

	target := event.Target
	device := a.deviceByID(target)
	if device == nil {
		if target == "" {
			return errNoDeviceTarget
		}
		return fmt.Errorf("%w: unknown device %q", errNoDeviceTarget, target)
	}
	return device.scheduler.EnqueueRequest(ctx, request)
}

func (a *App) deviceByID(id string) *appDevice {
	for _, d := range a.devices {
		if d.id == id {
			return d
		}
	}
	return nil
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

var (
	errNoRuleMatch    = errors.New("no matching rule")
	errNoDeviceTarget = errors.New("no device target")
)

func backgroundConfig(devCfg config.DeviceConfig) matrix.BackgroundConfig {
	if devCfg.Background.Animation == "" || !devCfg.Background.RestoreOnIdle {
		return matrix.BackgroundConfig{}
	}
	return matrix.BackgroundConfig{AnimationID: devCfg.Background.Animation}
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
	Status         string                       `json:"status"`
	WorkersRunning bool                         `json:"workers_running"`
	Draining       bool                         `json:"draining"`
	EventWorker    eventWorkerReady             `json:"event_worker"`
	Devices        map[string]deviceReadyEntry  `json:"devices"`
	// Aggregate fields retained for observability convenience.
	OutcomesDropped             uint64            `json:"outcome_reports_dropped"`
	OutcomeRecordingPanics      uint64            `json:"outcome_recording_panics"`
	TCPReconnectLogEventsDropped uint64           `json:"tcp_reconnect_log_events_dropped"`
	ObservabilityCallbackPanics  uint64           `json:"observability_callback_panics"`
	ObservabilityCallbackCounts  map[string]uint64 `json:"observability_callback_panic_counts,omitempty"`
}

type deviceReadyEntry struct {
	SchedulerState  matrix.State    `json:"scheduler_state"`
	MatrixConnected bool            `json:"matrix_connected"`
	Background      backgroundReady `json:"background"`
	LastSuccess     *time.Time      `json:"last_success,omitempty"`
	LastFailure     *time.Time      `json:"last_failure,omitempty"`
}

type backgroundReady struct {
	ConfiguredID   string                            `json:"configured_id,omitempty"`
	Kind           animations.PublicKind             `json:"kind,omitempty"`
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

	now := time.Now()
	deviceEntries := make(map[string]deviceReadyEntry, len(a.devices))
	allConnected := true
	var totalOutcomesDropped uint64
	var totalOutcomeRecordingPanics uint64
	var totalTCPLogEventsDropped uint64
	var totalObsPanics uint64
	var allObsCounts map[string]uint64

	for _, d := range a.devices {
		health := d.scheduler.Health()
		background := backgroundConvergenceProjectionForApp(health, now)
		recordBackgroundHealthMetrics(a.metrics, d.id, health, now)
		backgroundKind, _ := publicBackgroundKind(health.BackgroundKind)

		if !health.MatrixConnected || health.State == matrix.StateDisconnected || health.State == matrix.StateDraining {
			allConnected = false
		}

		entry := deviceReadyEntry{
			SchedulerState:  health.State,
			MatrixConnected: health.MatrixConnected,
			Background: backgroundReady{
				ConfiguredID:   health.BackgroundID,
				Kind:           backgroundKind,
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
			LastSuccess: health.LastSuccess,
			LastFailure: health.LastFailure,
		}
		deviceEntries[d.id] = entry

		totalOutcomesDropped += health.OutcomeReportsDropped
		totalOutcomeRecordingPanics += health.OutcomeRecordingPanics
		totalTCPLogEventsDropped += tcpReconnectLogEventsDropped(d.tcpLogs)
		totalObsPanics += health.ObservabilityCallbackPanics +
			observabilityCallbackPanics(d.client) +
			observabilityCallbackPanics(d.tcpLogs)
		allObsCounts = mergeObservabilityCallbackPanicCounts(allObsCounts,
			applicationObservabilityCallbackPanicCounts(d.scheduler, d.client, d.tcpLogs))
	}

	ready := workersRunning && !draining && allConnected

	status := "not_ready"
	if ready {
		status = "ready"
	}
	return readyResponse{
		Status:         status,
		WorkersRunning: workersRunning,
		Draining:       draining,
		EventWorker:    a.eventWorker.snapshot(now),
		Devices:        deviceEntries,
		OutcomesDropped:              totalOutcomesDropped,
		OutcomeRecordingPanics:       totalOutcomeRecordingPanics,
		TCPReconnectLogEventsDropped: totalTCPLogEventsDropped,
		ObservabilityCallbackPanics:  totalObsPanics,
		ObservabilityCallbackCounts:  allObsCounts,
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

func setMatrixConnectedMetric(registry *metrics.Registry, deviceID string, connected bool) {
	if connected {
		registry.MatrixConnected.WithLabelValues(deviceID).Set(1)
		return
	}
	registry.MatrixConnected.WithLabelValues(deviceID).Set(0)
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
	if errors.Is(err, errNoDeviceTarget) {
		logger.Warn("event has no routable device target", "event_id", event.ID, "source", event.Source, "type", event.Type, "target", event.Target, "error", err)
		return
	}
	logger.Warn("enqueue event animation", "event_id", event.ID, "source", event.Source, "type", event.Type, "error", err)
}

func recordItemOutcomeMetric(registry *metrics.Registry, deviceID string, report matrix.OutcomeReport) {
	registry.PlayItemsTotal.WithLabelValues(
		deviceID,
		string(report.ItemKind),
		outcomeMetricItem(report),
		string(report.Outcome),
	).Inc()
}

func recordReconnectAttempt(registry *metrics.Registry, deviceID string, attempt matrix.ReconnectAttempt, observeDelay bool) {
	registry.MatrixReconnectsTotal.WithLabelValues(deviceID, string(attempt.Source), string(attempt.ErrorKind)).Inc()
	if observeDelay {
		registry.MatrixReconnectDelay.WithLabelValues(deviceID, string(attempt.Source)).Observe(attempt.Delay.Seconds())
	}
}

func recordReconnectRecovery(registry *metrics.Registry, deviceID string, recovery matrix.ReconnectRecovery) {
	registry.MatrixReconnectRecoveriesTotal.WithLabelValues(deviceID, string(recovery.Source), string(recovery.State)).Inc()
}

func recordReconnectFailure(registry *metrics.Registry, deviceID string, failure matrix.ReconnectFailure) {
	registry.MatrixReconnectFailuresTotal.WithLabelValues(
		deviceID,
		string(failure.Source),
		string(failure.ErrorKind),
		string(failure.Outcome),
	).Inc()
}

func recordProbeFailure(registry *metrics.Registry, deviceID string, failure matrix.ProbeFailure) {
	registry.MatrixProbeFailuresTotal.WithLabelValues(deviceID, string(failure.ErrorKind), string(failure.Reason)).Inc()
}

func recordBackgroundRestoreMetric(registry *metrics.Registry, deviceID string, event matrix.BackgroundRestoreEvent) {
	kind, ok := publicBackgroundKindLabel(event.Kind)
	if !ok {
		return
	}
	switch event.State {
	case matrix.BackgroundConvergenceAttempting:
		registry.BackgroundRestoreAttemptsTotal.WithLabelValues(deviceID, kind).Inc()
	case matrix.BackgroundConvergenceFailed, matrix.BackgroundConvergenceRetrying:
		registry.BackgroundRestoreFailuresTotal.WithLabelValues(deviceID, kind, string(event.ErrorKind)).Inc()
	}
}

func (a *App) refreshBackgroundStateMetrics() {
	if a == nil {
		return
	}
	now := time.Now()
	for _, d := range a.devices {
		recordBackgroundHealthMetrics(a.metrics, d.id, d.scheduler.Health(), now)
	}
}

func recordBackgroundHealthMetrics(registry *metrics.Registry, deviceID string, health matrix.Health, now time.Time) {
	background := backgroundConvergenceProjectionForApp(health, now)
	setBackgroundStateMetrics(
		registry,
		deviceID,
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

func setBackgroundStateMetrics(
	registry *metrics.Registry,
	deviceID string,
	kind matrix.BackgroundKind,
	state matrix.BackgroundConvergenceState,
	dirty bool,
	converged bool,
	nextRetry *time.Time,
	now time.Time,
) {
	setBackgroundGaugeMetrics(registry, deviceID, kind, state, dirty, converged)
	setBackgroundNextRetryMetric(registry, deviceID, kind, nextRetry, now)
}

func setBackgroundGaugeMetrics(
	registry *metrics.Registry,
	deviceID string,
	kind matrix.BackgroundKind,
	state matrix.BackgroundConvergenceState,
	dirty bool,
	converged bool,
) {
	if registry == nil {
		return
	}
	labelKind, ok := publicBackgroundKindLabel(kind)
	if !ok {
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
	registry.BackgroundDirty.WithLabelValues(deviceID, labelKind).Set(dirtyValue)
	registry.BackgroundConverged.WithLabelValues(deviceID, labelKind).Set(convergedValue)
	for _, metricState := range matrix.BackgroundConvergenceV1States() {
		value := 0.0
		if state == metricState {
			value = 1
		}
		registry.BackgroundState.WithLabelValues(deviceID, labelKind, string(metricState)).Set(value)
	}
}

func setBackgroundNextRetryMetric(registry *metrics.Registry, deviceID string, kind matrix.BackgroundKind, nextRetry *time.Time, now time.Time) {
	if registry == nil {
		return
	}
	labelKind, ok := publicBackgroundKindLabel(kind)
	if !ok {
		return
	}
	nextRetrySeconds := 0.0
	if nextRetry != nil {
		nextRetrySeconds = nextRetry.Sub(now).Seconds()
		if nextRetrySeconds < 0 {
			nextRetrySeconds = 0
		}
	}
	registry.BackgroundNextRetrySeconds.WithLabelValues(deviceID, labelKind).Set(nextRetrySeconds)
}

func publicBackgroundKind(kind matrix.BackgroundKind) (animations.PublicKind, bool) {
	if kind == "" {
		return "", false
	}
	return animations.ProjectPublicKind(string(kind))
}

func publicBackgroundKindLabel(kind matrix.BackgroundKind) (string, bool) {
	publicKind, ok := publicBackgroundKind(kind)
	if !ok {
		return "", false
	}
	return string(publicKind), true
}

func logItemOutcome(logger *slog.Logger, deviceID string, report matrix.OutcomeReport) {
	logger.Info("matrix item outcome",
		"device", deviceID,
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

func logReconnectAttempt(logger *slog.Logger, deviceID string, attempt matrix.ReconnectAttempt) {
	attrs := []any{
		"device", deviceID,
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

func logReconnectRecovered(logger *slog.Logger, deviceID string, recovery matrix.ReconnectRecovery) {
	logger.Info("matrix reconnect recovered",
		"device", deviceID,
		"source", recovery.Source,
		"attempt", recovery.Attempt,
		"state", recovery.State,
		"outcome", "connected",
	)
}

func logReconnectFailure(logger *slog.Logger, deviceID string, failure matrix.ReconnectFailure) {
	attrs := []any{
		"device", deviceID,
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

func logProbeFailure(logger *slog.Logger, deviceID string, failure matrix.ProbeFailure) {
	attrs := []any{
		"device", deviceID,
		"error_kind", failure.ErrorKind,
		"reason", failure.Reason,
	}
	if failure.Error != "" {
		attrs = append(attrs, "error", failure.Error)
	}
	logger.Warn("matrix probe failure", attrs...)
}

func logBackgroundRestore(logger *slog.Logger, deviceID string, event matrix.BackgroundRestoreEvent) {
	attrs := []any{
		"device", deviceID,
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
			logReconnectAttempt(logger, "", attempt)
		},
	})
}

func (d *tcpReconnectLogDispatcher) LogReconnectRecovered(recovery matrix.ReconnectRecovery) {
	d.enqueue(tcpReconnectLogEvent{
		callback: matrix.ObservabilityCallbackReconnectRecovered,
		log: func(logger *slog.Logger) {
			logReconnectRecovered(logger, "", recovery)
		},
	})
}

func (d *tcpReconnectLogDispatcher) LogReconnectFailure(failure matrix.ReconnectFailure) {
	d.enqueue(tcpReconnectLogEvent{
		callback: matrix.ObservabilityCallbackReconnectFailure,
		log: func(logger *slog.Logger) {
			logReconnectFailure(logger, "", failure)
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
