package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/worxbend/echo/internal/events"
)

var (
	// ErrAppRunning is returned by RunWorkers when workers are already active
	// and by Close when RunWorkers is still active.
	ErrAppRunning = errors.New("app workers are running")
	// ErrAppClosed is returned by RunWorkers when the app has been closed or
	// when its one-shot worker lifecycle has already stopped.
	ErrAppClosed = errors.New("app is closed")
)

type appLifecycleState uint8

const (
	appLifecycleNeverRun appLifecycleState = iota
	appLifecycleRunning
	appLifecycleStopped
	appLifecycleClosed
)

type appLifecycle struct {
	mu                sync.Mutex
	state             appLifecycleState
	draining          bool
	shutdownRequested bool
	workerCancel      context.CancelFunc
	workerDone        chan struct{}
	workerErr         error
}

type appLifecycleSnapshot struct {
	workersRunning bool
	draining       bool
}

func (l *appLifecycle) startWorkers(cancel context.CancelFunc) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case appLifecycleClosed, appLifecycleStopped:
		return ErrAppClosed
	case appLifecycleRunning:
		return ErrAppRunning
	case appLifecycleNeverRun:
		if l.shutdownRequested {
			return ErrAppClosed
		}
		l.state = appLifecycleRunning
		l.draining = false
		l.workerCancel = cancel
		l.workerDone = make(chan struct{})
		l.workerErr = nil
		return nil
	}
	return ErrAppClosed
}

func (l *appLifecycle) markDraining() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state != appLifecycleNeverRun {
		l.draining = true
	}
}

func (l *appLifecycle) finishWorkers(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state == appLifecycleRunning {
		l.state = appLifecycleStopped
		l.draining = true
	}
	l.workerCancel = nil
	l.workerErr = err
	if l.workerDone != nil {
		close(l.workerDone)
		l.workerDone = nil
	}
}

func (l *appLifecycle) close() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case appLifecycleClosed:
		return false, nil
	case appLifecycleRunning:
		return false, ErrAppRunning
	case appLifecycleNeverRun, appLifecycleStopped:
		l.state = appLifecycleClosed
		l.draining = true
		return true, nil
	default:
		l.state = appLifecycleClosed
		l.draining = true
		return true, nil
	}
}

func (l *appLifecycle) beginShutdown() (chan struct{}, context.CancelFunc, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state != appLifecycleRunning {
		return nil, nil, false
	}
	l.draining = true
	l.shutdownRequested = true
	return l.workerDone, l.workerCancel, true
}

func (l *appLifecycle) workerError() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.workerErr
}

func (l *appLifecycle) snapshot() appLifecycleSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	return appLifecycleSnapshot{
		workersRunning: l.state == appLifecycleRunning,
		draining:       l.draining,
	}
}

func (a *App) admitRun(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	workerCtx, cancel := context.WithCancel(ctx)
	if err := a.lifecycle.startWorkers(cancel); err != nil {
		cancel()
		return nil, nil, err
	}
	return workerCtx, cancel, nil
}

// Run starts the app workers and HTTP server, then blocks until ctx is canceled
// or one of those components fails.
func (a *App) Run(ctx context.Context) error {
	runCtx, cancelRun, err := a.admitRun(ctx)
	if err != nil {
		return err
	}
	defer cancelRun()

	g, ctx := errgroup.WithContext(runCtx)

	g.Go(func() error {
		defer cancelRun()
		return a.runWorkersAdmitted(ctx)
	})

	server := &http.Server{
		Addr:              a.cfg.Server.Addr,
		Handler:           a.router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g.Go(func() error {
		defer cancelRun()
		a.logger.Info("http server starting", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	g.Go(func() error {
		<-ctx.Done()
		a.lifecycle.markDraining()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		a.logger.Info("http server shutting down")
		return server.Shutdown(shutdownCtx)
	})

	runErr := g.Wait()
	shutdownErr := a.Shutdown(context.Background())
	return errors.Join(runErr, shutdownErr)
}

func (a *App) RunWorkers(ctx context.Context) error {
	workerCtx, cancel, err := a.admitRun(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	return a.runWorkersAdmitted(workerCtx)
}

func (a *App) runWorkersAdmitted(workerCtx context.Context) error {
	g, ctx := errgroup.WithContext(workerCtx)

	// One scheduler goroutine per device.
	for _, d := range a.devices {
		d := d
		g.Go(func() error {
			return d.scheduler.Run(ctx)
		})
	}

	g.Go(func() error {
		return a.runEventWorker(ctx)
	})

	g.Go(func() error {
		return a.runHealthMetricsWorker(ctx)
	})

	g.Go(func() error {
		<-ctx.Done()
		a.lifecycle.markDraining()
		return nil
	})

	err := g.Wait()
	a.lifecycle.finishWorkers(err)
	return err
}

// Close releases app-owned resources. It is safe to call multiple times.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	shouldClose, err := a.lifecycle.close()
	if err != nil {
		return err
	}
	if !shouldClose {
		return nil
	}

	return errors.Join(err, a.closeResources())
}

// Shutdown coordinates worker cancellation and releases app-owned resources.
func (a *App) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	done, cancel, running := a.lifecycle.beginShutdown()
	if !running {
		return a.Close()
	}
	if cancel != nil {
		cancel()
	}

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	workerErr := a.lifecycle.workerError()
	shouldClose, closeErr := a.lifecycle.close()
	if closeErr != nil {
		return closeErr
	}
	if shouldClose {
		closeErr = a.closeResources()
	}
	return errors.Join(closeErr, workerErr)
}

func (a *App) closeResources() error {
	var err error
	a.clearEventWorkerEvent()
	for _, d := range a.devices {
		d.scheduler.Close()
		d.tcpLogs.Close()
		err = errors.Join(err, d.client.Close())
	}
	if a.bus != nil {
		if closeErr := a.bus.Close(); closeErr != nil && !errors.Is(closeErr, events.ErrBusClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}

func (a *App) runHealthMetricsWorker(ctx context.Context) error {
	a.syncHealthMetrics()

	// Use the smallest heartbeat interval across all devices (or 1s default).
	interval := time.Second
	for _, d := range a.devices {
		devCfg := a.cfg.Devices[d.id]
		if devCfg != nil && devCfg.HeartbeatInterval > 0 && (interval == time.Second || devCfg.HeartbeatInterval < interval) {
			interval = devCfg.HeartbeatInterval
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.syncHealthMetrics()
			return nil
		case <-ticker.C:
			a.syncHealthMetrics()
		}
	}
}

func (a *App) syncHealthMetrics() {
	for _, d := range a.devices {
		setMatrixConnectedMetric(a.metrics, d.id, d.scheduler.Health().MatrixConnected)
	}
}

func (a *App) runEventWorker(ctx context.Context) error {
	ch, unsubscribe := a.bus.SubscribeWithOptions(ctx, events.SubscriptionOptions{
		OnDepthChange: a.recordEventQueueDepth,
	})
	defer unsubscribe()
	defer a.clearEventWorkerEvent()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				a.recordEventQueueDepth(0)
				return nil
			}
			a.beginEventWorkerEvent(time.Now())
			a.recordEventQueueDepth(len(ch))
			a.metrics.EventsTotal.WithLabelValues(string(event.Source), event.Type).Inc()

			a.setEventWorkerStage(eventWorkerStageMap)
			if err := a.mapAndEnqueue(ctx, event); err != nil {
				a.setEventWorkerStage(eventWorkerStageLogDrop)
				logEnqueueError(a.logger, event, err)
				a.metrics.EventsDroppedTotal.WithLabelValues("map_or_enqueue").Inc()
				a.clearEventWorkerEvent()
				continue
			}
			a.clearEventWorkerEvent()
		}
	}
}

func (a *App) recordEventQueueDepth(depth int) {
	a.metrics.EventQueueDepth.Set(float64(depth))
}

func (a *App) recordEventWorkerInflight(inflight bool) {
	if inflight {
		a.metrics.EventWorkerInflight.Set(1)
		return
	}
	a.metrics.EventWorkerInflight.Set(0)
}

type eventWorkerStage string

const (
	eventWorkerStateIdle       = "idle"
	eventWorkerStateProcessing = "processing"

	eventWorkerStageReceive eventWorkerStage = "receive"
	eventWorkerStageMap     eventWorkerStage = "map"
	eventWorkerStageEnqueue eventWorkerStage = "enqueue"
	eventWorkerStageLogDrop eventWorkerStage = "log_drop"
)

type eventWorkerDiagnostics struct {
	mu        sync.Mutex
	active    bool
	stage     eventWorkerStage
	startedAt time.Time
}

type eventWorkerReady struct {
	State                 string   `json:"state"`
	Stage                 string   `json:"stage,omitempty"`
	ActiveDurationSeconds *float64 `json:"active_duration_seconds,omitempty"`
}

func (a *App) beginEventWorkerEvent(now time.Time) {
	a.recordEventWorkerInflight(true)
	a.eventWorker.mu.Lock()
	a.eventWorker.active = true
	a.eventWorker.stage = eventWorkerStageReceive
	a.eventWorker.startedAt = now
	a.eventWorker.mu.Unlock()
}

func (a *App) setEventWorkerStage(stage eventWorkerStage) {
	a.eventWorker.mu.Lock()
	if a.eventWorker.active {
		a.eventWorker.stage = stage
	}
	a.eventWorker.mu.Unlock()
}

func (a *App) clearEventWorkerEvent() {
	a.recordEventWorkerInflight(false)
	a.eventWorker.mu.Lock()
	a.eventWorker.active = false
	a.eventWorker.stage = ""
	a.eventWorker.startedAt = time.Time{}
	a.eventWorker.mu.Unlock()
}

func (d *eventWorkerDiagnostics) snapshot(now time.Time) eventWorkerReady {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.active {
		return eventWorkerReady{State: eventWorkerStateIdle}
	}
	duration := now.Sub(d.startedAt)
	if duration < 0 {
		duration = 0
	}
	seconds := duration.Seconds()
	return eventWorkerReady{
		State:                 eventWorkerStateProcessing,
		Stage:                 string(d.stage),
		ActiveDurationSeconds: &seconds,
	}
}
