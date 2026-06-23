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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/matrix"
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

	postInternalJSON(t, httpServer.URL+"/api/v1/matrix/fill", `{"r":9,"g":10,"b":11}`, http.StatusOK)

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

	postInternalJSON(t, httpServer.URL+"/api/v1/play", `{"animation":"notification","duration":"2s","restore":"leave"}`, http.StatusAccepted)

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
	postInternalJSON(t, httpServer.URL+"/api/v1/events", `{"type":"shutdown-timeout-before-cleanup-test"}`, http.StatusAccepted)

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
	postInternalJSON(t, httpServer.URL+"/api/v1/events", `{"type":"shutdown-timeout-test"}`, http.StatusServiceUnavailable)
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
