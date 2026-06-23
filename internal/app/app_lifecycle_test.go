package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/config"
	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/matrix"
)

func TestAppRunAfterCloseReturnsErrAppClosedWithoutBindingHTTP(t *testing.T) {
	cfg := newNeverRunAppTestConfig(t)
	occupied := occupyHTTPAddrForRunTest(t, &cfg.Server.Addr)
	defer occupied.Close()

	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	if err := application.Run(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Run() after Close() error = %v, want ErrAppClosed", err)
	}
}

func TestAppRunAfterExternalRunWorkersStopReturnsErrAppClosedWithoutBindingHTTP(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	occupied := occupyHTTPAddrForRunTest(t, &cfg.Server.Addr)
	defer occupied.Close()

	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	waitForActiveMatrixConnections(t, matrixServer, 1)
	cancel()
	waitAppWorkers(t, done)

	if err := application.Run(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Run() after external RunWorkers stop error = %v, want ErrAppClosed", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after external RunWorkers stop error = %v, want nil", err)
	}
}

func TestAppRunAfterShutdownReturnsErrAppClosedWithoutBindingHTTP(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	occupied := occupyHTTPAddrForRunTest(t, &cfg.Server.Addr)
	defer occupied.Close()

	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application, context.Background())
	waitForActiveMatrixConnections(t, matrixServer, 1)
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
	waitAppWorkers(t, done)

	if err := application.Run(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Run() after Shutdown() error = %v, want ErrAppClosed", err)
	}
}

func TestAppRunWithCanceledContextReturnsContextErrorWithoutBindingHTTP(t *testing.T) {
	cfg := newNeverRunAppTestConfig(t)
	occupied := occupyHTTPAddrForRunTest(t, &cfg.Server.Addr)
	defer occupied.Close()

	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := application.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() with canceled context error = %v, want context.Canceled", err)
	}
}

func TestAppRunContextCancellationReturnsNilAndCleansUp(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Server.Addr = reserveTCPAddrForRunTest(t)
	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runApp(t, application, ctx)
	waitForActiveMatrixConnections(t, matrixServer, 1)

	cancel()
	waitAppRun(t, done)
	waitForClosedMatrixConnections(t, matrixServer, 1)

	if err := application.RunWorkers(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("RunWorkers() after Run context cancellation error = %v, want ErrAppClosed", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after Run context cancellation error = %v, want nil", err)
	}
}

func TestAppRunWorkerFailureJoinsMatrixCloseFailure(t *testing.T) {
	matrixServer := newStatusFakeESPServer(t, lifecycleStatusUnknownCommand)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Server.Addr = reserveTCPAddrForRunTest(t)

	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("matrix close failure for test")
	application.matrix = failingCloseMatrixClient{matrixClientCloser: application.matrix, err: cleanupErr}

	err = application.Run(context.Background())
	if !errors.Is(err, matrix.ErrStatusUnknownCommand) {
		t.Fatalf("Run() error = %v, want matrix status root cause", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("Run() error = %v, want cleanup failure", err)
	}
	assertCompoundErrorContains(t, err, matrix.ErrStatusUnknownCommand, cleanupErr)
}

func TestAppRunListenFailureJoinsMatrixCloseFailure(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Server.Addr = occupied.Addr().String()
	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("matrix close failure for test")
	application.matrix = failingCloseMatrixClient{matrixClientCloser: application.matrix, err: cleanupErr}

	err = application.Run(context.Background())
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("Run() error = %v, want address already in use", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("Run() error = %v, want cleanup failure", err)
	}
	assertCompoundErrorContains(t, err, syscall.EADDRINUSE, cleanupErr)
}

func TestAppShutdownTimeoutRecoveryWhenEventWorkerIsBlocked(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	const blockedEventType = "shutdown-timeout-event-worker-block"
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releaseBlock := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	application.rules = blockingEventMapper{
		eventMapper: application.rules,
		eventType:   blockedEventType,
		entered:     entered,
		released:    release,
		enterOnce:   &enteredOnce,
	}

	done := runAppWorkers(t, application, context.Background())
	workersStopped := false
	defer func() {
		releaseBlock()
		if !workersStopped {
			waitAppWorkers(t, done)
		}
		_ = application.Shutdown(context.Background())
	}()
	waitForActiveMatrixConnections(t, matrixServer, 1)
	publishUntilEventWorkerBlocked(t, application, events.Event{Source: events.SourceExternal, Type: blockedEventType}, entered)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- application.Shutdown(shutdownCtx)
	}()

	if err := waitShutdownResult(t, shutdownDone); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want context deadline exceeded", err)
	}
	select {
	case err := <-done:
		t.Fatalf("RunWorkers() returned while event worker was blocked: %v", err)
	default:
	}
	if got := matrixServer.ClosedConnections(); got != 0 {
		t.Fatalf("matrix connections closed after timed-out Shutdown = %d, want 0", got)
	}
	if err := application.bus.Publish(context.Background(), events.Event{Source: events.SourceExternal, Type: "after-timeout"}); err != nil {
		t.Fatalf("bus Publish() after timed-out Shutdown error = %v, want nil", err)
	}

	releaseBlock()
	waitAppWorkers(t, done)
	workersStopped = true
	if got := matrixServer.ClosedConnections(); got != 0 {
		t.Fatalf("matrix connections closed after workers stopped before cleanup = %d, want 0", got)
	}
	if err := application.bus.Publish(context.Background(), events.Event{Source: events.SourceExternal, Type: "before-cleanup"}); err != nil {
		t.Fatalf("bus Publish() after workers stopped before cleanup error = %v, want nil", err)
	}

	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("follow-up Shutdown() error = %v, want nil", err)
	}
	waitForClosedMatrixConnections(t, matrixServer, 1)
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after follow-up Shutdown() error = %v, want nil", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close() after follow-up Shutdown() error = %v, want nil", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("second follow-up Shutdown() error = %v, want nil", err)
	}
	if err := application.bus.Publish(context.Background(), events.Event{Source: events.SourceExternal, Type: "after-cleanup"}); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("bus Publish() after cleanup error = %v, want ErrBusClosed", err)
	}
	if err := application.RunWorkers(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("RunWorkers() after shutdown timeout cleanup error = %v, want ErrAppClosed", err)
	}
	if err := application.Run(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Run() after shutdown timeout cleanup error = %v, want ErrAppClosed", err)
	}
	if got := matrixServer.ClosedConnections(); got != 1 {
		t.Fatalf("matrix connections closed after idempotent cleanup = %d, want 1", got)
	}
}

func TestEventWorkerReadinessDiagnosticsTrackMapStageAndClearAfterCancellation(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	const blockedEventType = "event-worker-readiness-map-block"
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	releaseBlock := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	application.rules = blockingEventMapper{
		eventMapper: application.rules,
		eventType:   blockedEventType,
		entered:     entered,
		released:    release,
		enterOnce:   &enteredOnce,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	workersStopped := false
	defer func() {
		releaseBlock()
		cancel()
		if !workersStopped {
			waitAppWorkers(t, done)
		}
		_ = application.Shutdown(context.Background())
	}()
	waitForActiveMatrixConnections(t, matrixServer, 1)
	publishUntilEventWorkerBlocked(t, application, events.Event{Source: events.SourceExternal, Type: blockedEventType}, entered)

	worker := waitForEventWorkerReady(t, application, eventWorkerStateProcessing, string(eventWorkerStageMap), true)
	if worker.ActiveDurationSeconds == nil || *worker.ActiveDurationSeconds <= 0 {
		t.Fatalf("event worker active duration = %v, want positive", worker.ActiveDurationSeconds)
	}

	cancel()
	releaseBlock()
	waitAppWorkers(t, done)
	workersStopped = true

	worker = waitForEventWorkerReady(t, application, eventWorkerStateIdle, "", false)
	if worker.ActiveDurationSeconds != nil {
		t.Fatalf("idle event worker active duration = %v, want nil", worker.ActiveDurationSeconds)
	}
}

func occupyHTTPAddrForRunTest(t *testing.T, addr *string) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	*addr = listener.Addr().String()
	return listener
}

func newHTTPMatrixTestConfig(t *testing.T, matrixAddr string) config.Config {
	t.Helper()
	host, portText, err := net.SplitHostPort(matrixAddr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	cfg := newNeverRunAppTestConfig(t)
	cfg.Matrix.Host = host
	cfg.Matrix.Port = port
	cfg.Matrix.ConnectTimeout = 20 * time.Millisecond
	cfg.Matrix.ResponseTimeout = time.Second
	cfg.Matrix.HeartbeatInterval = 20 * time.Millisecond
	cfg.Matrix.ProbeTimeout = 50 * time.Millisecond
	cfg.Matrix.ReconnectMinDelay = 10 * time.Millisecond
	cfg.Matrix.ReconnectMaxDelay = 50 * time.Millisecond
	return cfg
}

func newNeverRunAppTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Matrix.Host = "127.0.0.1"
	cfg.Matrix.Port = 1
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = writeRulesFile(t)
	return cfg
}

func writeRulesFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/rules.yaml"
	data := []byte(`rules:
  - id: http_notify_default
    when:
      source: http
      type: notify
    play:
      animation: notification
      priority: 50
      duration: 2s
      interrupt: none
      restore: background
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runAppWorkers(t *testing.T, application *App, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- application.RunWorkers(ctx)
	}()
	return done
}

func runApp(t *testing.T, application *App, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- application.Run(ctx)
	}()
	return done
}

func waitAppWorkers(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWorkers() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWorkers() did not stop")
	}
}

func waitAppRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop")
	}
}

func waitForActiveMatrixConnections(t *testing.T, server *lifecycleFakeESPServer, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := server.ActiveConnections(); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active matrix connections = %d, want %d", server.ActiveConnections(), want)
}

func waitForClosedMatrixConnections(t *testing.T, server *lifecycleFakeESPServer, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := server.ClosedConnections(); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("closed matrix connections = %d, want %d", server.ClosedConnections(), want)
}

func waitShutdownResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("Shutdown() did not return")
		return nil
	}
}

func publishUntilEventWorkerBlocked(t *testing.T, application *App, event events.Event, entered <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := application.bus.Publish(context.Background(), event); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case <-entered:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("event worker did not enter blocked mapping path")
}

func waitForEventWorkerReady(t *testing.T, application *App, wantState, wantStage string, wantDuration bool) eventWorkerReady {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last eventWorkerReady
	for time.Now().Before(deadline) {
		ready, _ := application.readiness()
		last = ready.EventWorker
		durationPresent := last.ActiveDurationSeconds != nil
		if last.State == wantState && last.Stage == wantStage && durationPresent == wantDuration {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event worker readiness = %#v, want state=%q stage=%q duration_present=%v",
		last, wantState, wantStage, wantDuration)
	return eventWorkerReady{}
}

type failingCloseMatrixClient struct {
	matrixClientCloser
	err error
}

func (c failingCloseMatrixClient) Close() error {
	return errors.Join(c.matrixClientCloser.Close(), c.err)
}

type blockingEventMapper struct {
	eventMapper
	eventType string
	entered   chan<- struct{}
	released  <-chan struct{}
	enterOnce *sync.Once
}

func (m blockingEventMapper) Map(event events.Event) (animations.AnimationRequest, bool) {
	if event.Type == m.eventType {
		m.enterOnce.Do(func() {
			close(m.entered)
		})
		<-m.released
	}
	return m.eventMapper.Map(event)
}

func assertCompoundErrorContains(t *testing.T, err error, wantRunErr error, wantCleanupErr error) {
	t.Helper()
	if err == nil {
		t.Fatal("Run() error = nil, want compound error")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("Run() error type %T does not expose joined causes", err)
	}
	causes := joined.Unwrap()
	if len(causes) < 2 {
		t.Fatalf("Run() joined error causes = %d, want at least 2", len(causes))
	}
	if !errors.Is(err, wantRunErr) {
		t.Fatalf("Run() error = %v, want run failure %v", err, wantRunErr)
	}
	if !errors.Is(err, wantCleanupErr) {
		t.Fatalf("Run() error = %v, want cleanup failure %v", err, wantCleanupErr)
	}
}

func reserveTCPAddrForRunTest(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

const (
	lifecycleMagic0          byte = 0x4C
	lifecycleMagic1          byte = 0x4D
	lifecycleProtocolVersion byte = 0x01
	lifecycleResponseCommand byte = 0x80
	lifecycleStatusOK        byte = 0x00

	lifecycleStatusUnknownCommand byte = 0x03
)

type lifecycleFakeESPServer struct {
	listener          net.Listener
	status            byte
	active            atomic.Int32
	closedConnections atomic.Int32
	closed            atomic.Bool
}

func newFakeESPServer(t *testing.T) *lifecycleFakeESPServer {
	t.Helper()
	return newStatusFakeESPServer(t, lifecycleStatusOK)
}

func newStatusFakeESPServer(t *testing.T, status byte) *lifecycleFakeESPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &lifecycleFakeESPServer{
		listener: listener,
		status:   status,
	}
	go server.serve()
	return server
}

func (s *lifecycleFakeESPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *lifecycleFakeESPServer) ActiveConnections() int32 {
	return s.active.Load()
}

func (s *lifecycleFakeESPServer) ClosedConnections() int32 {
	return s.closedConnections.Load()
}

func (s *lifecycleFakeESPServer) Close() {
	if s.closed.Swap(true) {
		return
	}
	_ = s.listener.Close()
}

func (s *lifecycleFakeESPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *lifecycleFakeESPServer) handle(conn net.Conn) {
	s.active.Add(1)
	defer func() {
		s.active.Add(-1)
		s.closedConnections.Add(1)
		_ = conn.Close()
	}()

	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		remaining := make([]byte, int(header[4])+1)
		if _, err := io.ReadFull(conn, remaining); err != nil {
			return
		}
		response := []byte{
			lifecycleMagic0,
			lifecycleMagic1,
			lifecycleProtocolVersion,
			lifecycleResponseCommand,
			s.status,
		}
		response = append(response, lifecycleXORChecksum(response))
		if _, err := conn.Write(response); err != nil {
			return
		}
	}
}

func lifecycleXORChecksum(data []byte) byte {
	var checksum byte
	for _, value := range data {
		checksum ^= value
	}
	return checksum
}
