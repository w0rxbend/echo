package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/app"
	"github.com/worxbend/echo/internal/config"
	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/integrations/httpapi"
	"github.com/worxbend/echo/internal/matrix"
)

const (
	testCommandPing      byte = 0x00
	testCommandClear     byte = 0x01
	testCommandFill      byte = 0x03
	testCommandSetFrame  byte = 0x05
	testCommandSetPreset byte = 0x08
	testMagic0           byte = 0x4C
	testMagic1           byte = 0x4D
	testProtocolVersion  byte = 0x01
	testResponseCommand  byte = 0x80
	testFramePayloadSize      = 192

	testStatusOK             byte = 0x00
	testStatusUnknownCommand byte = 0x03
)

func TestAppNewAllowsLocalAdminWithoutToken(t *testing.T) {
	for _, addr := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"} {
		t.Run(addr, func(t *testing.T) {
			cfg := newAdminAuthTestConfig(t)
			cfg.Server.Addr = addr
			cfg.Server.AdminTokenEnv = ""

			if _, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
				t.Fatalf("app.New() error = %v", err)
			}
		})
	}
}

func TestAppNewRejectsNonLocalAdminWithoutTokenConfig(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "matrix.local:8080", "192.168.1.20:8080"} {
		t.Run(addr, func(t *testing.T) {
			cfg := newAdminAuthTestConfig(t)
			cfg.Server.Addr = addr
			cfg.Server.AdminTokenEnv = ""

			_, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil {
				t.Fatal("app.New() error = nil, want missing admin token env error")
			}
			if !strings.Contains(err.Error(), "server.admin_token_env is required") {
				t.Fatalf("app.New() error = %q, want server.admin_token_env is required", err)
			}
		})
	}
}

func TestAppNewRejectsNonLocalAdminWithUnsetOrBlankToken(t *testing.T) {
	const tokenEnv = "ECHO_TEST_ADMIN_TOKEN"

	t.Run("unset", func(t *testing.T) {
		unsetEnv(t, tokenEnv)
		cfg := newAdminAuthTestConfig(t)
		cfg.Server.Addr = "0.0.0.0:8080"
		cfg.Server.AdminTokenEnv = tokenEnv

		_, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil {
			t.Fatal("app.New() error = nil, want unset admin token error")
		}
		if !strings.Contains(err.Error(), `server.admin_token_env "ECHO_TEST_ADMIN_TOKEN" is unset or blank`) {
			t.Fatalf("app.New() error = %q, want unset or blank token error", err)
		}
	})

	t.Run("blank", func(t *testing.T) {
		t.Setenv(tokenEnv, " \t")
		cfg := newAdminAuthTestConfig(t)
		cfg.Server.Addr = ":8080"
		cfg.Server.AdminTokenEnv = tokenEnv

		_, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil {
			t.Fatal("app.New() error = nil, want blank admin token error")
		}
		if !strings.Contains(err.Error(), `server.admin_token_env "ECHO_TEST_ADMIN_TOKEN" is unset or blank`) {
			t.Fatalf("app.New() error = %q, want unset or blank token error", err)
		}
	})
}

func TestAdminRoutesRequireBearerTokenForNonLocalBind(t *testing.T) {
	const tokenEnv = "ECHO_TEST_ADMIN_TOKEN"
	const token = "test-secret-token"
	t.Setenv(tokenEnv, token)

	cfg := newAdminAuthTestConfig(t)
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Server.AdminTokenEnv = tokenEnv

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	tests := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "bad", authorization: "Bearer wrong-token", wantStatus: http.StatusForbidden},
		{name: "valid", authorization: "Bearer " + token, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodDelete, httpServer.URL+"/api/v1/queue", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				data, _ := io.ReadAll(resp.Body)
				t.Fatalf("DELETE /queue status = %d, body = %s, want %d", resp.StatusCode, data, tt.wantStatus)
			}
		})
	}
}

func TestQueueInspectionRequiresBearerTokenForNonLocalBind(t *testing.T) {
	const tokenEnv = "ECHO_TEST_QUEUE_ADMIN_TOKEN"
	const token = "test-secret-token"
	t.Setenv(tokenEnv, token)

	cfg := newAdminAuthTestConfig(t)
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Server.AdminTokenEnv = tokenEnv

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	tests := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "bad", authorization: "Bearer wrong-token", wantStatus: http.StatusForbidden},
		{name: "valid", authorization: "Bearer " + token, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/queue", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				data, _ := io.ReadAll(resp.Body)
				t.Fatalf("GET /queue status = %d, body = %s, want %d", resp.StatusCode, data, tt.wantStatus)
			}
		})
	}
}

func TestQueueInspectionAllowsLocalBindWithoutToken(t *testing.T) {
	cfg := newAdminAuthTestConfig(t)
	cfg.Server.Addr = "127.0.0.1:8080"
	cfg.Server.AdminTokenEnv = ""

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL + "/api/v1/queue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /queue status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}
}

func TestReadyzUnavailableBeforeWorkersStart(t *testing.T) {
	cfg := newAdminAuthTestConfig(t)
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	assertStatus(t, httpServer.URL+"/healthz", http.StatusOK)
	assertStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
}

func TestReadyzAvailableAfterWorkersConnectMatrix(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	body := waitForReadyDetails(t, httpServer.URL+"/readyz", http.StatusOK)
	if body.EventWorker.State != "idle" {
		t.Fatalf("readyz event_worker.state = %q, want idle", body.EventWorker.State)
	}
	if body.EventWorker.ActiveDurationSeconds != nil {
		t.Fatalf("readyz idle event_worker.active_duration_seconds = %v, want nil", body.EventWorker.ActiveDurationSeconds)
	}
}

func TestMetricsExposeTCPImmediateReconnectAfterDroppedCommandResponse(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.DropNextResponse(testCommandFill)
	resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":4,"g":5,"b":6}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /matrix/fill status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="tcp_immediate"`, `error_kind="retryable"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="tcp_immediate"`, `state="ready"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="transport_error"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="ok"`, " 1")

	if got := matrixServer.CommandCount(testCommandFill); got != 2 {
		t.Fatalf("fill command frames = %d, want 2", got)
	}
	if matrixServer.Pipelined() {
		t.Fatal("client sent another matrix command before receiving a response")
	}
}

func TestMetricsExposeTCPImmediateReconnectFailureWhenReconnectFails(t *testing.T) {
	matrixServer := newFakeESPServer(t)

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.Close()
	reqCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, httpServer.URL+"/api/v1/matrix/fill", bytes.NewBufferString(`{"r":4,"g":5,"b":6}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="tcp_immediate"`, `error_kind="retryable"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
		`source="tcp_immediate"`, `error_kind="retryable"`, `outcome="failed"`, " 1")
}

func TestMetricsExposeTCPImmediateReconnectFailureWhenRetryTransportFails(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	donePost := make(chan int, 1)
	fillCount := matrixServer.CommandCount(testCommandFill)
	matrixServer.DropResponseOnCommandCount(testCommandFill, fillCount+1)
	matrixServer.DropResponseOnCommandCount(testCommandFill, fillCount+2)
	go func() {
		resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":4,"g":5,"b":6}`))
		if err != nil {
			donePost <- 0
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		donePost <- resp.StatusCode
	}()
	waitForMatrixCommand(t, matrixServer, testCommandFill)
	select {
	case status := <-donePost:
		if status != http.StatusOK && status != http.StatusBadGateway {
			t.Fatalf("POST /matrix/fill status = %d, want %d or %d; fill command count = %d", status, http.StatusOK, http.StatusBadGateway, matrixServer.CommandCount(testCommandFill))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("POST /matrix/fill did not return after retry transport failure")
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="tcp_immediate"`, `state="ready"`, " 1")
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
		`source="tcp_immediate"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="transport_error"`, " 2")
	if got, wantMin := matrixServer.CommandCount(testCommandFill), fillCount+2; got < wantMin {
		t.Fatalf("fill command frames = %d, want at least %d", got, wantMin)
	}
}

func TestMetricsExposeTCPImmediateRecoveryWithoutFailureOnPermanentRetryErrors(t *testing.T) {
	tests := []struct {
		name       string
		setupRetry func(*fakeESPServer)
		wantStatus int
		metricPart string
	}{
		{
			name: "firmware_status",
			setupRetry: func(server *fakeESPServer) {
				server.FailNextStatus(testCommandFill, testStatusUnknownCommand)
			},
			wantStatus: http.StatusBadGateway,
			metricPart: `status="unknown_command"`,
		},
		{
			name: "protocol_response_validation",
			setupRetry: func(server *fakeESPServer) {
				server.CorruptNextResponse(testCommandFill)
			},
			wantStatus: http.StatusBadGateway,
			metricPart: `status="protocol_error"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matrixServer := newFakeESPServer(t)
			defer matrixServer.Close()

			cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
			application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}

			done := runAppWorkers(t, application)
			defer func() {
				shutdownAppWorkers(t, application, done)
			}()

			httpServer := httptest.NewServer(application.Handler())
			defer httpServer.Close()
			waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

			matrixServer.DropNextResponse(testCommandFill)
			tt.setupRetry(matrixServer)
			resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":4,"g":5,"b":6}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				data, _ := io.ReadAll(resp.Body)
				t.Fatalf("POST /matrix/fill status = %d, body = %s, want %d", resp.StatusCode, data, tt.wantStatus)
			}

			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
				`source="tcp_immediate"`, `error_kind="retryable"`, " 1")
			assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
				[]string{"error_kind", "source"}, `source="tcp_immediate"`, `error_kind="retryable"`)
			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
				`source="tcp_immediate"`, `state="ready"`, " 1")
			assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
				[]string{"source", "state"}, `source="tcp_immediate"`, `state="ready"`)
			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
				`command="fill"`, `status="transport_error"`, " 1")
			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
				`command="fill"`, tt.metricPart, " 1")
			assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
				[]string{"command", "status"}, `command="fill"`, tt.metricPart)
			assertNoMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total", `source="tcp_immediate"`)
			if got := matrixServer.CommandCount(testCommandFill); got != 2 {
				t.Fatalf("fill command frames = %d, want 2", got)
			}
		})
	}
}

func TestMetricsDoNotExposeTCPImmediateReadyRecoveryBeforeRetryPingValid(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Matrix.HeartbeatInterval = 200 * time.Millisecond
	cfg.Matrix.ProbeTimeout = 100 * time.Millisecond
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pingCount := matrixServer.CommandCount(testCommandPing)
	matrixServer.DropResponseOnCommandCount(testCommandPing, pingCount+1)
	matrixServer.CloseConnections()

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="tcp_immediate"`, `error_kind="retryable"`, " 1")
	body := waitForReadyDetails(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
	if body.Status != "not_ready" {
		t.Fatalf("readyz status = %q, want not_ready", body.Status)
	}
	if body.MatrixConnected {
		t.Fatal("readyz matrix_connected = true, want false after invalid retry ping")
	}
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="tcp_immediate"`, `state="ready"`)

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="tcp_immediate"`, `state="ready"`)

	matrixServer.CloseConnections()
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="tcp_immediate"`, `state="ready"`, " 1")
}

func TestMetricsExposeTCPImmediateRetryPingVerificationFailure(t *testing.T) {
	tests := []struct {
		name       string
		setupRetry func(*fakeESPServer)
		metricPart string
		wantErr    error
	}{
		{
			name: "firmware_status",
			setupRetry: func(server *fakeESPServer) {
				server.FailNextStatus(testCommandPing, testStatusUnknownCommand)
			},
			metricPart: `status="unknown_command"`,
			wantErr:    matrix.ErrStatusUnknownCommand,
		},
		{
			name: "protocol_response_validation",
			setupRetry: func(server *fakeESPServer) {
				server.CorruptNextResponse(testCommandPing)
			},
			metricPart: `status="protocol_error"`,
			wantErr:    matrix.ErrProtocol,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matrixServer := newFakeESPServer(t)
			defer matrixServer.Close()

			cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
			cfg.Matrix.HeartbeatInterval = 200 * time.Millisecond
			cfg.Matrix.ProbeTimeout = 100 * time.Millisecond
			cfg.Matrix.ReconnectMinDelay = 10 * time.Second
			cfg.Matrix.ReconnectMaxDelay = 10 * time.Second
			application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}

			done := runAppWorkers(t, application)
			defer func() {
				shutdownAppWorkersExpectingError(t, application, done, tt.wantErr)
			}()

			httpServer := httptest.NewServer(application.Handler())
			defer httpServer.Close()
			waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

			pingCount := matrixServer.CommandCount(testCommandPing)
			matrixServer.DropResponseOnCommandCount(testCommandPing, pingCount+1)
			tt.setupRetry(matrixServer)

			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
				`source="tcp_immediate"`, `error_kind="retryable"`, " 1")
			assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
				[]string{"error_kind", "source"}, `source="tcp_immediate"`, `error_kind="retryable"`)
			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
				`source="tcp_immediate"`, `error_kind="permanent"`, `outcome="verification_failed"`, " 1")
			assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
				[]string{"error_kind", "outcome", "source"}, `source="tcp_immediate"`, `error_kind="permanent"`, `outcome="verification_failed"`)
			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
				`command="ping"`, `status="transport_error"`, " 1")
			waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
				`command="ping"`, tt.metricPart, " 1")
			assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
				[]string{"command", "status"}, `command="ping"`, tt.metricPart)
			assertNoMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
				`source="tcp_immediate"`, `state="ready"`)

			body := waitForReadyDetails(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
			if body.Status != "not_ready" {
				t.Fatalf("readyz status = %q, want not_ready", body.Status)
			}
			if body.WorkersRunning && body.MatrixConnected {
				t.Fatalf("readyz reported running recovered matrix after retry-ping verification failure: %#v", body)
			}
		})
	}
}

func TestReadyzUnavailableAfterIdleMatrixDisconnect(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	matrixServer.Close()

	body := waitForReadyDetails(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
	if body.Status != "not_ready" {
		t.Fatalf("readyz status = %q, want not_ready", body.Status)
	}
	if !body.WorkersRunning {
		t.Fatal("readyz workers_running = false, want true")
	}
	if body.Draining {
		t.Fatal("readyz draining = true, want false")
	}
	if body.SchedulerState != "disconnected" {
		t.Fatalf("readyz scheduler_state = %q, want disconnected", body.SchedulerState)
	}
	if body.MatrixConnected {
		t.Fatal("readyz matrix_connected = true, want false")
	}
	if body.LastSuccess == nil {
		t.Fatal("readyz last_success = nil, want timestamp")
	}
	if body.LastFailure == nil {
		t.Fatal("readyz last_failure = nil, want timestamp")
	}
}

func TestReadyzUnavailableWhenMatrixUnavailable(t *testing.T) {
	host, port := unusedLocalTCPAddr(t)
	cfg := newHTTPMatrixTestConfigForHostPort(t, host, port)
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	assertStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
	time.Sleep(30 * time.Millisecond)
	assertStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
}

func TestReadyzUnavailableAfterWorkersStop(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkersWithContext(t, application, ctx)
	workersStopped := false
	defer func() {
		if !workersStopped {
			cancel()
			waitAppWorkers(t, done)
		}
		shutdownApp(t, application)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	cancel()
	waitAppWorkers(t, done)
	workersStopped = true
	assertStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
}

func TestNotifyStreamsFramesAndRestoresBackground(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	host, portText, err := net.SplitHostPort(matrixServer.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	rulesPath := writeRulesFile(t)
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Matrix.Host = host
	cfg.Matrix.Port = port
	cfg.Matrix.ConnectTimeout = time.Second
	cfg.Matrix.ResponseTimeout = time.Second
	cfg.Matrix.ReconnectMinDelay = 10 * time.Millisecond
	cfg.Matrix.ReconnectMaxDelay = 50 * time.Millisecond
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = rulesPath
	cfg.Background.Animation = "matrix_rain_background"
	cfg.Background.RestoreOnIdle = true
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, "matrix_rain_background", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    animations.RGB{G: 255, B: 85},
	})

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	t.Cleanup(func() {
		shutdownAppWorkers(t, application, done)
	})

	time.Sleep(20 * time.Millisecond)
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	body := bytes.NewBufferString(`{"title":"Test","message":"hello","duration":"50ms"}`)
	resp, err := http.Post(httpServer.URL+"/api/v1/notify", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /notify status = %d, body = %s", resp.StatusCode, data)
	}

	var sawFrame bool
	var sawPreset bool
	var commandCount int
	deadline := time.After(3 * time.Second)
	for !sawFrame || !sawPreset {
		select {
		case frame := <-matrixServer.frames:
			commandCount++
			switch frame.Command {
			case testCommandPing:
			case testCommandSetFrame:
				sawFrame = true
				if len(frame.Payload) != testFramePayloadSize {
					t.Fatalf("SetFullFrame payload length = %d, want %d", len(frame.Payload), testFramePayloadSize)
				}
				if allZero(frame.Payload) {
					t.Fatal("SetFullFrame payload is all zero, want generated notification pixels")
				}
			case testCommandSetPreset:
				sawPreset = true
				want := []byte{12, 90, 0, 0, 255, 85}
				if !bytes.Equal(frame.Payload, want) {
					t.Fatalf("Preset payload = %v, want %v", frame.Payload, want)
				}
			default:
				t.Fatalf("unexpected matrix command 0x%02x", frame.Command)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for frame and background restore; sawFrame=%v sawPreset=%v", sawFrame, sawPreset)
		}
	}

	waitForResponses(t, matrixServer, commandCount)
	if matrixServer.Pipelined() {
		t.Fatal("client sent another matrix command before receiving a response")
	}
}

func registryWithFirmwarePreset(t *testing.T, id string, preset animations.FirmwarePreset) *animations.Registry {
	t.Helper()
	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterFirmwarePreset(id, preset); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestAnimationsEndpointListsOnlyRenderableAnimations(t *testing.T) {
	httpServer := newAnimationAPITestServer(t)
	const firmwarePresetID = "matrix_rain_background"

	resp, err := http.Get(httpServer.URL + "/api/v1/animations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /animations status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}

	var body struct {
		Animations []string `json:"animations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if got, want := body.Animations, []string{animations.NotificationAnimationID}; !equalStrings(got, want) {
		t.Fatalf("GET /animations animations = %v, want %v", got, want)
	}
	if containsString(body.Animations, firmwarePresetID) {
		t.Fatalf("GET /animations animations = %v, must exclude firmware preset %q", body.Animations, firmwarePresetID)
	}
}

func TestAnimationCatalogEndpointIncludesNonPlayableMetadata(t *testing.T) {
	httpServer := newAnimationAPITestServer(t)
	const firmwarePresetID = "matrix_rain_background"

	resp, err := http.Get(httpServer.URL + "/api/v1/animations/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /animations/catalog status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}

	var body struct {
		Animations []map[string]any `json:"animations"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}

	type catalogEntry struct {
		ID       string
		Kind     string
		Playable bool
	}
	want := map[string]catalogEntry{
		firmwarePresetID:                   {ID: firmwarePresetID, Kind: string(animations.PublicKindFirmwarePreset), Playable: false},
		animations.NotificationAnimationID: {ID: animations.NotificationAnimationID, Kind: string(animations.PublicKindGenerated), Playable: true},
	}
	seen := map[string]bool{}
	allowedFields := map[string]bool{
		"id":        true,
		"kind":      true,
		"playable":  true,
		"effect_id": true,
		"interval":  true,
		"color":     true,
	}
	for i, entry := range body.Animations {
		for field := range entry {
			if !allowedFields[field] {
				t.Fatalf("GET /animations/catalog entry %d id=%v leaked unsupported field %q: %+v", i, entry["id"], field, entry)
			}
		}
		id, ok := entry["id"].(string)
		if !ok {
			t.Fatalf("GET /animations/catalog entry %d missing stable string field id: %+v", i, entry)
		}
		kind, ok := entry["kind"].(string)
		if !ok {
			t.Fatalf("GET /animations/catalog entry %d missing stable string field kind: %+v", i, entry)
		}
		playable, ok := entry["playable"].(bool)
		if !ok {
			t.Fatalf("GET /animations/catalog entry %d missing stable bool field playable: %+v", i, entry)
		}

		if kind == "renderable" {
			t.Fatalf("GET /animations/catalog entry %d id=%s leaked internal kind %q", i, id, kind)
		}
		switch kind {
		case string(animations.PublicKindGenerated), string(animations.PublicKindFirmwarePreset):
		default:
			t.Fatalf("GET /animations/catalog entry %d id=%s has unsupported kind %q", i, id, kind)
		}
		if kind == string(animations.PublicKindGenerated) && !playable {
			t.Fatalf("GET /animations/catalog entry %d id=%s kind=generated must have playable=true", i, id)
		}
		if kind == string(animations.PublicKindFirmwarePreset) && playable {
			t.Fatalf("GET /animations/catalog entry %d id=%s kind=firmware_preset must have playable=false", i, id)
		}
		if kind == string(animations.PublicKindGenerated) {
			for _, field := range []string{"effect_id", "interval", "color"} {
				if _, ok := entry[field]; ok {
					t.Fatalf("GET /animations/catalog generated entry %d id=%s leaked firmware metadata field %q: %+v", i, id, field, entry)
				}
			}
		}
		if effectID, ok := entry["effect_id"]; ok {
			if _, ok := effectID.(json.Number); !ok {
				t.Fatalf("GET /animations/catalog firmware entry %d id=%s effect_id has type %T, want JSON number", i, id, effectID)
			}
		}
		if interval, ok := entry["interval"]; ok {
			if _, ok := interval.(string); !ok {
				t.Fatalf("GET /animations/catalog firmware entry %d id=%s interval has type %T, want JSON string", i, id, interval)
			}
		}
		if color, ok := entry["color"]; ok {
			colorObject, ok := color.(map[string]any)
			if !ok {
				t.Fatalf("GET /animations/catalog firmware entry %d id=%s color has type %T, want JSON object", i, id, color)
			}
			if _, ok := colorObject["r"].(json.Number); !ok {
				t.Fatalf("GET /animations/catalog firmware entry %d id=%s color.r has type %T, want JSON number", i, id, colorObject["r"])
			}
			if _, ok := colorObject["g"].(json.Number); !ok {
				t.Fatalf("GET /animations/catalog firmware entry %d id=%s color.g has type %T, want JSON number", i, id, colorObject["g"])
			}
			if _, ok := colorObject["b"].(json.Number); !ok {
				t.Fatalf("GET /animations/catalog firmware entry %d id=%s color.b has type %T, want JSON number", i, id, colorObject["b"])
			}
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("GET /animations/catalog has duplicate id %q", id)
		}
		seen[id] = true

		expected, ok := want[id]
		if !ok {
			continue
		}
		if kind != expected.Kind || playable != expected.Playable {
			t.Fatalf("GET /animations/catalog id=%s fields %s = %+v, want %+v", id, id, catalogEntry{ID: id, Kind: kind, Playable: playable}, expected)
		}
		if id == firmwarePresetID {
			effectID, ok := entry["effect_id"].(json.Number)
			if !ok || effectID.String() != "12" {
				t.Fatalf("GET /animations/catalog firmware preset effect_id = %v (%T), want JSON number 12", entry["effect_id"], entry["effect_id"])
			}
			interval, ok := entry["interval"].(string)
			if !ok || interval != "90ms" {
				t.Fatalf("GET /animations/catalog firmware preset interval = %v (%T), want JSON string %q", entry["interval"], entry["interval"], "90ms")
			}
			color, ok := entry["color"].(map[string]any)
			if !ok {
				t.Fatalf("GET /animations/catalog firmware preset color = %v (%T), want JSON object", entry["color"], entry["color"])
			}
			if got, ok := color["r"].(json.Number); !ok || got.String() != "0" {
				t.Fatalf("GET /animations/catalog firmware preset color.r = %v (%T), want JSON number 0", color["r"], color["r"])
			}
			if got, ok := color["g"].(json.Number); !ok || got.String() != "255" {
				t.Fatalf("GET /animations/catalog firmware preset color.g = %v (%T), want JSON number 255", color["g"], color["g"])
			}
			if got, ok := color["b"].(json.Number); !ok || got.String() != "85" {
				t.Fatalf("GET /animations/catalog firmware preset color.b = %v (%T), want JSON number 85", color["b"], color["b"])
			}
		}
	}
	for id, expected := range want {
		if !seen[id] {
			t.Fatalf("GET /animations/catalog missing required entry: %+v", expected)
		}
	}
}

func TestConfigAuthoredFrameAnimationPublicSurfaces(t *testing.T) {
	const (
		frameAnimationID = "pixel_badge"
		firmwarePresetID = "matrix_rain_background"
	)
	httpServer := newConfigAuthoredFrameAnimationAPITestServer(t)

	resp, err := http.Get(httpServer.URL + "/api/v1/animations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /animations status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}
	var animationsBody struct {
		Animations []string `json:"animations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&animationsBody); err != nil {
		t.Fatal(err)
	}
	if !containsString(animationsBody.Animations, frameAnimationID) {
		t.Fatalf("GET /animations animations = %v, want %q", animationsBody.Animations, frameAnimationID)
	}
	if containsString(animationsBody.Animations, firmwarePresetID) {
		t.Fatalf("GET /animations animations = %v, must exclude firmware preset %q", animationsBody.Animations, firmwarePresetID)
	}

	catalogResp, err := http.Get(httpServer.URL + "/api/v1/animations/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("GET /animations/catalog status = %d, body = %s, want %d", catalogResp.StatusCode, data, http.StatusOK)
	}
	var catalogBody struct {
		Animations []map[string]any `json:"animations"`
	}
	decoder := json.NewDecoder(catalogResp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&catalogBody); err != nil {
		t.Fatal(err)
	}
	var frameCatalog map[string]any
	for i, entry := range catalogBody.Animations {
		id, _ := entry["id"].(string)
		kind, _ := entry["kind"].(string)
		if kind == "renderable" {
			t.Fatalf("GET /animations/catalog entry %d id=%s leaked internal kind %q", i, id, kind)
		}
		if id == frameAnimationID {
			frameCatalog = entry
		}
	}
	if frameCatalog == nil {
		t.Fatalf("GET /animations/catalog missing %q: %+v", frameAnimationID, catalogBody.Animations)
	}
	if got := frameCatalog["kind"]; got != string(animations.PublicKindGenerated) {
		t.Fatalf("GET /animations/catalog %s kind = %v, want %q", frameAnimationID, got, animations.PublicKindGenerated)
	}
	if got := frameCatalog["playable"]; got != true {
		t.Fatalf("GET /animations/catalog %s playable = %v, want true", frameAnimationID, got)
	}
	for _, field := range []string{"effect_id", "interval", "color"} {
		if _, ok := frameCatalog[field]; ok {
			t.Fatalf("GET /animations/catalog frame animation leaked firmware metadata field %q: %+v", field, frameCatalog)
		}
	}

	playResp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", bytes.NewBufferString(`{"animation":"pixel_badge","duration":"50ms","restore":"leave"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer playResp.Body.Close()
	if data, err := io.ReadAll(playResp.Body); err != nil {
		t.Fatal(err)
	} else if playResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /play frame animation status = %d, body = %s, want %d", playResp.StatusCode, data, http.StatusAccepted)
	}
	waitForQueueDepth(t, httpServer.URL, 1)

	eventsResp, err := http.Post(httpServer.URL+"/api/v1/events", "application/json", bytes.NewBufferString(`{"type":"notify","attributes":{"animation":"pixel_badge","duration":"50ms","restore":"leave"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer eventsResp.Body.Close()
	if data, err := io.ReadAll(eventsResp.Body); err != nil {
		t.Fatal(err)
	} else if eventsResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /events frame override status = %d, body = %s, want %d", eventsResp.StatusCode, data, http.StatusAccepted)
	}

	firmwareResp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", bytes.NewBufferString(`{"animation":"matrix_rain_background","duration":"50ms","restore":"leave"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer firmwareResp.Body.Close()
	assertFirmwarePresetRejected(t, "POST /play", firmwareResp)
	waitForQueueDepth(t, httpServer.URL, 1)
}

func TestFirmwarePresetIsNotPlayableThroughPublicAnimationIngress(t *testing.T) {
	const firmwarePresetID = "matrix_rain_background"

	t.Run("catalog", func(t *testing.T) {
		type catalogColor struct {
			R byte `json:"r"`
			G byte `json:"g"`
			B byte `json:"b"`
		}
		httpServer := newAnimationAPITestServer(t)
		resp, err := http.Get(httpServer.URL + "/api/v1/animations/catalog")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET /animations/catalog status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
		}

		var body struct {
			Animations []struct {
				ID       string        `json:"id"`
				Kind     string        `json:"kind"`
				Playable bool          `json:"playable"`
				EffectID *byte         `json:"effect_id"`
				Interval *string       `json:"interval"`
				Color    *catalogColor `json:"color"`
			} `json:"animations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, entry := range body.Animations {
			switch entry.Kind {
			case string(animations.PublicKindGenerated):
				if entry.EffectID != nil || entry.Interval != nil || entry.Color != nil {
					t.Fatalf("generated catalog entry must not include firmware preset metadata: %+v", entry)
				}
			case string(animations.PublicKindFirmwarePreset):
				if entry.ID != firmwarePresetID {
					continue
				}
				if entry.EffectID == nil || *entry.EffectID != 12 {
					t.Fatalf("catalog firmware preset entry %s has effect_id=%v, want 12", firmwarePresetID, entry.EffectID)
				}
				if entry.Interval == nil || *entry.Interval != "90ms" {
					t.Fatalf("catalog firmware preset entry %s has interval=%v, want %q", firmwarePresetID, entry.Interval, "90ms")
				}
				if entry.Color == nil || entry.Color.R != 0 || entry.Color.G != 255 || entry.Color.B != 85 {
					t.Fatalf("catalog firmware preset entry %s has color=%+v, want {r:0 g:255 b:85}", firmwarePresetID, entry.Color)
				}
				if entry.Playable {
					t.Fatalf("catalog firmware preset entry = %+v, want playable=false", entry)
				}
				return
			default:
				t.Fatalf("GET /animations/catalog entry %q has unsupported kind %q", entry.ID, entry.Kind)
			}
		}
		t.Fatalf("catalog missing firmware preset %q: %+v", firmwarePresetID, body.Animations)
	})

	t.Run("play", func(t *testing.T) {
		httpServer := newAnimationAPITestServer(t)
		body := bytes.NewBufferString(fmt.Sprintf(`{"animation":%q,"duration":"50ms","restore":"leave"}`, firmwarePresetID))
		resp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		assertFirmwarePresetRejected(t, "POST /play", resp)
		waitForQueueDepth(t, httpServer.URL, 0)
	})

	t.Run("notify", func(t *testing.T) {
		httpServer := newAnimationAPITestServer(t)
		body := bytes.NewBufferString(fmt.Sprintf(`{"title":"Test","message":"hello","animation":%q,"duration":"50ms"}`, firmwarePresetID))
		resp, err := http.Post(httpServer.URL+"/api/v1/notify", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		assertFirmwarePresetRejected(t, "POST /notify", resp)
	})

	t.Run("events", func(t *testing.T) {
		httpServer := newAnimationAPITestServer(t)
		body := bytes.NewBufferString(fmt.Sprintf(`{"type":"notify","attributes":{"animation":%q}}`, firmwarePresetID))
		resp, err := http.Post(httpServer.URL+"/api/v1/events", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		assertFirmwarePresetRejected(t, "POST /events", resp)
	})
}

func assertFirmwarePresetRejected(t *testing.T, operation string, resp *http.Response) {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%s status = %d, body = %s, want %d", operation, resp.StatusCode, data, http.StatusBadRequest)
	}
	for _, part := range []string{"matrix_rain_background", "not renderable/playable"} {
		if !strings.Contains(string(data), part) {
			t.Fatalf("%s body = %s, want to contain %q", operation, data, part)
		}
	}
}

func readErrorResponse(t *testing.T, operation string, resp *http.Response, wantStatus int) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s status = %d, body = %s, want %d", operation, resp.StatusCode, data, wantStatus)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("%s body = %s, decode error: %v", operation, data, err)
	}
	if body.Error == "" {
		t.Fatalf("%s body = %s, want non-empty error", operation, data)
	}
	return body.Error
}

func TestPlayValidatesPlayableAnimation(t *testing.T) {
	tests := []struct {
		name            string
		animation       string
		wantStatus      int
		wantErrorParts  []string
		wantQueuedDepth int
	}{
		{
			name:            "firmware preset",
			animation:       "matrix_rain_background",
			wantStatus:      http.StatusBadRequest,
			wantErrorParts:  []string{"matrix_rain_background", "not renderable/playable"},
			wantQueuedDepth: 0,
		},
		{
			name:            "unknown",
			animation:       "unknown_animation",
			wantStatus:      http.StatusBadRequest,
			wantErrorParts:  []string{"unknown_animation", "unknown animation"},
			wantQueuedDepth: 0,
		},
		{
			name:            "renderable",
			animation:       animations.NotificationAnimationID,
			wantStatus:      http.StatusAccepted,
			wantQueuedDepth: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpServer := newAnimationAPITestServer(t)
			body := bytes.NewBufferString(fmt.Sprintf(`{"animation":%q,"duration":"50ms","restore":"leave"}`, tt.animation))

			resp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST /play status = %d, body = %s, want %d", resp.StatusCode, data, tt.wantStatus)
			}
			for _, part := range tt.wantErrorParts {
				if !strings.Contains(string(data), part) {
					t.Fatalf("POST /play body = %s, want to contain %q", data, part)
				}
			}
			waitForQueueDepth(t, httpServer.URL, tt.wantQueuedDepth)
		})
	}
}

func TestNotifyAnimationOverrideValidatesPlayableAnimation(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantStatus     int
		wantErrorParts []string
	}{
		{
			name:       "omitted animation",
			body:       `{"title":"Test","message":"hello","duration":"50ms"}`,
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "renderable override",
			body:       `{"title":"Test","message":"hello","animation":"notification","duration":"50ms"}`,
			wantStatus: http.StatusAccepted,
		},
		{
			name:           "firmware preset override",
			body:           `{"title":"Test","message":"hello","animation":"matrix_rain_background","duration":"50ms"}`,
			wantStatus:     http.StatusBadRequest,
			wantErrorParts: []string{"matrix_rain_background", "not renderable/playable"},
		},
		{
			name:           "unknown override",
			body:           `{"title":"Test","message":"hello","animation":"unknown_animation","duration":"50ms"}`,
			wantStatus:     http.StatusBadRequest,
			wantErrorParts: []string{"unknown_animation", "unknown animation"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpServer := newAnimationAPITestServer(t)

			resp, err := http.Post(httpServer.URL+"/api/v1/notify", "application/json", bytes.NewBufferString(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST /notify status = %d, body = %s, want %d", resp.StatusCode, data, tt.wantStatus)
			}
			for _, part := range tt.wantErrorParts {
				if !strings.Contains(string(data), part) {
					t.Fatalf("POST /notify body = %s, want to contain %q", data, part)
				}
			}
		})
	}
}

func TestOverrideValidationErrorVocabularyMatchesAcrossHTTPIngresses(t *testing.T) {
	httpServer := newAnimationAPITestServer(t)

	tests := []struct {
		name      string
		requests  map[string]string
		wantParts []string
	}{
		{
			name: "unknown animation",
			requests: map[string]string{
				"/api/v1/events": `{"type":"notify","attributes":{"animation":"unknown_animation"}}`,
				"/api/v1/notify": `{"title":"Test","message":"hello","animation":"unknown_animation"}`,
				"/api/v1/play":   `{"animation":"unknown_animation","duration":"50ms","restore":"leave"}`,
			},
			wantParts: []string{"unknown animation", "unknown_animation"},
		},
		{
			name: "non-renderable animation",
			requests: map[string]string{
				"/api/v1/events": `{"type":"notify","attributes":{"animation":"matrix_rain_background"}}`,
				"/api/v1/notify": `{"title":"Test","message":"hello","animation":"matrix_rain_background"}`,
				"/api/v1/play":   `{"animation":"matrix_rain_background","duration":"50ms","restore":"leave"}`,
			},
			wantParts: []string{"matrix_rain_background", "not renderable/playable"},
		},
		{
			name: "invalid restore",
			requests: map[string]string{
				"/api/v1/events": `{"type":"notify","attributes":{"restore":"afterglow"}}`,
				"/api/v1/notify": `{"title":"Test","message":"hello","restore":"afterglow"}`,
				"/api/v1/play":   `{"animation":"notification","duration":"50ms","restore":"afterglow"}`,
			},
			wantParts: []string{"invalid restore policy"},
		},
		{
			name: "malformed duration",
			requests: map[string]string{
				"/api/v1/events": `{"type":"notify","attributes":{"duration":"not-a-duration"}}`,
				"/api/v1/notify": `{"title":"Test","message":"hello","duration":"not-a-duration"}`,
				"/api/v1/play":   `{"animation":"notification","duration":"not-a-duration","restore":"leave"}`,
			},
			wantParts: []string{"invalid duration"},
		},
		{
			name: "negative duration",
			requests: map[string]string{
				"/api/v1/events": `{"type":"notify","attributes":{"duration":"-1ms"}}`,
				"/api/v1/notify": `{"title":"Test","message":"hello","duration":"-1ms"}`,
				"/api/v1/play":   `{"animation":"notification","duration":"-1ms","restore":"leave"}`,
			},
			wantParts: []string{"duration cannot be negative"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotByPath := make(map[string]string, len(tt.requests))
			var baseline string
			for path, body := range tt.requests {
				resp, err := http.Post(httpServer.URL+path, "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				errorMessage := readErrorResponse(t, path, resp, http.StatusBadRequest)
				gotByPath[path] = errorMessage
				if baseline == "" {
					baseline = errorMessage
				}
				if errorMessage != baseline {
					t.Fatalf("%s error = %q, want same vocabulary as first ingress %q; all errors = %#v", path, errorMessage, baseline, gotByPath)
				}
				for _, part := range tt.wantParts {
					if !strings.Contains(errorMessage, part) {
						t.Fatalf("%s error = %q, want to contain %q", path, errorMessage, part)
					}
				}
			}
		})
	}
}

func TestEventsAnimationOverrideValidatesPlayableAnimationBeforePublish(t *testing.T) {
	tests := []struct {
		name           string
		animation      string
		wantStatus     int
		wantErrorParts []string
		wantPublished  bool
	}{
		{
			name:          "renderable override",
			animation:     animations.NotificationAnimationID,
			wantStatus:    http.StatusAccepted,
			wantPublished: true,
		},
		{
			name:           "firmware preset override",
			animation:      "matrix_rain_background",
			wantStatus:     http.StatusBadRequest,
			wantErrorParts: []string{"matrix_rain_background", "not renderable/playable"},
		},
		{
			name:           "unknown override",
			animation:      "unknown_animation",
			wantStatus:     http.StatusBadRequest,
			wantErrorParts: []string{"unknown_animation", "unknown animation"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := events.MustNewBus(4)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			ch, unsubscribe := bus.Subscribe(ctx)
			t.Cleanup(unsubscribe)

			registry := registryWithFirmwarePreset(t, "matrix_rain_background", animations.FirmwarePreset{
				EffectID: 12,
				Interval: 90 * time.Millisecond,
				Color:    animations.RGB{G: 255, B: 85},
			})
			api, err := httpapi.New(httpapi.Options{
				Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				Bus:        bus,
				Registry:   registry,
				ServerAddr: "127.0.0.1:0",
			})
			if err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewServer(api.Router())
			t.Cleanup(httpServer.Close)

			body := bytes.NewBufferString(fmt.Sprintf(
				`{"type":"notify","attributes":{"animation":%q}}`,
				tt.animation,
			))
			resp, err := http.Post(httpServer.URL+"/events", "application/json", body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST /events status = %d, body = %s, want %d", resp.StatusCode, data, tt.wantStatus)
			}
			for _, part := range tt.wantErrorParts {
				if !strings.Contains(string(data), part) {
					t.Fatalf("POST /events body = %s, want to contain %q", data, part)
				}
			}

			if tt.wantPublished {
				select {
				case event := <-ch:
					if got := event.Attributes["animation"]; got != tt.animation {
						t.Fatalf("published attributes.animation = %q, want %q", got, tt.animation)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for accepted event to publish")
				}
				return
			}

			select {
			case event := <-ch:
				t.Fatalf("invalid animation override was published to async event path: %#v", event)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestEventsOverrideValidationRejectsInvalidRestoreAndDurationBeforePublish(t *testing.T) {
	tests := []struct {
		name           string
		attributes     string
		wantErrorParts []string
	}{
		{
			name:           "invalid restore",
			attributes:     `"restore":"afterglow"`,
			wantErrorParts: []string{"invalid restore policy"},
		},
		{
			name:           "malformed duration",
			attributes:     `"duration":"not-a-duration"`,
			wantErrorParts: []string{"invalid duration"},
		},
		{
			name:           "out of bounds duration",
			attributes:     `"duration":"-1ms"`,
			wantErrorParts: []string{"duration cannot be negative"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := events.MustNewBus(4)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			ch, unsubscribe := bus.Subscribe(ctx)
			t.Cleanup(unsubscribe)

			registry, err := animations.NewDefaultRegistry()
			if err != nil {
				t.Fatal(err)
			}
			api, err := httpapi.New(httpapi.Options{
				Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				Bus:        bus,
				Registry:   registry,
				ServerAddr: "127.0.0.1:0",
			})
			if err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewServer(api.Router())
			t.Cleanup(httpServer.Close)

			body := bytes.NewBufferString(fmt.Sprintf(
				`{"type":"notify","attributes":{%s}}`,
				tt.attributes,
			))
			resp, err := http.Post(httpServer.URL+"/events", "application/json", body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("POST /events status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusBadRequest)
			}
			for _, part := range tt.wantErrorParts {
				if !strings.Contains(string(data), part) {
					t.Fatalf("POST /events body = %s, want to contain %q", data, part)
				}
			}

			select {
			case event := <-ch:
				t.Fatalf("invalid override event was published to async event path: %#v", event)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestEventsOverrideValidationAllowsCustomAttributesBeforePublish(t *testing.T) {
	bus := events.MustNewBus(4)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, unsubscribe := bus.Subscribe(ctx)
	t.Cleanup(unsubscribe)

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(httpapi.Options{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Bus:        bus,
		Registry:   registry,
		ServerAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(api.Router())
	t.Cleanup(httpServer.Close)

	body := bytes.NewBufferString(`{"type":"notify","attributes":{"restore":"leave","duration":"250ms","custom":"kept","param.color":"green","param.speed":"fast"}}`)
	resp, err := http.Post(httpServer.URL+"/events", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /events status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusAccepted)
	}

	select {
	case event := <-ch:
		want := map[string]string{
			"restore":     "leave",
			"duration":    "250ms",
			"custom":      "kept",
			"param.color": "green",
			"param.speed": "fast",
		}
		if len(event.Attributes) != len(want) {
			t.Fatalf("published attributes = %#v, want %#v", event.Attributes, want)
		}
		for key, value := range want {
			if got := event.Attributes[key]; got != value {
				t.Fatalf("published attributes[%q] = %q, want %q; all attributes = %#v", key, got, value, event.Attributes)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for accepted event to publish")
	}
}

func newAnimationAPITestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := newAdminAuthTestConfig(t)
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, "matrix_rain_background", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    animations.RGB{G: 255, B: 85},
	})

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownApp(t, application)
	})

	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func newConfigAuthoredFrameAnimationAPITestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	animationsPath := dir + "/animations.yaml"
	rulesPath := dir + "/rules.yaml"
	configPath := dir + "/config.yaml"

	if err := os.WriteFile(animationsPath, []byte(`
animations:
  pixel_badge:
    type: frames
    palette:
      ".": "#000000"
      R: "#FF0000"
      G: "#00FF00"
    frames:
      - delay: 50ms
        rows:
          - "R......."
          - ".G......"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
  matrix_rain_background:
    type: firmware_preset
    effect_id: 12
    interval: 90ms
    color: "#00FF55"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath, []byte(`
rules:
  - id: frame_notify
    when:
      source: http
      type: notify
    play:
      animation: pixel_badge
      priority: 50
      duration: 2s
      interrupt: none
      restore: background
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
animations_file: %q
rules_file: %q
server:
  addr: "127.0.0.1:0"
  admin_token_env: ""
matrix:
  host: "127.0.0.1"
  port: 1
  connect_timeout: 1s
  response_timeout: 1s
  reconnect_min_delay: 10ms
  reconnect_max_delay: 50ms
queue:
  events_buffer: 16
  play_buffer: 16
  overflow_policy: block
  dedup_window: 0s
`, animationsPath, rulesPath)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownApp(t, application)
	})

	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func TestMatrixFillWaitsForCurrentAnimationThroughScheduler(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	host, portText, err := net.SplitHostPort(matrixServer.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Matrix.Host = host
	cfg.Matrix.Port = port
	cfg.Matrix.ConnectTimeout = time.Second
	cfg.Matrix.ResponseTimeout = time.Second
	cfg.Matrix.ReconnectMinDelay = 10 * time.Millisecond
	cfg.Matrix.ReconnectMaxDelay = 50 * time.Millisecond
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = writeRulesFile(t)

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	t.Cleanup(func() {
		shutdownAppWorkers(t, application, done)
	})

	time.Sleep(20 * time.Millisecond)
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	pausedFrameResponse := matrixServer.PauseNextFrameResponse()
	notifyBody := bytes.NewBufferString(`{"title":"Test","message":"hello","duration":"750ms","restore":"leave"}`)
	resp, err := http.Post(httpServer.URL+"/api/v1/notify", "application/json", notifyBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /notify status = %d, body = %s", resp.StatusCode, data)
	}

	var commands []recordedFrame
	var sawFirstFrame bool
	deadline := time.After(3 * time.Second)
	for !sawFirstFrame {
		select {
		case frame := <-matrixServer.frames:
			commands = append(commands, frame)
			if frame.Command == testCommandSetFrame {
				sawFirstFrame = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for first notification frame")
		}
	}
	select {
	case <-pausedFrameResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first frame response to pause")
	}

	fillDone := make(chan error, 1)
	go func() {
		fillBody := bytes.NewBufferString(`{"r":1,"g":2,"b":3}`)
		resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", fillBody)
		if err != nil {
			fillDone <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			fillDone <- errors.New("POST /matrix/fill status = " + strconv.Itoa(resp.StatusCode) + ", body = " + string(data))
			return
		}
		fillDone <- nil
	}()
	time.Sleep(20 * time.Millisecond)
	matrixServer.ResumePausedFrameResponse()

	framesBeforeFill := 0
	sawFill := false
	deadline = time.After(3 * time.Second)
	for !sawFill {
		select {
		case frame := <-matrixServer.frames:
			commands = append(commands, frame)
			switch frame.Command {
			case testCommandSetFrame:
				framesBeforeFill++
			case testCommandFill:
				sawFill = true
				if !bytes.Equal(frame.Payload, []byte{1, 2, 3}) {
					t.Fatalf("fill payload = %v, want [1 2 3]", frame.Payload)
				}
			case testCommandPing:
			default:
				t.Fatalf("unexpected matrix command before fill 0x%02x", frame.Command)
			}
		case <-deadline:
			t.Fatal("timed out waiting for scheduled fill command")
		}
	}

	if framesBeforeFill != 2 {
		t.Fatalf("frames after fill was requested and before fill command = %d, want 2", framesBeforeFill)
	}

	select {
	case err := <-fillDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("POST /matrix/fill did not return after scheduled execution")
	}

	waitForResponses(t, matrixServer, len(commands))
	if matrixServer.Pipelined() {
		t.Fatal("client sent another matrix command before receiving a response")
	}
}

func TestMatrixControlsConvergeToConfiguredBackgroundAfterHTTPControl(t *testing.T) {
	backgroundPayload := []byte{12, 90, 0, 0, 255, 85}

	tests := []struct {
		name        string
		path        string
		body        string
		command     byte
		wantPayload []byte
	}{
		{
			name:        "fill",
			path:        "/api/v1/matrix/fill",
			body:        `{"r":4,"g":5,"b":6}`,
			command:     testCommandFill,
			wantPayload: []byte{4, 5, 6},
		},
		{
			name:        "clear",
			path:        "/api/v1/matrix/clear",
			body:        `{}`,
			command:     testCommandClear,
			wantPayload: []byte{},
		},
		{
			name:        "preset",
			path:        "/api/v1/matrix/preset",
			body:        `{"effect_id":7,"interval":"25ms","color":{"r":1,"g":2,"b":3}}`,
			command:     testCommandSetPreset,
			wantPayload: []byte{7, 25, 0, 1, 2, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matrixServer := newFakeESPServer(t)
			defer matrixServer.Close()

			cfg := newHTTPMatrixBackgroundTestConfig(t, matrixServer.Addr())
			application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}

			done := runAppWorkers(t, application)
			defer func() {
				matrixServer.ResumePausedCommandResponse()
				shutdownAppWorkers(t, application, done)
			}()

			httpServer := httptest.NewServer(application.Handler())
			defer httpServer.Close()
			waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
			waitForMatrixCommandMatching(t, matrixServer, "initial configured background preset", func(frame recordedFrame) bool {
				return frame.Command == testCommandSetPreset && bytes.Equal(frame.Payload, backgroundPayload)
			})

			pausedResponse := matrixServer.PauseNextCommandResponse(tt.command)
			controlDone := make(chan httpControlResult, 1)
			go func() {
				resp, err := http.Post(httpServer.URL+tt.path, "application/json", bytes.NewBufferString(tt.body))
				if err != nil {
					controlDone <- httpControlResult{err: err}
					return
				}
				defer resp.Body.Close()
				data, readErr := io.ReadAll(resp.Body)
				if readErr != nil {
					controlDone <- httpControlResult{err: readErr}
					return
				}
				controlDone <- httpControlResult{status: resp.StatusCode, body: string(data)}
			}()

			waitForMatrixCommandMatching(t, matrixServer, "requested "+tt.name+" control", func(frame recordedFrame) bool {
				return frame.Command == tt.command && bytes.Equal(frame.Payload, tt.wantPayload)
			})
			select {
			case <-pausedResponse:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for fake ESP to pause the requested control response")
			}
			select {
			case result := <-controlDone:
				t.Fatalf("POST %s returned before fake ESP acknowledged requested control: status=%d body=%q err=%v", tt.path, result.status, result.body, result.err)
			default:
			}

			matrixServer.ResumePausedCommandResponse()
			result := waitForHTTPControlResult(t, controlDone, tt.path)
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.status != http.StatusOK {
				t.Fatalf("POST %s status = %d, body = %s, want %d", tt.path, result.status, result.body, http.StatusOK)
			}

			waitForMatrixCommandMatching(t, matrixServer, "post-control configured background preset", func(frame recordedFrame) bool {
				return frame.Command == testCommandSetPreset && bytes.Equal(frame.Payload, backgroundPayload)
			})
			if matrixServer.Pipelined() {
				t.Fatal("client sent another matrix command before receiving a response")
			}
		})
	}
}

func TestMatrixPresetDurationBoundReturnsBadRequest(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		matrixServer.ResumePausedFrameResponse()
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pausedFrameResponse := matrixServer.PauseNextFrameResponse()
	resp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", bytes.NewBufferString(`{"animation":"notification","duration":"750ms","restore":"leave"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /play status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusAccepted)
	}
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	select {
	case <-pausedFrameResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first frame response to pause")
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	start := time.Now()
	resp, err = client.Post(httpServer.URL+"/api/v1/matrix/preset", "application/json", bytes.NewBufferString(`{"effect_id":1,"interval":"70s"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /matrix/preset status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusBadRequest)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("POST /matrix/preset returned after %s, want prompt validation failure", elapsed)
	}
	if got := matrixServer.CommandCount(testCommandSetPreset); got != 0 {
		t.Fatalf("preset command count = %d, want 0", got)
	}
}

func TestMatrixControlFirmwareStatusReturnsBadGateway(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()
	matrixServer.FailNextStatus(testCommandFill, testStatusUnknownCommand)

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":9,"g":8,"b":7}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /matrix/fill status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusBadGateway)
	}
	if got := matrixServer.CommandCount(testCommandFill); got != 1 {
		t.Fatalf("fill command count = %d, want 1", got)
	}
}

func TestMatrixControlRetryableSocketFailureEventuallySucceeds(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()
	matrixServer.DropNextResponse(testCommandFill)

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":1,"g":2,"b":3}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /matrix/fill status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}
	if got := matrixServer.CommandCount(testCommandFill); got != 2 {
		t.Fatalf("fill command count = %d, want 2", got)
	}
}

func TestQueueClearUnblocksWaitingMatrixControl(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		matrixServer.ResumePausedFrameResponse()
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pausedFrameResponse := matrixServer.PauseNextFrameResponse()
	resp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", bytes.NewBufferString(`{"animation":"notification","duration":"750ms","restore":"leave"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /play status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusAccepted)
	}
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	select {
	case <-pausedFrameResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first frame response to pause")
	}

	fillStatus := make(chan int, 1)
	go func() {
		resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":1,"g":2,"b":3}`))
		if err != nil {
			fillStatus <- 0
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		fillStatus <- resp.StatusCode
	}()
	waitForQueueDepth(t, httpServer.URL, 1)

	cleared := deleteQueue(t, httpServer.URL)
	if cleared != 1 {
		t.Fatalf("DELETE /queue cleared = %d, want 1", cleared)
	}

	select {
	case status := <-fillStatus:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("POST /matrix/fill status = %d, want %d", status, http.StatusServiceUnavailable)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("POST /matrix/fill did not return promptly after queue clear")
	}
}

func TestQueueInspectionAndClearCoverMixedSchedulerOwnedQueue(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application)
	defer func() {
		matrixServer.ResumePausedFrameResponse()
		shutdownAppWorkers(t, application, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pausedFrameResponse := matrixServer.PauseNextFrameResponse()
	resp, err := http.Post(httpServer.URL+"/api/v1/play", "application/json", bytes.NewBufferString(`{"animation":"notification","duration":"750ms","restore":"leave"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /play status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusAccepted)
	}
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	select {
	case <-pausedFrameResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first frame response to pause")
	}

	fillStatus := make(chan int, 1)
	go func() {
		resp, err := http.Post(httpServer.URL+"/api/v1/matrix/fill", "application/json", bytes.NewBufferString(`{"r":1,"g":2,"b":3}`))
		if err != nil {
			fillStatus <- 0
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		fillStatus <- resp.StatusCode
	}()
	waitForQueueDepth(t, httpServer.URL, 1)

	resp, err = http.Post(httpServer.URL+"/api/v1/play", "application/json", bytes.NewBufferString(`{"animation":"notification","duration":"500ms","restore":"leave","priority":7}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /play status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusAccepted)
	}
	queue := waitForQueueSnapshot(t, httpServer.URL, 2)
	if queue.State == "" {
		t.Fatal("GET /queue state is empty")
	}
	if len(queue.Items) != 2 {
		t.Fatalf("GET /queue items length = %d, want 2", len(queue.Items))
	}
	if len(queue.RawItems) != 2 {
		t.Fatalf("GET /queue raw items length = %d, want 2", len(queue.RawItems))
	}

	control := queue.Items[0]
	if control.Kind != "control" {
		t.Fatalf("GET /queue first item kind = %q, want control", control.Kind)
	}
	if control.ID == "" {
		t.Fatal("GET /queue first item id is empty")
	}
	if control.Priority != 0 {
		t.Fatalf("GET /queue first item priority = %d, want 0", control.Priority)
	}
	if control.CreatedAt.IsZero() {
		t.Fatal("GET /queue first item created_at is zero")
	}
	if control.Control == nil {
		t.Fatalf("GET /queue first item = %+v, want control metadata", control)
	}
	if control.Control.ID != control.ID {
		t.Fatalf("GET /queue control id = %q, want top-level id %q", control.Control.ID, control.ID)
	}
	if control.Control.Kind != "fill" {
		t.Fatalf("GET /queue control kind = %q, want fill", control.Control.Kind)
	}
	if control.Control.Priority != 0 {
		t.Fatalf("GET /queue control priority = %d, want 0", control.Control.Priority)
	}
	if control.Control.Color != (queueColor{R: 1, G: 2, B: 3}) {
		t.Fatalf("GET /queue control color = %+v, want {1 2 3}", control.Control.Color)
	}
	if control.Control.CreatedAt.IsZero() {
		t.Fatal("GET /queue control created_at is zero")
	}
	assertNoJSONFields(t, queue.RawItems[0], "frames", "loop", "on_start", "on_finish")
	rawControl := rawObjectField(t, queue.RawItems[0], "control")
	assertNoJSONFields(t, rawControl, "ctx", "done", "err", "completed")

	animation := queue.Items[1]
	if animation.Kind != "animation" {
		t.Fatalf("GET /queue second item kind = %q, want animation", animation.Kind)
	}
	if animation.Control != nil {
		t.Fatalf("GET /queue second item control = %+v, want nil", animation.Control)
	}
	if animation.AnimationID != "notification" {
		t.Fatalf("GET /queue second item animation_id = %q, want notification", animation.AnimationID)
	}
	if animation.Priority != 7 {
		t.Fatalf("GET /queue second item priority = %d, want 7", animation.Priority)
	}
	if animation.RestorePolicy != "leave" {
		t.Fatalf("GET /queue second item restore_policy = %q, want leave", animation.RestorePolicy)
	}
	if animation.CreatedAt.IsZero() {
		t.Fatal("GET /queue second item created_at is zero")
	}
	assertNoJSONFields(t, queue.RawItems[1], "frames", "loop", "on_start", "on_finish", "control")

	if cleared := deleteQueue(t, httpServer.URL); cleared != 2 {
		t.Fatalf("DELETE /queue cleared = %d, want 2 mixed queued items", cleared)
	}

	select {
	case status := <-fillStatus:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("POST /matrix/fill status = %d, want %d", status, http.StatusServiceUnavailable)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("POST /matrix/fill did not return promptly after mixed queue clear")
	}
}

func newAdminAuthTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Matrix.Host = "127.0.0.1"
	cfg.Matrix.Port = 1
	cfg.Matrix.ConnectTimeout = time.Second
	cfg.Matrix.ResponseTimeout = time.Second
	cfg.Matrix.ReconnectMinDelay = 10 * time.Millisecond
	cfg.Matrix.ReconnectMaxDelay = 50 * time.Millisecond
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = writeRulesFile(t)
	return cfg
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
	return newHTTPMatrixTestConfigForHostPort(t, host, port)
}

func newHTTPMatrixBackgroundTestConfig(t *testing.T, matrixAddr string) config.Config {
	t.Helper()
	cfg := newHTTPMatrixTestConfig(t, matrixAddr)
	cfg.Background.Animation = "matrix_rain_background"
	cfg.Background.RestoreOnIdle = true
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, "matrix_rain_background", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    animations.RGB{G: 255, B: 85},
	})
	return cfg
}

func newHTTPMatrixTestConfigForHostPort(t *testing.T, host string, port int) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Matrix.Host = host
	cfg.Matrix.Port = port
	cfg.Matrix.ConnectTimeout = 20 * time.Millisecond
	cfg.Matrix.ResponseTimeout = time.Second
	cfg.Matrix.HeartbeatInterval = 10 * time.Millisecond
	cfg.Matrix.ProbeTimeout = 50 * time.Millisecond
	cfg.Matrix.ReconnectMinDelay = 10 * time.Millisecond
	cfg.Matrix.ReconnectMaxDelay = 50 * time.Millisecond
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = writeRulesFile(t)
	return cfg
}

func unusedLocalTCPAddr(t *testing.T) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func runAppWorkers(t *testing.T, application *app.App) <-chan error {
	t.Helper()
	return runAppWorkersWithContext(t, application, context.Background())
}

func runAppWorkersWithContext(t *testing.T, application *app.App, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- application.RunWorkers(ctx)
	}()
	return done
}

func shutdownAppWorkers(t *testing.T, application *app.App, done <-chan error) {
	t.Helper()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	shutdownErr := application.Shutdown(shutdownCtx)
	select {
	case runErr := <-done:
		if shutdownErr != nil {
			t.Fatalf("Shutdown() error = %v", shutdownErr)
		}
		if runErr != nil {
			t.Fatalf("RunWorkers() error = %v", runErr)
		}
	case <-time.After(time.Second):
		if shutdownErr != nil {
			t.Fatalf("Shutdown() error = %v; RunWorkers() did not stop", shutdownErr)
		}
		t.Fatal("RunWorkers() did not stop")
	}
}

func shutdownAppWorkersExpectingError(t *testing.T, application *app.App, done <-chan error, want error) {
	t.Helper()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	shutdownErr := application.Shutdown(shutdownCtx)
	select {
	case runErr := <-done:
		if shutdownErr != nil {
			t.Fatalf("Shutdown() error = %v", shutdownErr)
		}
		if !errors.Is(runErr, want) {
			t.Fatalf("RunWorkers() error = %v, want %v", runErr, want)
		}
	case <-time.After(time.Second):
		if shutdownErr != nil {
			t.Fatalf("Shutdown() error = %v; RunWorkers() did not stop", shutdownErr)
		}
		t.Fatal("RunWorkers() did not stop")
	}
}

func shutdownApp(t *testing.T, application *app.App) {
	t.Helper()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := application.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
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

func assertStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s status = %d, body = %s, want %d", url, resp.StatusCode, data, want)
	}
}

func waitForStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastStatus int
	var lastBody []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		lastStatus = resp.StatusCode
		lastBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if lastStatus == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s status = %d, body = %s, want %d", url, lastStatus, lastBody, want)
}

func waitForMetricLine(t *testing.T, baseURL, metric string, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = getMetrics(t, baseURL)
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

func assertNoMetricLine(t *testing.T, baseURL, metric string, parts ...string) {
	t.Helper()
	body := getMetrics(t, baseURL)
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
			t.Fatalf("metrics unexpectedly contained %s with %v in line:\n%s", metric, parts, line)
		}
	}
}

func assertMetricLabelKeys(t *testing.T, baseURL, metric string, want []string, parts ...string) {
	t.Helper()
	body := getMetrics(t, baseURL)
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
		if !matched {
			continue
		}
		got := metricLabelKeys(t, metric, line)
		want = append([]string(nil), want...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("metric %s label keys = %v, want %v in line:\n%s", metric, got, want, line)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("metric %s label keys = %v, want %v in line:\n%s", metric, got, want, line)
			}
		}
		return
	}
	t.Fatalf("metrics missing %s with %v in:\n%s", metric, parts, body)
}

func metricLabelKeys(t *testing.T, metric, line string) []string {
	t.Helper()
	if strings.HasPrefix(line, metric+" ") {
		return nil
	}
	prefix := metric + "{"
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("metric line for %s has unexpected format:\n%s", metric, line)
	}
	end := strings.Index(line[len(prefix):], "}")
	if end < 0 {
		t.Fatalf("metric line for %s is missing closing labels brace:\n%s", metric, line)
	}
	labels := line[len(prefix) : len(prefix)+end]
	if labels == "" {
		return nil
	}
	pairs := strings.Split(labels, ",")
	keys := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		idx := strings.Index(pair, "=")
		if idx <= 0 {
			t.Fatalf("metric line for %s has malformed label %q in:\n%s", metric, pair, line)
		}
		keys = append(keys, pair[:idx])
	}
	sort.Strings(keys)
	return keys
}

func getMetrics(t *testing.T, baseURL string) string {
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

func waitForReadyDetails(t *testing.T, url string, wantStatus int) readyDetails {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastStatus int
	var last readyDetails
	var lastBody []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		lastStatus = resp.StatusCode
		lastBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err := json.Unmarshal(lastBody, &last); err != nil {
			t.Fatalf("decode readyz body %q: %v", lastBody, err)
		}
		if lastStatus == wantStatus {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s status = %d, body = %s, want %d", url, lastStatus, lastBody, wantStatus)
	return readyDetails{}
}

func waitForQueueDepth(t *testing.T, baseURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last queueResponse
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/v1/queue")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		last = decodeQueueResponse(t, data)
		if last.Depth == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue depth = %d, want %d", last.Depth, want)
}

func waitForQueueSnapshot(t *testing.T, baseURL string, wantDepth int) queueResponse {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last queueResponse
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/v1/queue")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		last = decodeQueueResponse(t, data)
		if last.Depth == wantDepth && len(last.Items) == wantDepth {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue depth/items = %d/%d, want %d/%d", last.Depth, len(last.Items), wantDepth, wantDepth)
	return queueResponse{}
}

func deleteQueue(t *testing.T, baseURL string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/queue", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body queueClearResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /queue status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return body.Cleared
}

func decodeQueueResponse(t *testing.T, data []byte) queueResponse {
	t.Helper()

	var typed queueResponse
	if err := json.Unmarshal(data, &typed); err != nil {
		t.Fatalf("decode queue response %q: %v", data, err)
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode raw queue response %q: %v", data, err)
	}
	typed.RawItems = raw.Items
	return typed
}

func rawObjectField(t *testing.T, raw map[string]json.RawMessage, field string) map[string]json.RawMessage {
	t.Helper()
	value, ok := raw[field]
	if !ok {
		t.Fatalf("JSON object missing field %q", field)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		t.Fatalf("decode JSON field %q as object: %v", field, err)
	}
	return object
}

func assertNoJSONFields(t *testing.T, raw map[string]json.RawMessage, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if _, ok := raw[field]; ok {
			t.Fatalf("JSON field %q unexpectedly present in %v", field, raw)
		}
	}
}

func waitForMatrixCommand(t *testing.T, server *fakeESPServer, command byte) recordedFrame {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame := <-server.frames:
			if frame.Command == command {
				return frame
			}
		case <-deadline:
			t.Fatalf("timed out waiting for matrix command 0x%02x", command)
		}
	}
}

func waitForMatrixCommandMatching(t *testing.T, server *fakeESPServer, description string, match func(recordedFrame) bool) recordedFrame {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame := <-server.frames:
			if match(frame) {
				return frame
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

type httpControlResult struct {
	status int
	body   string
	err    error
}

func waitForHTTPControlResult(t *testing.T, done <-chan httpControlResult, path string) httpControlResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(time.Second):
		t.Fatalf("POST %s did not return after fake ESP acknowledged requested control", path)
	}
	return httpControlResult{}
}

type queueResponse struct {
	Depth    int                          `json:"depth"`
	State    string                       `json:"state"`
	Items    []queueItem                  `json:"items"`
	RawItems []map[string]json.RawMessage `json:"-"`
}

type queueItem struct {
	Kind          string        `json:"kind"`
	ID            string        `json:"id"`
	EventID       string        `json:"event_id"`
	AnimationID   string        `json:"animation_id"`
	Priority      int           `json:"priority"`
	RestorePolicy string        `json:"restore_policy"`
	CreatedAt     time.Time     `json:"created_at"`
	Deadline      time.Time     `json:"deadline"`
	Control       *queueControl `json:"control"`
}

type queueControl struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	Priority   int           `json:"priority"`
	Brightness byte          `json:"brightness"`
	EffectID   byte          `json:"effect_id"`
	Interval   time.Duration `json:"interval"`
	Color      queueColor    `json:"color"`
	CreatedAt  time.Time     `json:"created_at"`
	Deadline   time.Time     `json:"deadline"`
}

type queueColor struct {
	R byte `json:"r"`
	G byte `json:"g"`
	B byte `json:"b"`
}

type readyDetails struct {
	Status          string           `json:"status"`
	WorkersRunning  bool             `json:"workers_running"`
	Draining        bool             `json:"draining"`
	SchedulerState  string           `json:"scheduler_state"`
	MatrixConnected bool             `json:"matrix_connected"`
	EventWorker     readyEventWorker `json:"event_worker"`
	LastSuccess     *time.Time       `json:"last_success"`
	LastFailure     *time.Time       `json:"last_failure"`
}

type readyEventWorker struct {
	State                 string   `json:"state"`
	Stage                 string   `json:"stage"`
	ActiveDurationSeconds *float64 `json:"active_duration_seconds"`
}

type queueClearResponse struct {
	Cleared int `json:"cleared"`
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	old, ok := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, old)
			return
		}
		_ = os.Unsetenv(name)
	})
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

type fakeESPServer struct {
	listener net.Listener
	frames   chan recordedFrame

	mu             sync.Mutex
	closed         bool
	conns          map[net.Conn]struct{}
	responseWrites int
	pipelined      bool
	commandCounts  map[byte]int
	statuses       map[byte][]byte
	drops          map[byte]int
	dropCounts     map[byte]map[int]struct{}
	corrupts       map[byte]int

	pauseResponseCommand byte
	pauseResponse        chan struct{}
	pausedResponse       chan struct{}
}

type recordedFrame struct {
	Command byte
	Payload []byte
}

func newFakeESPServer(t *testing.T) *fakeESPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeESPServer{
		listener:      listener,
		frames:        make(chan recordedFrame, 64),
		conns:         make(map[net.Conn]struct{}),
		commandCounts: make(map[byte]int),
		statuses:      make(map[byte][]byte),
		drops:         make(map[byte]int),
		dropCounts:    make(map[byte]map[int]struct{}),
		corrupts:      make(map[byte]int),
	}
	go server.serve()
	return server
}

func (s *fakeESPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *fakeESPServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	_ = s.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *fakeESPServer) CloseConnections() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *fakeESPServer) ResponseWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.responseWrites
}

func (s *fakeESPServer) Pipelined() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pipelined
}

func (s *fakeESPServer) CommandCount(command byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commandCounts[command]
}

func (s *fakeESPServer) FailNextStatus(command, status byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[command] = append(s.statuses[command], status)
}

func (s *fakeESPServer) DropNextResponse(command byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops[command]++
}

func (s *fakeESPServer) DropResponseOnCommandCount(command byte, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropCounts[command] == nil {
		s.dropCounts[command] = make(map[int]struct{})
	}
	s.dropCounts[command][count] = struct{}{}
}

func (s *fakeESPServer) CorruptNextResponse(command byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corrupts[command]++
}

func (s *fakeESPServer) PauseNextFrameResponse() <-chan struct{} {
	return s.PauseNextCommandResponse(testCommandSetFrame)
}

func (s *fakeESPServer) PauseNextCommandResponse(command byte) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pauseResponseCommand = command
	s.pauseResponse = make(chan struct{})
	s.pausedResponse = make(chan struct{})
	return s.pausedResponse
}

func (s *fakeESPServer) ResumePausedFrameResponse() {
	s.ResumePausedCommandResponse()
}

func (s *fakeESPServer) ResumePausedCommandResponse() {
	s.mu.Lock()
	ch := s.pauseResponse
	if ch != nil {
		s.pauseResponse = nil
		s.pausedResponse = nil
	}
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (s *fakeESPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeESPServer) handle(conn net.Conn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	reader := bufio.NewReader(conn)
	for {
		frame, err := readMatrixFrame(reader, conn)
		if err != nil {
			return
		}
		s.recordCommand(frame.Command)
		s.frames <- frame

		conn.SetReadDeadline(time.Now().Add(2 * time.Millisecond))
		if _, err := reader.Peek(1); err == nil {
			s.mu.Lock()
			s.pipelined = true
			s.mu.Unlock()
		}
		conn.SetReadDeadline(time.Time{})
		if pause := s.pauseBeforeResponse(frame); pause != nil {
			<-pause
			s.clearPausedResponse(pause)
		}

		if s.shouldDropResponse(frame.Command) {
			return
		}

		response := []byte{testMagic0, testMagic1, testProtocolVersion, testResponseCommand, s.nextStatus(frame.Command)}
		response = append(response, xorChecksum(response))
		if s.shouldCorruptResponse(frame.Command) {
			response[len(response)-1] ^= 0xff
		}
		if _, err := conn.Write(response); err != nil {
			return
		}
		s.mu.Lock()
		s.responseWrites++
		s.mu.Unlock()
	}
}

func (s *fakeESPServer) recordCommand(command byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commandCounts[command]++
}

func (s *fakeESPServer) nextStatus(command byte) byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := s.statuses[command]
	if len(statuses) == 0 {
		return testStatusOK
	}
	status := statuses[0]
	s.statuses[command] = statuses[1:]
	return status
}

func (s *fakeESPServer) shouldDropResponse(command byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if counts := s.dropCounts[command]; counts != nil {
		if _, ok := counts[s.commandCounts[command]]; ok {
			delete(counts, s.commandCounts[command])
			return true
		}
	}
	if s.drops[command] <= 0 {
		return false
	}
	s.drops[command]--
	return true
}

func (s *fakeESPServer) shouldCorruptResponse(command byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.corrupts[command] <= 0 {
		return false
	}
	s.corrupts[command]--
	return true
}

func (s *fakeESPServer) pauseBeforeResponse(frame recordedFrame) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame.Command != s.pauseResponseCommand || s.pauseResponse == nil {
		return nil
	}
	if s.pausedResponse != nil {
		close(s.pausedResponse)
		s.pausedResponse = nil
	}
	return s.pauseResponse
}

func (s *fakeESPServer) clearPausedResponse(pause <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pauseResponse == pause {
		s.pauseResponse = nil
	}
}

func readMatrixFrame(reader *bufio.Reader, conn net.Conn) (recordedFrame, error) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return recordedFrame{}, err
	}
	if header[0] != testMagic0 || header[1] != testMagic1 || header[2] != testProtocolVersion {
		return recordedFrame{}, errors.New("invalid matrix command header")
	}

	payload := make([]byte, int(header[4]))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return recordedFrame{}, err
	}
	checksum := []byte{0}
	if _, err := io.ReadFull(reader, checksum); err != nil {
		return recordedFrame{}, err
	}

	raw := append(append([]byte{}, header...), payload...)
	if checksum[0] != xorChecksum(raw) {
		return recordedFrame{}, errors.New("invalid matrix command checksum")
	}
	return recordedFrame{Command: header[3], Payload: payload}, nil
}

func xorChecksum(data []byte) byte {
	var checksum byte
	for _, b := range data {
		checksum ^= b
	}
	return checksum
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func waitForResponses(t *testing.T, server *fakeESPServer, commandCount int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if server.ResponseWrites() >= commandCount {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("response writes = %d, commands = %d", server.ResponseWrites(), commandCount)
}
