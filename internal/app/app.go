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
	"github.com/worxbend/echo/internal/observability"
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
	cfg         config.Config
	logger      *slog.Logger
	metrics     *metrics.Registry
	bus         *events.Bus
	rules       eventMapper
	registry    *animations.Registry
	devices     []*appDevice // ordered by device ID for deterministic iteration
	devicesByID map[string]*appDevice
	httpAPI     *httpapi.Server
	lifecycle   appLifecycle

	eventWorker eventWorkerDiagnostics
}

type eventMapper interface {
	Map(events.Event) (animations.AnimationRequest, bool)
}

type matrixClientCloser interface {
	matrix.Client
	matrixObservabilityPanicCounter
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
		cfg:         cfg,
		logger:      logger,
		metrics:     registry,
		devicesByID: make(map[string]*appDevice, len(cfg.Devices)),
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

	// The bus is a singleton, so its panic counters are registered once here with
	// no device label -- a per-device registration would collide on the second
	// device, and the counts are not attributable to one device anyway.
	for _, cb := range busObservabilityCallbackNames() {
		cb := cb
		if err := registry.RegisterEventObservabilityCallbackPanics(cb, func() float64 {
			return float64(bus.ObservabilityCallbackPanicCounts()[cb])
		}); err != nil {
			return nil, err
		}
	}

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
		if devCfg == nil {
			return nil, fmt.Errorf("invalid device %q configuration", id)
		}
		device, err := newAppDevice(
			logger,
			registry,
			animationRegistry,
			id,
			devCfg,
			cfg.Queue.PlayBuffer,
			newOptions.wrapReliableOutcomeSink,
		)
		if err != nil {
			return nil, err
		}
		partial.devices = append(partial.devices, device)
		partial.devicesByID[id] = device
		schedulers[id] = device.scheduler
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

// securityHeaders sets the response headers that apply to everything this
// server serves. Until now only /docs set them, because only /docs is HTML --
// but what they defend against is not limited to HTML. The JSON endpoints and
// /metrics get opened in browsers too, by hand: a readiness check pasted into
// a tab, a scrape someone wants to eyeball. Without nosniff the browser is
// free to disregard Content-Type and guess a type from the bytes, and a guess
// of "HTML" on a body whose leading field an attacker influences is the usual
// way a JSON endpoint ends up executing script on this origin. DENY and
// no-referrer cost nothing on an API that is never meant to be framed and
// whose paths carry device identifiers that have no business travelling in a
// Referer to wherever a page links next.
//
// These are set on the way in, before any handler runs, so they also survive
// the responses this server writes without reaching a handler at all -- the
// 500 from middleware.Recoverer, the 504 from middleware.Timeout -- and a
// handler that needs something stricter still wins, because setting the same
// header in the handler replaces the value rather than appending to it. That
// is what lets HandleDocs keep its own content-security-policy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *App) router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// No middleware.RealIP: chi deprecated it as unfixable rather than patching
	// it (GHSA-3fxj-6jh8-hvhx and friends). It rewrites RemoteAddr from
	// X-Forwarded-For / True-Client-IP / X-Real-IP whether or not anything
	// upstream actually sets them, so any client can choose its own apparent
	// address. Nothing here reads RemoteAddr, so it was pure attack surface.
	r.Use(securityHeaders)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", a.handleHealth)
	r.Get("/readyz", a.handleReady)
	r.Handle("/metrics", http.HandlerFunc(a.handleMetrics))
	r.Get("/openapi.json", a.httpAPI.HandleOpenAPI)
	r.Get("/swagger.json", a.httpAPI.HandleSwagger)
	r.Get("/docs", a.httpAPI.HandleDocs)
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
	if a == nil {
		return nil
	}
	if d := a.devicesByID[id]; d != nil {
		return d
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
	// Validate rather than trust. The HTTP boundary already checks these, but the
	// bus is the extension point for other producers, and an unrecognised restore
	// policy reaching Scheduler.restore returns an error that exits Run — so an
	// unchecked attribute here would stop the device's scheduler outright.
	if restore := event.Attributes["restore"]; restore != "" {
		if policy := animations.RestorePolicy(restore); animations.IsValidRestorePolicy(policy) {
			request.RestorePolicy = policy
		}
	}
	if loop := event.Attributes["loop"]; loop != "" {
		if policy := animations.LoopPolicy(loop); animations.IsValidLoopPolicy(policy) {
			request.Loop = policy
		}
	}
	if duration := event.Attributes["duration"]; duration != "" {
		if parsed, err := time.ParseDuration(duration); err == nil {
			request.MaxDuration = parsed
		}
	}
	// Priority is not a refinement like the overrides above: it orders the queue,
	// and for rules that set an interrupt mode it decides which queued items are
	// evicted and whether the in-flight animation is cancelled. /events and
	// /notify are the only device routes without adminOnly, so this integer
	// arrives unauthenticated. Let an event lower its own priority, never raise it
	// above the ceiling the matched rule set -- otherwise an anonymous
	// notification outranks, evicts, or cancels work queued through the
	// admin-only /play route. That keeps the rules file the whole policy: a caller
	// steering which rule matches can still only reach a priority an operator
	// wrote down.
	if event.Priority != 0 && event.Priority < request.Priority {
		request.Priority = event.Priority
	}
	// No interrupt_mode case, deliberately. The HTTP boundary validates that
	// attribute so a typo fails loudly, but preemption stays the operator's
	// decision: honouring it here would let an unauthenticated event overrule a
	// rule that says interrupt: none.
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

func newAppDevice(
	logger *slog.Logger,
	registry *metrics.Registry,
	animationRegistry *animations.Registry,
	deviceID string,
	devCfg *config.DeviceConfig,
	playQueueCapacity int,
	wrapReliableOutcomeSink func(func(matrix.OutcomeReport)) func(matrix.OutcomeReport),
) (*appDevice, error) {
	layout, err := animations.NewLayout(
		devCfg.Layout.Width,
		devCfg.Layout.Height,
		devCfg.Layout.Wiring,
		devCfg.Layout.OddRowDisplayFlip,
		devCfg.Layout.Rotation,
	)
	if err != nil {
		return nil, err
	}
	packer, err := animations.NewPacker(layout)
	if err != nil {
		return nil, err
	}

	tcpLogs := newTCPReconnectLogDispatcher(logger, 64)
	// The dispatcher owns a goroutine from construction, but only a fully built
	// device reaches App.devices and therefore closeResources. Every error return
	// below would otherwise leak that goroutine for the process lifetime.
	deviceBuilt := false
	defer func() {
		if !deviceBuilt {
			tcpLogs.Close()
		}
	}()

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
	if wrapReliableOutcomeSink != nil {
		if wrapped := wrapReliableOutcomeSink(recordReliableOutcome); wrapped != nil {
			recordReliableOutcome = wrapped
		}
	}

	// Copied rather than aliased: the scheduler outlives this call and must not
	// observe later edits to the device config.
	brightness := devCfg.Brightness
	scheduler, err := matrix.NewSchedulerWithReliableAppOutcomeRecorder(matrix.SchedulerOptions{
		Client:            matrixClient,
		Registry:          animationRegistry,
		Packer:            packer,
		QueueCapacity:     playQueueCapacity,
		Background:        backgroundConfig(*devCfg),
		InitialBrightness: &brightness,
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
	registry.PlayQueueDepth.WithLabelValues(deviceID).Set(0)
	setMatrixConnectedMetric(registry, deviceID, false)

	if err := registry.RegisterPlayItemOutcomesDropped(deviceID, func() float64 {
		return float64(scheduler.OutcomeReportsDropped())
	}); err != nil {
		return nil, err
	}
	if err := registry.RegisterPlayItemOutcomeRecordingPanics(deviceID, func() float64 {
		return float64(scheduler.OutcomeRecordingPanics())
	}); err != nil {
		return nil, err
	}
	if err := registry.RegisterTCPReconnectLogEventsDropped(deviceID, func() float64 {
		return float64(tcpLogs.EventsDropped())
	}); err != nil {
		return nil, err
	}
	for _, cb := range schedulerObservabilityCallbackNames() {
		cb := cb
		if err := registry.RegisterMatrixObservabilityCallbackPanics(deviceID, string(matrix.ReconnectSourceSchedulerBackoff), cb, func() float64 {
			return float64(scheduler.ObservabilityCallbackPanicCounts()[cb])
		}); err != nil {
			return nil, err
		}
	}
	for _, cb := range tcpObservabilityCallbackNames() {
		cb := cb
		if err := registry.RegisterMatrixObservabilityCallbackPanics(deviceID, string(matrix.ReconnectSourceTCPImmediate), cb, func() float64 {
			return float64(observabilityCallbackPanicCount(matrixClient, cb) + observabilityCallbackPanicCount(tcpLogs, cb))
		}); err != nil {
			return nil, err
		}
	}

	deviceBuilt = true
	return &appDevice{
		id:        deviceID,
		client:    matrixClient,
		scheduler: scheduler,
		tcpLogs:   tcpLogs,
	}, nil
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

type readyResponse struct {
	Status         string                      `json:"status"`
	WorkersRunning bool                        `json:"workers_running"`
	Draining       bool                        `json:"draining"`
	EventWorker    eventWorkerReady            `json:"event_worker"`
	Devices        map[string]deviceReadyEntry `json:"devices"`
	// Aggregate fields retained for observability convenience. The panic counters
	// span every component that recovers from a best-effort callback: each
	// device's scheduler, matrix client and TCP reconnect log dispatcher, plus the
	// process-wide event bus.
	OutcomesDropped              uint64            `json:"outcome_reports_dropped"`
	OutcomeRecordingPanics       uint64            `json:"outcome_recording_panics"`
	TCPReconnectLogEventsDropped uint64            `json:"tcp_reconnect_log_events_dropped"`
	ObservabilityCallbackPanics  uint64            `json:"observability_callback_panics"`
	ObservabilityCallbackCounts  map[string]uint64 `json:"observability_callback_panic_counts,omitempty"`
}

// deviceReadyEntry mirrors matrix.Health for the readiness payload. PanelEnabled
// and Brightness are omitted when the scheduler holds no instruction for them;
// present-and-false and present-and-zero are meaningful settings, not absences.
type deviceReadyEntry struct {
	SchedulerState  matrix.State    `json:"scheduler_state"`
	MatrixConnected bool            `json:"matrix_connected"`
	PanelEnabled    *bool           `json:"panel_enabled,omitempty"`
	Brightness      *byte           `json:"brightness,omitempty"`
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
			PanelEnabled:    health.PanelEnabled,
			Brightness:      health.Brightness,
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

	// The event bus is a singleton rather than a per-device component, so it sits
	// outside the loop above -- and was therefore counted by nothing at all. Its
	// depth and backpressure observers write to the metrics registry, so a panic
	// in one of them silently stops a gauge from moving; folding its counters in
	// here is what makes that visible.
	totalObsPanics += observabilityCallbackPanics(a.bus)
	allObsCounts = mergeObservabilityCallbackPanicCounts(allObsCounts, a.bus.ObservabilityCallbackPanicCounts())

	ready := workersRunning && !draining && allConnected

	status := "not_ready"
	if ready {
		status = "ready"
	}
	return readyResponse{
		Status:                       status,
		WorkersRunning:               workersRunning,
		Draining:                     draining,
		EventWorker:                  a.eventWorker.snapshot(now),
		Devices:                      deviceEntries,
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

func observabilityCallbackPanics(counter matrixObservabilityPanicCounter) uint64 {
	if counter == nil {
		return 0
	}
	return counter.ObservabilityCallbackPanics()
}

func observabilityCallbackPanicCount(counter matrixObservabilityPanicCounter, callback string) uint64 {
	if counter == nil {
		return 0
	}
	return counter.ObservabilityCallbackPanicCounts()[callback]
}

func applicationObservabilityCallbackPanicCounts(
	scheduler *matrix.Scheduler,
	matrixClient matrixObservabilityPanicCounter,
	tcpReconnectLogs matrixObservabilityPanicCounter,
) map[string]uint64 {
	var counts map[string]uint64
	if scheduler != nil {
		counts = mergeObservabilityCallbackPanicCounts(counts, scheduler.ObservabilityCallbackPanicCounts())
	}
	if matrixClient != nil {
		counts = mergeObservabilityCallbackPanicCounts(counts, matrixClient.ObservabilityCallbackPanicCounts())
	}
	if tcpReconnectLogs != nil {
		counts = mergeObservabilityCallbackPanicCounts(counts, tcpReconnectLogs.ObservabilityCallbackPanicCounts())
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
	if fn, ok := backgroundRestoreMetricHandlers[event.State]; ok {
		fn(registry, deviceID, kind, event)
	}
}

var backgroundRestoreMetricHandlers = map[matrix.BackgroundConvergenceState]func(*metrics.Registry, string, string, matrix.BackgroundRestoreEvent){
	matrix.BackgroundConvergenceAttempting: func(registry *metrics.Registry, deviceID string, kind string, _ matrix.BackgroundRestoreEvent) {
		registry.BackgroundRestoreAttemptsTotal.WithLabelValues(deviceID, kind).Inc()
	},
	matrix.BackgroundConvergenceFailed: func(registry *metrics.Registry, deviceID string, kind string, event matrix.BackgroundRestoreEvent) {
		registry.BackgroundRestoreFailuresTotal.WithLabelValues(deviceID, kind, string(event.ErrorKind)).Inc()
	},
	matrix.BackgroundConvergenceRetrying: func(registry *metrics.Registry, deviceID string, kind string, event matrix.BackgroundRestoreEvent) {
		registry.BackgroundRestoreFailuresTotal.WithLabelValues(deviceID, kind, string(event.ErrorKind)).Inc()
	},
	matrix.BackgroundConvergenceConverged: func(registry *metrics.Registry, deviceID string, kind string, event matrix.BackgroundRestoreEvent) {
		registry.BackgroundRestoreSuccessesTotal.WithLabelValues(deviceID, kind, backgroundRestoreOutcome(event)).Inc()
	},
}

// Restore outcomes are derived from the event's own failure count rather than
// projected from a matrix vocabulary, so the label stays bounded to these two
// values and cannot drift as the scheduler gains states.
const (
	backgroundRestoreOutcomeConverged = "converged"
	backgroundRestoreOutcomeRecovered = "recovered"
)

// backgroundRestoreOutcome separates a restore that succeeded first try from one
// that climbed out of a retry loop -- the transition worth alerting on.
func backgroundRestoreOutcome(event matrix.BackgroundRestoreEvent) string {
	if event.FailureCount > 0 {
		return backgroundRestoreOutcomeRecovered
	}
	return backgroundRestoreOutcomeConverged
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
	// Convergence is the only non-attempt state that is not a failure; without
	// this branch a successful restore would be logged as one. Failure count and
	// outcome ride along here alone so the attempt and failure log shapes are
	// left as they were.
	if event.State == matrix.BackgroundConvergenceConverged {
		logger.Info("matrix background restore converged",
			append(attrs,
				"failure_count", event.FailureCount,
				"outcome", backgroundRestoreOutcome(event),
			)...)
		return
	}
	logger.Warn("matrix background restore failure", attrs...)
}

func schedulerObservabilityCallbackNames() []string {
	return matrix.SchedulerObservabilityCallbackNames()
}

func busObservabilityCallbackNames() []string {
	return []string{
		events.ObservabilityCallbackDepthChange,
		events.ObservabilityCallbackPublishBackpressureWait,
		events.ObservabilityCallbackPublishBackpressureTimeout,
	}
}

func tcpObservabilityCallbackNames() []string {
	return matrix.TCPClientObservabilityCallbackNames()
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

	callbackPanics observability.CallbackPanics
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
			d.drainAccepted()
			return
		case event := <-d.events:
			d.runEvent(event)
		}
	}
}

// drainAccepted flushes events that enqueue already accepted before Close.
//
// Without this, shutdown silently discarded up to cap(d.events) reconnect log
// lines: they were never written, and they were not counted as drops either,
// because enqueue had already taken responsibility for them. A reconnect storm
// immediately before shutdown is exactly when those lines matter most.
//
// Close deliberately does NOT wait for this drain. Blocking shutdown on a slog
// handler would reintroduce the coupling this dispatcher exists to prevent —
// TCPClient invokes the reconnect callbacks while its command-serialization mutex
// is held, so log handling must never gate anything. Draining without joining
// trades a short post-Close tail of log writes for not losing them.
func (d *tcpReconnectLogDispatcher) drainAccepted() {
	for {
		select {
		case event := <-d.events:
			d.runEvent(event)
		default:
			return
		}
	}
}

func (d *tcpReconnectLogDispatcher) runEvent(event tcpReconnectLogEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			d.callbackPanics.Record(event.callback)
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
	return d.callbackPanics.Total()
}

func (d *tcpReconnectLogDispatcher) ObservabilityCallbackPanicCounts() map[string]uint64 {
	if d == nil {
		return nil
	}
	return d.callbackPanics.Counts()
}
