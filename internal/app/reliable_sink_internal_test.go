package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/config"
	"github.com/worxbend/echo/internal/matrix"
	"github.com/worxbend/echo/internal/metrics"
)

func TestReadyAndMetricsExposeNonzeroOutcomeRecordingPanics(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := newWithOptions(
		newHTTPMatrixTestConfig(t, matrixServer.Addr()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		withReliableOutcomeSinkWrapperForTest(func(next func(matrix.OutcomeReport)) func(matrix.OutcomeReport) {
			return func(report matrix.OutcomeReport) {
				next(report)
				panic("test reliable outcome sink panic")
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForInternalStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postInternalJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":9,"g":10,"b":11}`, http.StatusOK)

	waitForInternalReadyOutcomeRecordingPanics(t, httpServer.URL, 1)
	waitForInternalMetricLine(t, httpServer.URL, "matrix_proxy_play_item_outcome_recording_panics_total", " 1")
}

func TestAppShutdownTimeoutDefersResourceCloseUntilWorkersStopWithBlockedReliableSink(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	reliableSink := newBlockingReliableOutcomeSink()
	application, err := newWithOptions(
		newHTTPMatrixTestConfig(t, matrixServer.Addr()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		withReliableOutcomeSinkWrapperForTest(reliableSink.Wrap),
	)
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application, context.Background())
	workersStopped := false
	defer func() {
		reliableSink.release()
		if !workersStopped {
			waitAppWorkers(t, done)
		}
		_ = application.Shutdown(context.Background())
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForInternalStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	waitForActiveMatrixConnections(t, matrixServer, 1)

	postInternalJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"2s","restore":"leave"}`, http.StatusAccepted)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- application.Shutdown(shutdownCtx)
	}()

	select {
	case <-reliableSink.entered:
	case <-time.After(time.Second):
		t.Fatal("reliable outcome sink was not invoked during shutdown")
	}
	if err := waitShutdownResult(t, shutdownDone); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want context deadline exceeded", err)
	}
	select {
	case err := <-done:
		t.Fatalf("RunWorkers() returned before reliable sink was released: %v", err)
	default:
	}

	ready, status := getInternalReadyDetails(t, httpServer.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusServiceUnavailable, ready)
	}
	if ready.Status != "not_ready" {
		t.Fatalf("/readyz status = %q, want not_ready", ready.Status)
	}
	if !ready.WorkersRunning {
		t.Fatal("/readyz workers_running = false, want true while shutdown is still unwinding")
	}
	if !ready.Draining {
		t.Fatal("/readyz draining = false, want true")
	}
	if got := matrixServer.ClosedConnections(); got != 0 {
		t.Fatalf("matrix connections closed after timed-out Shutdown = %d, want 0", got)
	}

	reliableSink.release()
	waitAppWorkers(t, done)
	workersStopped = true
	if got := matrixServer.ClosedConnections(); got != 0 {
		t.Fatalf("matrix connections closed after workers stopped before cleanup = %d, want 0", got)
	}
	postInternalJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"shutdown-timeout-before-cleanup-test"}`, http.StatusAccepted)

	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("follow-up Shutdown() error = %v, want nil", err)
	}
	waitForClosedMatrixConnections(t, matrixServer, 1)
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after follow-up Shutdown() error = %v, want nil", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("second follow-up Shutdown() error = %v, want nil", err)
	}
	if got := matrixServer.ClosedConnections(); got != 1 {
		t.Fatalf("matrix connections closed after idempotent cleanup = %d, want 1", got)
	}
	postInternalJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"shutdown-timeout-test"}`, http.StatusServiceUnavailable)
	if err := application.RunWorkers(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("RunWorkers() after shutdown timeout cleanup error = %v, want ErrAppClosed", err)
	}
}

func withReliableOutcomeSinkWrapperForTest(wrapper func(func(matrix.OutcomeReport)) func(matrix.OutcomeReport)) appNewOption {
	return appNewOptionFunc(func(options *appNewOptions) {
		options.wrapReliableOutcomeSink = wrapper
	})
}

type blockingReliableOutcomeSink struct {
	entered     chan struct{}
	released    chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingReliableOutcomeSink() *blockingReliableOutcomeSink {
	return &blockingReliableOutcomeSink{
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (s *blockingReliableOutcomeSink) Wrap(next func(matrix.OutcomeReport)) func(matrix.OutcomeReport) {
	return func(report matrix.OutcomeReport) {
		next(report)
		s.enterOnce.Do(func() {
			close(s.entered)
		})
		<-s.released
	}
}

func (s *blockingReliableOutcomeSink) release() {
	s.releaseOnce.Do(func() {
		close(s.released)
	})
}

type internalReadyDetails struct {
	Status         string `json:"status"`
	WorkersRunning bool   `json:"workers_running"`
	Draining       bool   `json:"draining"`
}

func postInternalJSON(t *testing.T, url, body string, want int) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s status = %d, want %d", url, resp.StatusCode, want)
	}
}

func waitForInternalStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastStatus int
	var lastBody []byte
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		lastErr = nil
		lastStatus = resp.StatusCode
		lastBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if lastStatus == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET %s error = %v, want status %d", url, lastErr, want)
	}
	t.Fatalf("GET %s status = %d, body = %s, want %d", url, lastStatus, lastBody, want)
}

func getInternalReadyDetails(t *testing.T, baseURL string) (internalReadyDetails, int) {
	t.Helper()
	resp, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body internalReadyDetails
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode /readyz response %q: %v", data, err)
	}
	return body, resp.StatusCode
}

func waitForInternalReadyOutcomeRecordingPanics(t *testing.T, baseURL string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last any
	var lastBody []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		lastBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var body map[string]any
		if err := json.Unmarshal(lastBody, &body); err != nil {
			t.Fatalf("decode /readyz response %q: %v", lastBody, err)
		}
		last = body["outcome_recording_panics"]
		if last == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz outcome_recording_panics = %v, want %g; body = %s", last, want, lastBody)
}

func waitForInternalMetricLine(t *testing.T, baseURL, metric string, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = getInternalMetrics(t, baseURL)
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, metric) {
				continue
			}
			matched := true
			for _, part := range parts {
				if !strings.Contains(line, part) {
					matched = false
					break
				}
			}
			if matched {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("metrics missing %s with %v in:\n%s", metric, parts, body)
}

func getInternalMetrics(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, body = %s", resp.StatusCode, data)
	}
	return string(data)
}

// ── tcpReconnectLogDispatcher regressions ────────────────────────────────────
//
// Both bugs below were silent: the goroutine leak only showed up as a slow drift
// in goroutine count across failed constructions, and the lost shutdown logs were
// not even counted as drops, because enqueue had already accepted them.

// Close used to abandon every event still sitting in the buffer: run() selected on
// a closed stop channel and returned, so up to 64 accepted reconnect log lines were
// never written and never counted. A reconnect storm right before shutdown is
// exactly when those lines matter.
func TestTCPReconnectLogDispatcherFlushesAcceptedEventsOnClose(t *testing.T) {
	var mu sync.Mutex
	var logged int

	// A handler that blocks until released, so events pile up in the buffer while
	// the run goroutine is stuck on the first one.
	release := make(chan struct{})
	handler := &countingBlockingHandler{release: release, onHandle: func() {
		mu.Lock()
		logged++
		mu.Unlock()
	}}

	dispatcher := newTCPReconnectLogDispatcher(slog.New(handler), 8)

	const enqueued = 5
	for i := 0; i < enqueued; i++ {
		dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{Attempt: i + 1})
	}
	if got := dispatcher.EventsDropped(); got != 0 {
		t.Fatalf("EventsDropped = %d, want 0; the buffer has room for all %d events", got, enqueued)
	}

	dispatcher.Close()
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		seen := logged
		mu.Unlock()
		if seen == enqueued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logged %d of %d accepted events after Close; accepted events must be flushed, not discarded", seen, enqueued)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := dispatcher.EventsDropped(); got != 0 {
		t.Fatalf("EventsDropped = %d, want 0; flushed events are not drops", got)
	}
}

// The dispatcher owns a goroutine from construction, but only a fully built device
// reaches App.devices and therefore closeResources. Every error return in
// newAppDevice after the dispatcher is created used to leak that goroutine.
func TestNewAppDeviceDoesNotLeakReconnectLogGoroutineWhenConstructionFails(t *testing.T) {
	registry, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	animationRegistry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}

	// An unsupported wiring fails animations.NewLayout... so instead fail later:
	// a background animation that is not in the registry fails the scheduler
	// constructor, which sits after the dispatcher is created.
	devCfg := &config.DeviceConfig{
		Host:            "127.0.0.1",
		Port:            7777,
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		Layout: config.LayoutConfig{
			Width:             8,
			Height:            8,
			Wiring:            "h-tl",
			OddRowDisplayFlip: true,
		},
		Background: config.BackgroundConfig{
			Animation:     "no-such-animation",
			RestoreOnIdle: true,
		},
	}

	before := runtime.NumGoroutine()
	const attempts = 25
	for i := 0; i < attempts; i++ {
		device, err := newAppDevice(
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			registry,
			animationRegistry,
			"leak-probe",
			devCfg,
			16,
			nil,
		)
		if err == nil {
			if device != nil && device.tcpLogs != nil {
				device.tcpLogs.Close()
			}
			t.Fatalf("newAppDevice succeeded on attempt %d; this test needs a construction failure after the dispatcher is created", i)
		}
	}

	// Closed dispatchers' goroutines exit asynchronously; allow them to drain.
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d across %d failed constructions; the reconnect log dispatcher leaked", before, after, attempts)
	}
}

// countingBlockingHandler blocks every Handle call until release is closed.
type countingBlockingHandler struct {
	release  chan struct{}
	onHandle func()
}

func (h *countingBlockingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingBlockingHandler) Handle(_ context.Context, _ slog.Record) error {
	<-h.release
	if h.onHandle != nil {
		h.onHandle()
	}
	return nil
}

func (h *countingBlockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *countingBlockingHandler) WithGroup(string) slog.Handler { return h }
