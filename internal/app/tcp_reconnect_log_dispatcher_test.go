package app

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/matrix"
	"github.com/worxbend/echo/internal/metrics"
)

func TestTCPReconnectLogDispatcherCountsQueueFullDrops(t *testing.T) {
	handler := newBlockingDispatcherLogHandler()
	dispatcher := newTCPReconnectLogDispatcher(slog.New(handler), 1)
	defer func() {
		handler.release()
		dispatcher.Close()
	}()

	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect log handler was not invoked")
	}

	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})

	if got := dispatcher.EventsDropped(); got != 1 {
		t.Fatalf("EventsDropped() = %d, want 1", got)
	}
}

func TestTCPReconnectLogDispatcherEnqueueDoesNotWaitForBlockedLogger(t *testing.T) {
	handler := newBlockingDispatcherLogHandler()
	dispatcher := newTCPReconnectLogDispatcher(slog.New(handler), 1)
	defer func() {
		handler.release()
		dispatcher.Close()
	}()

	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect log handler was not invoked")
	}

	dispatcher.LogReconnectRecovered(matrix.ReconnectRecovery{})
	assertReturnsQuickly(t, func() {
		dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	})
	assertReturnsQuickly(t, func() {
		dispatcher.LogReconnectRecovered(matrix.ReconnectRecovery{})
	})
	assertReturnsQuickly(t, func() {
		dispatcher.LogReconnectFailure(matrix.ReconnectFailure{})
	})

	if got := dispatcher.EventsDropped(); got != 3 {
		t.Fatalf("EventsDropped() = %d, want 3", got)
	}
	if got := handler.handles.Load(); got != 1 {
		t.Fatalf("log handler calls while first logger is blocked = %d, want 1", got)
	}
}

func TestTCPReconnectLogDispatcherCloseStopsAdmissionWithoutPreemptingRunningLogger(t *testing.T) {
	handler := newBlockingDispatcherLogHandler()
	dispatcher := newTCPReconnectLogDispatcher(slog.New(handler), 1)
	defer handler.release()

	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect log handler was not invoked")
	}

	assertReturnsQuickly(t, dispatcher.Close)

	select {
	case <-handler.exited:
		t.Fatal("dispatcher close preempted logger already running")
	default:
	}

	assertReturnsQuickly(t, func() {
		dispatcher.LogReconnectRecovered(matrix.ReconnectRecovery{})
	})
	if got := dispatcher.EventsDropped(); got != 1 {
		t.Fatalf("EventsDropped() after enqueue on closed dispatcher = %d, want 1", got)
	}
	if got := handler.handles.Load(); got != 1 {
		t.Fatalf("log handler calls after dispatcher close = %d, want 1", got)
	}
}

func TestTCPReconnectLogDispatcherCountsEnqueueAfterCloseDrops(t *testing.T) {
	dispatcher := newTCPReconnectLogDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	dispatcher.Close()

	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})

	if got := dispatcher.EventsDropped(); got != 1 {
		t.Fatalf("EventsDropped() = %d, want 1", got)
	}
}

func TestTCPReconnectLogDispatcherDropMetricIsTotalOnly(t *testing.T) {
	handler := newBlockingDispatcherLogHandler()
	dispatcher := newTCPReconnectLogDispatcher(slog.New(handler), 1)
	defer func() {
		handler.release()
		dispatcher.Close()
	}()

	registry, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterTCPReconnectLogEventsDropped(func() float64 {
		return float64(dispatcher.EventsDropped())
	}); err != nil {
		t.Fatal(err)
	}

	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect log handler was not invoked")
	}
	dispatcher.LogReconnectRecovered(matrix.ReconnectRecovery{})
	dispatcher.LogReconnectAttempt(matrix.ReconnectAttempt{})
	dispatcher.LogReconnectRecovered(matrix.ReconnectRecovery{})
	dispatcher.LogReconnectFailure(matrix.ReconnectFailure{})

	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "matrix_proxy_tcp_reconnect_log_events_dropped_total" {
			continue
		}
		if got := len(family.GetMetric()); got != 1 {
			t.Fatalf("dropped reconnect log metric series = %d, want 1", got)
		}
		metric := family.GetMetric()[0]
		if labels := metric.GetLabel(); len(labels) != 0 {
			t.Fatalf("dropped reconnect log metric labels = %v, want total-only counter with no labels", labels)
		}
		if got := metric.GetCounter().GetValue(); got != 3 {
			t.Fatalf("dropped reconnect log metric value = %g, want 3", got)
		}
		return
	}
	t.Fatal("matrix_proxy_tcp_reconnect_log_events_dropped_total was not gathered")
}

type blockingDispatcherLogHandler struct {
	entered     chan struct{}
	exited      chan struct{}
	released    chan struct{}
	enterOnce   sync.Once
	exitOnce    sync.Once
	releaseOnce sync.Once
	handles     atomic.Uint64
}

func newBlockingDispatcherLogHandler() *blockingDispatcherLogHandler {
	return &blockingDispatcherLogHandler{
		entered:  make(chan struct{}),
		exited:   make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (h *blockingDispatcherLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *blockingDispatcherLogHandler) Handle(context.Context, slog.Record) error {
	h.handles.Add(1)
	h.enterOnce.Do(func() {
		close(h.entered)
	})
	<-h.released
	h.exitOnce.Do(func() {
		close(h.exited)
	})
	return nil
}

func (h *blockingDispatcherLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *blockingDispatcherLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *blockingDispatcherLogHandler) release() {
	h.releaseOnce.Do(func() {
		close(h.released)
	})
}

func assertReturnsQuickly(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("operation did not return promptly")
	}
}
