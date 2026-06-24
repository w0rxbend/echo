package app_test

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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/app"
	"github.com/worxbend/echo/internal/config"
)

const (
	testCommandPing      byte = 0x00
	testCommandFill      byte = 0x03
	testCommandSetFrame  byte = 0x05
	testCommandSetPreset byte = 0x08
	testFramePayloadSize      = 192

	testMagic0          byte = 0x4C
	testMagic1          byte = 0x4D
	testProtocolVersion byte = 0x01
	testResponseCommand byte = 0x80

	testStatusOK             byte = 0x00
	testStatusUnknownCommand byte = 0x03
)

func TestMetricsReportQueueClearedControlAndAnimationByItemLabel(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		matrixServer.ResumePausedFrameResponse()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pausedFrameResponse := matrixServer.PauseNextFrameResponse()
	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"750ms","restore":"leave"}`, http.StatusAccepted)
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	select {
	case <-pausedFrameResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first frame response to pause")
	}

	fillStatus := make(chan int, 1)
	go func() {
		fillStatus <- postJSONStatus(httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`)
	}()
	waitForQueueDepth(t, httpServer.URL, 1)
	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"500ms","restore":"leave"}`, http.StatusAccepted)
	waitForQueueDepth(t, httpServer.URL, 2)

	if cleared := deleteQueue(t, httpServer.URL); cleared != 2 {
		t.Fatalf("DELETE /queue cleared = %d, want 2", cleared)
	}
	select {
	case status := <-fillStatus:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("POST /matrix/fill status = %d, want %d", status, http.StatusServiceUnavailable)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("POST /matrix/fill did not return promptly after queue clear")
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="control"`, `item="fill"`, `outcome="queue_cleared"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="animation"`, `item="notification"`, `outcome="queue_cleared"`, " 1")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
}

func TestMetricsReportExecutedAndPermanentErrorOutcomes(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":4,"g":5,"b":6}`, http.StatusOK)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="control"`, `item="fill"`, `outcome="executed"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="ok"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_command_duration_seconds_count",
		`command="fill"`)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")

	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"1ms","restore":"leave"}`, http.StatusAccepted)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="animation"`, `item="notification"`, `outcome="executed"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_animation_render_duration_seconds_count",
		`animation="notification"`)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")

	matrixServer.FailNextStatus(testCommandFill, testStatusUnknownCommand)
	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":7,"g":8,"b":9}`, http.StatusBadGateway)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="control"`, `item="fill"`, `outcome="permanent_error"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="unknown_command"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_item_outcomes_dropped_total", " 0")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
}

func TestAppAppliesMatrixRainBackgroundAtStartupThroughTCP(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := loadMatrixRainBackgroundTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	preset := waitForMatrixCommand(t, matrixServer, testCommandSetPreset)
	assertMatrixRainPresetPayload(t, preset.Payload)
}

func TestAppRestoresConfiguredFirmwarePresetBackgroundThroughScheduler(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Background.Animation = "matrix_rain_background"
	cfg.Devices["default"].Background.RestoreOnIdle = true
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, "matrix_rain_background", animations.FirmwarePreset{
		EffectID: 44,
		Interval: 123 * time.Millisecond,
		Color:    animations.RGB{R: 1, G: 2, B: 3},
	})

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"1ms","restore":"background"}`, http.StatusAccepted)
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	preset := waitForMatrixCommand(t, matrixServer, testCommandSetPreset)
	want := []byte{44, 123, 0, 1, 2, 3}
	if !bytes.Equal(preset.Payload, want) {
		t.Fatalf("background preset payload = %v, want configured registry payload %v", preset.Payload, want)
	}
}

func TestReadyAndMetricsExposePreviousFrameBackgroundDedupeAsPlaybackRestoreConvergence(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	const backgroundID = "matrix_rain_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, backgroundID, animations.FirmwarePreset{
		EffectID: 44,
		Interval: 123 * time.Millisecond,
		Color:    animations.RGB{R: 1, G: 2, B: 3},
	})

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	ready, status := waitForReadyBackground(t, httpServer.URL, backgroundID, "firmware_preset", "converged")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if ready.DefaultDevice().Background.LastSuccess == nil {
		t.Fatalf("/readyz background last_success is nil after startup background restore: %#v", ready.DefaultDevice().Background)
	}
	initialLastSuccess := *ready.DefaultDevice().Background.LastSuccess
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		1, `kind="firmware_preset"`)
	initialAttempts := currentMetricLineValue(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		`kind="firmware_preset"`)
	if initialAttempts != 1 {
		t.Fatalf("background restore attempts after startup = %g, want 1", initialAttempts)
	}
	initialPreset := waitForMatrixCommand(t, matrixServer, testCommandSetPreset)
	wantPresetPayload := []byte{44, 123, 0, 1, 2, 3}
	if !bytes.Equal(initialPreset.Payload, wantPresetPayload) {
		t.Fatalf("startup background preset payload = %v, want %v", initialPreset.Payload, wantPresetPayload)
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"1ms","restore":"previous_frame"}`, http.StatusAccepted)
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	restoredPreset := waitForMatrixCommand(t, matrixServer, testCommandSetPreset)
	if !bytes.Equal(restoredPreset.Payload, wantPresetPayload) {
		t.Fatalf("previous-frame restored preset payload = %v, want %v", restoredPreset.Payload, wantPresetPayload)
	}
	time.Sleep(50 * time.Millisecond)

	ready, status = waitForReadyBackground(t, httpServer.URL, backgroundID, "firmware_preset", "converged")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if ready.DefaultDevice().Background.Dirty || !ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want false/true after previous-frame restore: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess == nil || !ready.DefaultDevice().Background.LastSuccess.Equal(initialLastSuccess) {
		t.Fatalf("/readyz background last_success = %v, want unchanged %v after playback restore convergence: %#v",
			ready.DefaultDevice().Background.LastSuccess, initialLastSuccess, ready.DefaultDevice().Background)
	}
	if got := currentMetricLineValue(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		`kind="firmware_preset"`); got != initialAttempts {
		t.Fatalf("background restore attempts after previous-frame convergence = %g, want unchanged %g", got, initialAttempts)
	}
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_restore_failures_total", `kind="firmware_preset"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="converged"`, " 1")
	assertNoRenderableBackgroundMetrics(t, httpServer.URL)
}

func TestReadyAndMetricsExposeFirmwarePresetBackgroundFailureAndRecoveryWithoutPlaybackPollution(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()
	matrixServer.FailCommandStatus(testCommandSetPreset, testStatusUnknownCommand)

	const backgroundID = "matrix_rain_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, backgroundID, animations.FirmwarePreset{
		EffectID: 44,
		Interval: 123 * time.Millisecond,
		Color:    animations.RGB{R: 1, G: 2, B: 3},
	})

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	ready, status := waitForReadyBackground(t, httpServer.URL, backgroundID, "firmware_preset", "retrying")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().MatrixConnected {
		t.Fatalf("/readyz matrix_connected = false, want true while background is retrying: %#v", ready)
	}
	if !ready.DefaultDevice().Background.Dirty || ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want true/false: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastAttempt == nil {
		t.Fatalf("/readyz background last_attempt is nil: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess != nil {
		t.Fatalf("/readyz background last_success = %v, want nil after failed restore", ready.DefaultDevice().Background.LastSuccess)
	}
	if ready.DefaultDevice().Background.NextRetry == nil || !ready.DefaultDevice().Background.NextRetry.After(time.Now()) {
		t.Fatalf("/readyz background next_retry = %v, want future retry after failed restore: %#v",
			ready.DefaultDevice().Background.NextRetry, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.FailureCount == 0 {
		t.Fatalf("/readyz background failure_count = 0, want nonzero after failed restore: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastError == "" {
		t.Fatalf("/readyz background last_error is empty: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastErrorClass != "permanent" {
		t.Fatalf("/readyz background last_error_class = %q, want permanent: %#v",
			ready.DefaultDevice().Background.LastErrorClass, ready.DefaultDevice().Background)
	}

	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		1, `kind="firmware_preset"`)
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_failures_total",
		1, `kind="firmware_preset"`, `error_class="permanent"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		[]string{"device", "kind"}, `kind="firmware_preset"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_restore_failures_total",
		[]string{"device", "error_class", "kind"}, `kind="firmware_preset"`, `error_class="permanent"`)
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_next_retry_seconds",
		1, `kind="firmware_preset"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="retrying"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="dirty"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="converged"`, " 0")
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_next_retry_seconds",
		[]string{"device", "kind"}, `kind="firmware_preset"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_state",
		[]string{"device", "kind", "state"}, `kind="firmware_preset"`, `state="retrying"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_next_retry_seconds", backgroundID)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_state", backgroundID)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_failure_count")
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total")

	matrixServer.RecoverCommandStatus(testCommandSetPreset)
	matrixServer.CloseActiveConnections()
	ready, status = waitForReadyBackground(t, httpServer.URL, backgroundID, "firmware_preset", "converged")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().MatrixConnected {
		t.Fatalf("/readyz matrix_connected = false after background recovery: %#v", ready)
	}
	if ready.DefaultDevice().Background.Dirty || !ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want false/true after recovery: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess == nil {
		t.Fatalf("/readyz background last_success is nil after recovery: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.NextRetry != nil {
		t.Fatalf("/readyz background next_retry = %v, want nil after recovery: %#v",
			ready.DefaultDevice().Background.NextRetry, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.FailureCount != 0 {
		t.Fatalf("/readyz background failure_count = %d, want 0 after recovery: %#v",
			ready.DefaultDevice().Background.FailureCount, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastError != "" || ready.DefaultDevice().Background.LastErrorClass != "none" {
		t.Fatalf("/readyz background last error after recovery = %q/%q, want empty/none: %#v",
			ready.DefaultDevice().Background.LastError, ready.DefaultDevice().Background.LastErrorClass, ready.DefaultDevice().Background)
	}
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		2, `kind="firmware_preset"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_next_retry_seconds",
		`kind="firmware_preset"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="converged"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="retrying"`, " 0")
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_next_retry_seconds", backgroundID)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_state", backgroundID)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total")
}

func TestReadyAndMetricsExposeDueBackgroundRetryAsFailedWhilePlaybackActive(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	const backgroundID = "matrix_rain_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].ResponseTimeout = 20 * time.Millisecond
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true
	cfg.AnimationRegistry = registryWithFirmwarePreset(t, backgroundID, animations.FirmwarePreset{
		EffectID: 44,
		Interval: 123 * time.Millisecond,
		Color:    animations.RGB{R: 1, G: 2, B: 3},
	})

	pausedBackgroundResponse := matrixServer.PauseNextCommandResponse(testCommandSetPreset)
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		matrixServer.ResumePausedResponse()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	select {
	case <-pausedBackgroundResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial background preset response to pause")
	}
	ready, status := waitForReadyBackground(t, httpServer.URL, backgroundID, "firmware_preset", "retrying")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if ready.DefaultDevice().Background.NextRetry == nil {
		t.Fatalf("/readyz background next_retry is nil while retrying: %#v", ready.DefaultDevice().Background)
	}
	nextRetry := *ready.DefaultDevice().Background.NextRetry
	failureCount := ready.DefaultDevice().Background.FailureCount
	if failureCount == 0 {
		t.Fatalf("/readyz background failure_count = 0, want retained failed retry state: %#v", ready.DefaultDevice().Background)
	}
	matrixServer.ResumePausedResponse()

	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"5s","restore":"leave"}`, http.StatusAccepted)
	waitForSchedulerState(t, httpServer.URL, "playing_transient")
	if sleep := time.Until(nextRetry.Add(50 * time.Millisecond)); sleep > 0 {
		time.Sleep(sleep)
	}

	ready, status = waitForReadyBackground(t, httpServer.URL, backgroundID, "firmware_preset", "failed")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d; background non-convergence must not fail readiness",
			status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().Background.Dirty || ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want true/false after due retry: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.NextRetry == nil || !ready.DefaultDevice().Background.NextRetry.Equal(nextRetry) {
		t.Fatalf("/readyz background next_retry = %v, want retained due timestamp %v: %#v",
			ready.DefaultDevice().Background.NextRetry, nextRetry, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.FailureCount != failureCount {
		t.Fatalf("/readyz background failure_count = %d, want retained %d: %#v",
			ready.DefaultDevice().Background.FailureCount, failureCount, ready.DefaultDevice().Background)
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_next_retry_seconds",
		`kind="firmware_preset"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="failed"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="retrying"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="attempting"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="firmware_preset"`, `state="dirty"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_dirty",
		`kind="firmware_preset"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_converged",
		`kind="firmware_preset"`, " 0")
}

func TestReadyAndMetricsExposeGeneratedBackgroundFrameFailureAndRecoveryWithoutPlaybackPollution(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()
	matrixServer.FailCommandStatus(testCommandSetFrame, testStatusUnknownCommand)

	const backgroundID = "generated_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Layout.OddRowDisplayFlip = false
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	backgroundFrames := generatedBackgroundFrameFixture(t)
	if err := registry.RegisterGenerated(backgroundID, "test_generated_background", animations.AnimationFunc(
		func(context.Context, animations.Params) ([]animations.Frame, error) {
			return append([]animations.Frame(nil), backgroundFrames...), nil
		},
	)); err != nil {
		t.Fatal(err)
	}
	cfg.AnimationRegistry = registry

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	ready, status := waitForReadyBackground(t, httpServer.URL, backgroundID, "generated", "retrying")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().MatrixConnected {
		t.Fatalf("/readyz matrix_connected = false, want true while generated background is retrying: %#v", ready)
	}
	if !ready.DefaultDevice().Background.Dirty || ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want true/false: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastAttempt == nil {
		t.Fatalf("/readyz background last_attempt is nil: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess != nil {
		t.Fatalf("/readyz background last_success = %v, want nil after failed restore", ready.DefaultDevice().Background.LastSuccess)
	}
	if ready.DefaultDevice().Background.LastError == "" {
		t.Fatalf("/readyz background last_error is empty: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastErrorClass != "permanent" {
		t.Fatalf("/readyz background last_error_class = %q, want permanent: %#v",
			ready.DefaultDevice().Background.LastErrorClass, ready.DefaultDevice().Background)
	}

	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		1, `kind="generated"`)
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_failures_total",
		1, `kind="generated"`, `error_class="permanent"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		[]string{"device", "kind"}, `kind="generated"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_restore_failures_total",
		[]string{"device", "error_class", "kind"}, `kind="generated"`, `error_class="permanent"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total")

	matrixServer.RecoverCommandStatus(testCommandSetFrame)
	matrixServer.CloseActiveConnections()
	ready, status = waitForReadyBackground(t, httpServer.URL, backgroundID, "generated", "converged")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().MatrixConnected {
		t.Fatalf("/readyz matrix_connected = false after generated background recovery: %#v", ready)
	}
	if ready.DefaultDevice().Background.Dirty || !ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want false/true after recovery: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess == nil {
		t.Fatalf("/readyz background last_success is nil after recovery: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastError != "" || ready.DefaultDevice().Background.LastErrorClass != "none" {
		t.Fatalf("/readyz background last error after recovery = %q/%q, want empty/none: %#v",
			ready.DefaultDevice().Background.LastError, ready.DefaultDevice().Background.LastErrorClass, ready.DefaultDevice().Background)
	}
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		2, `kind="generated"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total")
	assertNoRenderableBackgroundMetrics(t, httpServer.URL)
}

func TestReadyAndMetricsProjectRenderableBackgroundKindToGenerated(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	const backgroundID = "generated_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Layout.OddRowDisplayFlip = false
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	backgroundFrames := generatedBackgroundFrameFixture(t)
	if err := registry.RegisterGenerated(backgroundID, "test_generated_background", animations.AnimationFunc(
		func(context.Context, animations.Params) ([]animations.Frame, error) {
			return append([]animations.Frame(nil), backgroundFrames...), nil
		},
	)); err != nil {
		t.Fatal(err)
	}
	cfg.AnimationRegistry = registry

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	ready, status := waitForReadyBackground(t, httpServer.URL, backgroundID, "generated", "converged")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if ready.DefaultDevice().Background.Kind != "generated" {
		t.Fatalf("/readyz background.kind = %q, want generated: %#v", ready.DefaultDevice().Background.Kind, ready.DefaultDevice().Background)
	}

	readyBody := getReadyBody(t, httpServer.URL)
	if !strings.Contains(readyBody, `"kind":"generated"`) {
		t.Fatalf("/readyz response missing public generated kind:\n%s", readyBody)
	}
	if strings.Contains(readyBody, `"kind":"renderable"`) {
		t.Fatalf("/readyz response leaked internal renderable kind:\n%s", readyBody)
	}

	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		1, `kind="generated"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_dirty",
		`kind="generated"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_converged",
		`kind="generated"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state",
		`kind="generated"`, `state="converged"`, " 1")

	metricsBody := getMetrics(t, httpServer.URL)
	if strings.Contains(metricsBody, `kind="renderable"`) {
		t.Fatalf("/metrics leaked internal renderable kind:\n%s", metricsBody)
	}
	assertNoRenderableBackgroundMetrics(t, httpServer.URL)
}

func TestReadyAndMetricsExposePartialGeneratedBackgroundFrameFailureAndFullReplayRecovery(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()
	matrixServer.FailNextStatus(testCommandSetFrame, testStatusOK)
	matrixServer.FailNextStatus(testCommandSetFrame, testStatusUnknownCommand)

	const backgroundID = "generated_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Layout.OddRowDisplayFlip = false
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true

	backgroundFrames := generatedBackgroundFrameFixture(t)
	expectedBackgroundPayloads := expectedPackedPayloads(t, cfg, backgroundFrames)
	assertGeneratedBackgroundFixtureCatchesRawDisplayBypass(t, backgroundFrames, expectedBackgroundPayloads)

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterGenerated(backgroundID, "test_generated_background", animations.AnimationFunc(
		func(context.Context, animations.Params) ([]animations.Frame, error) {
			return append([]animations.Frame(nil), backgroundFrames...), nil
		},
	)); err != nil {
		t.Fatal(err)
	}
	cfg.AnimationRegistry = registry

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	waitForExactSetFramePayloads(t, matrixServer, expectedBackgroundPayloads[:2])
	ready, status := waitForReadyBackground(t, httpServer.URL, backgroundID, "generated", "retrying")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().MatrixConnected {
		t.Fatalf("/readyz matrix_connected = false, want true while partial background stream is retrying: %#v", ready)
	}
	if !ready.DefaultDevice().Background.Dirty || ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want true/false: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastAttempt == nil {
		t.Fatalf("/readyz background last_attempt is nil: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess != nil {
		t.Fatalf("/readyz background last_success = %v, want nil after partial stream failure", ready.DefaultDevice().Background.LastSuccess)
	}
	if ready.DefaultDevice().Background.LastError == "" {
		t.Fatalf("/readyz background last_error is empty: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastErrorClass != "permanent" {
		t.Fatalf("/readyz background last_error_class = %q, want permanent: %#v",
			ready.DefaultDevice().Background.LastErrorClass, ready.DefaultDevice().Background)
	}

	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		1, `kind="generated"`)
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_failures_total",
		1, `kind="generated"`, `error_class="permanent"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_dirty",
		`kind="generated"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_converged",
		`kind="generated"`, " 0")
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_dirty",
		[]string{"device", "kind"}, `kind="generated"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_background_converged",
		[]string{"device", "kind"}, `kind="generated"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total")

	matrixServer.CloseActiveConnections()
	waitForExactSetFramePayloads(t, matrixServer, expectedBackgroundPayloads)
	ready, status = waitForReadyBackground(t, httpServer.URL, backgroundID, "generated", "converged")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %#v, want %d", status, ready, http.StatusOK)
	}
	if !ready.DefaultDevice().MatrixConnected {
		t.Fatalf("/readyz matrix_connected = false after partial background recovery: %#v", ready)
	}
	if ready.DefaultDevice().Background.Dirty || !ready.DefaultDevice().Background.Converged {
		t.Fatalf("/readyz background dirty/converged = %v/%v, want false/true after recovery: %#v",
			ready.DefaultDevice().Background.Dirty, ready.DefaultDevice().Background.Converged, ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastSuccess == nil {
		t.Fatalf("/readyz background last_success is nil after recovery: %#v", ready.DefaultDevice().Background)
	}
	if ready.DefaultDevice().Background.LastError != "" || ready.DefaultDevice().Background.LastErrorClass != "none" {
		t.Fatalf("/readyz background last error after recovery = %q/%q, want empty/none: %#v",
			ready.DefaultDevice().Background.LastError, ready.DefaultDevice().Background.LastErrorClass, ready.DefaultDevice().Background)
	}
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total",
		2, `kind="generated"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_dirty",
		`kind="generated"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_converged",
		`kind="generated"`, " 1")
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total")
	assertNoRenderableBackgroundMetrics(t, httpServer.URL)
}

func TestAppRestoresMatrixRainBackgroundAfterFakeESPReconnectThroughTCP(t *testing.T) {
	matrixAddr := reserveTCPAddr(t)
	matrixServer := newFakeESPServerAt(t, matrixAddr)

	cfg := loadMatrixRainBackgroundTestConfig(t, matrixAddr)
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		matrixServer.Close()
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	assertMatrixRainPresetPayload(t, waitForMatrixCommand(t, matrixServer, testCommandSetPreset).Payload)

	matrixServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)

	replacement := newFakeESPServerAt(t, matrixAddr)
	defer replacement.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	assertMatrixRainPresetPayload(t, waitForMatrixCommand(t, replacement, testCommandSetPreset).Payload)
}

func TestAppRestoresBackgroundAfterStreamedHTTPNotificationFrames(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := loadMatrixRainBackgroundTestConfig(t, matrixServer.Addr())
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	assertMatrixRainPresetPayload(t, waitForMatrixCommand(t, matrixServer, testCommandSetPreset).Payload)
	drainMatrixFrames(matrixServer)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/notify", `{"title":"Test","message":"hello","duration":"550ms"}`, http.StatusAccepted)
	frames := waitForMatrixCommandSequenceUntil(t, matrixServer, testCommandSetPreset)
	var setFrameCount int
	for i, frame := range frames {
		switch frame.Command {
		case testCommandPing:
		case testCommandSetFrame:
			setFrameCount++
			if len(frame.Payload) != testFramePayloadSize {
				t.Fatalf("SetFullFrame payload length = %d, want %d", len(frame.Payload), testFramePayloadSize)
			}
			if allZero(frame.Payload) {
				t.Fatal("SetFullFrame payload is all zero, want generated notification pixels")
			}
		case testCommandSetPreset:
			if i != len(frames)-1 {
				t.Fatalf("background preset appeared before the end of recorded command sequence: %v", commandBytes(frames))
			}
			assertMatrixRainPresetPayload(t, frame.Payload)
		default:
			t.Fatalf("unexpected matrix command 0x%02x in notification sequence", frame.Command)
		}
	}
	if setFrameCount != 3 {
		t.Fatalf("SetFullFrame command count before background restore = %d, want 3 for finite notification stream", setFrameCount)
	}
}

func TestAppStreamsGeneratedBackgroundFramesThroughFakeESP(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	const backgroundID = "generated_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Layout.OddRowDisplayFlip = false
	cfg.Devices["default"].Background.Animation = backgroundID
	cfg.Devices["default"].Background.RestoreOnIdle = true

	backgroundFrames := generatedBackgroundFrameFixture(t)
	expectedBackgroundPayloads := expectedPackedPayloads(t, cfg, backgroundFrames)
	assertGeneratedBackgroundFixtureCatchesRawDisplayBypass(t, backgroundFrames, expectedBackgroundPayloads)

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterGenerated(backgroundID, "test_generated_background", animations.AnimationFunc(
		func(context.Context, animations.Params) ([]animations.Frame, error) {
			return append([]animations.Frame(nil), backgroundFrames...), nil
		},
	)); err != nil {
		t.Fatal(err)
	}
	cfg.AnimationRegistry = registry

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	waitForExactSetFramePayloads(t, matrixServer, expectedBackgroundPayloads)
	waitForSchedulerState(t, httpServer.URL, "ready")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
	assertNoBufferedSetFrame(t, matrixServer)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/notify", `{"title":"Test","message":"hello","duration":"550ms"}`, http.StatusAccepted)
	notificationFrames := waitForGeneratedBackgroundAfterNotification(t, matrixServer, expectedBackgroundPayloads)
	if notificationFrames != 3 {
		t.Fatalf("notification SetFullFrame command count before generated background restore = %d, want 3", notificationFrames)
	}
	waitForSchedulerState(t, httpServer.URL, "ready")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
	assertNoBufferedSetFrame(t, matrixServer)
}

func TestAppPlaysConfigAuthoredFrameAnimationThroughFakeESP(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := loadFramePlaybackTestConfig(t, matrixServer.Addr())
	animation, ok := cfg.AnimationRegistry.Get("orientation_badge")
	if !ok {
		t.Fatal("config-authored frame animation is not registered")
	}
	frames, err := animation.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	expectedPayloads := expectedPackedPayloads(t, cfg, frames)
	assertFramePlaybackFixtureCatchesUncompensatedLayout(t, frames, expectedPayloads)

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	drainMatrixFrames(matrixServer)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"orientation_badge","duration":"250ms","restore":"leave"}`, http.StatusAccepted)
	waitForExactSetFramePayloads(t, matrixServer, expectedPayloads)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_animation_render_duration_seconds_count",
		`animation="orientation_badge"`)
	waitForSchedulerState(t, httpServer.URL, "ready")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="animation"`, `item="orientation_badge"`, `outcome="executed"`, " 1")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
	assertNoBufferedSetFrame(t, matrixServer)
}

func TestMetricsRemainReliableWhenOutcomeObserverDropsUnderBackpressure(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Queue.PlayBuffer = 32
	logHandler := newBlockingOutcomeLogHandler()
	application, err := app.New(cfg, slog.New(logHandler))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		matrixServer.ResumePausedFrameResponse()
		logHandler.release()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pausedFrameResponse := matrixServer.PauseNextFrameResponse()
	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"750ms","restore":"leave"}`, http.StatusAccepted)
	waitForMatrixCommand(t, matrixServer, testCommandSetFrame)
	select {
	case <-pausedFrameResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first frame response to pause")
	}

	const queuedOutcomes = 24
	for i := 0; i < queuedOutcomes; i++ {
		postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"500ms","restore":"leave"}`, http.StatusAccepted)
	}
	waitForQueueDepth(t, httpServer.URL, queuedOutcomes)

	if cleared := deleteQueue(t, httpServer.URL); cleared != queuedOutcomes {
		t.Fatalf("DELETE /queue cleared = %d, want %d", cleared, queuedOutcomes)
	}
	select {
	case <-logHandler.entered:
	case <-time.After(time.Second):
		t.Fatal("outcome log handler was not invoked")
	}
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="animation"`, `item="notification"`, `outcome="queue_cleared"`, " "+strconv.Itoa(queuedOutcomes))
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_play_item_outcomes_dropped_total", 1, `device="default"`)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
}

func TestEventWorkerMetricsTrackSubscriberBacklogAndInflightEvent(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Queue.EventsBuffer = 4
	logHandler := newBlockingMessageLogHandler("event did not match any rule")
	application, err := app.New(cfg, slog.New(logHandler))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		logHandler.release()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	assertMetricHelpLine(t, httpServer.URL, "matrix_proxy_event_queue_depth",
		"normalized events waiting in the app event-worker subscriber channel")
	assertMetricHelpLine(t, httpServer.URL, "matrix_proxy_event_worker_inflight",
		"app event worker is actively processing one received event")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "0")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_worker_inflight", "0")

	select {
	case <-logHandler.entered:
		t.Fatal("event worker log handler blocked before test event was published")
	default:
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-queue-depth-test"}`, http.StatusAccepted)
	select {
	case <-logHandler.entered:
	case <-time.After(time.Second):
		t.Fatal("event worker did not reach blocking log handler")
	}
	ready := waitForReadyEventWorker(t, httpServer.URL, "processing", "log_drop", true)
	if ready.EventWorker.ActiveDurationSeconds == nil || *ready.EventWorker.ActiveDurationSeconds <= 0 {
		t.Fatalf("/readyz event worker active duration = %v, want positive", ready.EventWorker.ActiveDurationSeconds)
	}
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_events_total",
		`source="external"`, `type="event-queue-depth-test"`, " 1")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "0")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_worker_inflight", "1")

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-queue-depth-test"}`, http.StatusAccepted)
	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-queue-depth-test"}`, http.StatusAccepted)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "2")

	logHandler.release()
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "0")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_worker_inflight", "0")
	waitForReadyEventWorker(t, httpServer.URL, "idle", "", false)
}

func TestEventPublisherBackpressureMetricsAreExposedWithStableLabels(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := application.Close(); err != nil && !errors.Is(err, app.ErrAppClosed) {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	assertMetricHelpLine(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds",
		"event publishers spent blocked behind full subscriber channels")
	assertMetricHelpLine(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_timeouts_total",
		"publish context expired while blocked behind subscriber backpressure")
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_count", nil)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_sum", nil)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_bucket", []string{"le"}, `le="+Inf"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_timeouts_total", nil)
	staleMetric := "matrix_proxy_event_publish_backpressure_" + "wait_seconds"
	assertNoMetricLine(t, httpServer.URL, "# HELP "+staleMetric)
	assertNoMetricLine(t, httpServer.URL, "# TYPE "+staleMetric)
	assertNoMetricLine(t, httpServer.URL, staleMetric)
}

func TestEventPublisherBackpressureMetricsIncreaseWhenPublishBlocks(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Queue.EventsBuffer = 1
	logHandler := newBlockingMessageLogHandler("event did not match any rule")
	application, err := app.New(cfg, slog.New(logHandler))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		logHandler.release()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-publish-backpressure-test"}`, http.StatusAccepted)
	select {
	case <-logHandler.entered:
	case <-time.After(time.Second):
		t.Fatal("event worker did not reach blocking log handler")
	}
	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-publish-backpressure-test"}`, http.StatusAccepted)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "1")

	statuses := make(chan int, 1)
	go func() {
		statuses <- postJSONStatus(httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-publish-backpressure-test"}`)
	}()
	assertHTTPPostStillBlocked(t, statuses)

	logHandler.release()
	select {
	case status := <-statuses:
		if status != http.StatusAccepted {
			t.Fatalf("blocked POST /events status = %d, want %d", status, http.StatusAccepted)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked POST /events did not return after event worker was released")
	}

	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_count", 1)
	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_sum", 0.001)
	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_timeouts_total", 0)
}

func TestEventPublisherBackpressureTimeoutMetricIncreasesWhenPublishContextExpires(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Queue.EventsBuffer = 1
	logHandler := newBlockingMessageLogHandler("event did not match any rule")
	application, err := app.New(cfg, slog.New(logHandler))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		logHandler.release()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-publish-backpressure-timeout-test"}`, http.StatusAccepted)
	select {
	case <-logHandler.entered:
	case <-time.After(time.Second):
		t.Fatal("event worker did not reach blocking log handler")
	}
	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-publish-backpressure-timeout-test"}`, http.StatusAccepted)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "1")

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer reqCancel()
	results := make(chan httpPostResult, 1)
	go func() {
		status, err := postJSONStatusWithContext(reqCtx, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-publish-backpressure-timeout-test"}`)
		results <- httpPostResult{status: status, err: err}
	}()
	assertHTTPPostStillPending(t, results)

	select {
	case result := <-results:
		if result.err == nil && result.status != http.StatusServiceUnavailable {
			t.Fatalf("timed-out POST /events status = %d, error = %v; want context error or %d",
				result.status, result.err, http.StatusServiceUnavailable)
		}
	case <-time.After(time.Second):
		t.Fatal("POST /events did not return after request context timeout")
	}

	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_count", 1)
	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_timeouts_total", 1)
}

func TestEventWorkerMetricsReturnToZeroAfterShutdownWithBlockedPublisher(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Queue.EventsBuffer = 1
	logHandler := newBlockingMessageLogHandler("event did not match any rule")
	application, err := app.New(cfg, slog.New(logHandler))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		logHandler.release()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-worker-shutdown-depth-race"}`, http.StatusAccepted)
	select {
	case <-logHandler.entered:
	case <-time.After(time.Second):
		t.Fatal("event worker did not reach blocking log handler")
	}
	waitForReadyEventWorker(t, httpServer.URL, "processing", "log_drop", true)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "0")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_worker_inflight", "1")

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-worker-shutdown-depth-race"}`, http.StatusAccepted)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "1")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_worker_inflight", "1")

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer reqCancel()
	results := make(chan httpPostResult, 1)
	go func() {
		status, err := postJSONStatusWithContext(reqCtx, httpServer.URL+"/api/v1/devices/default/events", `{"type":"event-worker-shutdown-depth-race"}`)
		results <- httpPostResult{status: status, err: err}
	}()
	assertHTTPPostStillPending(t, results)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- application.Shutdown(shutdownCtx)
	}()
	assertShutdownStillPending(t, shutdownDone)

	select {
	case result := <-results:
		if result.err == nil && result.status != http.StatusServiceUnavailable {
			t.Fatalf("timed-out POST /events status = %d, error = %v; want context error or %d",
				result.status, result.err, http.StatusServiceUnavailable)
		}
	case <-time.After(time.Second):
		t.Fatal("POST /events did not return after request context timeout")
	}
	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_count", 1)
	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_duration_seconds_sum", 0.001)
	waitForMetricValueAtLeast(t, httpServer.URL, "matrix_proxy_event_publish_backpressure_timeouts_total", 1)

	logHandler.release()
	if err := waitShutdownResult(t, shutdownDone); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_queue_depth", "0")
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_event_worker_inflight", "0")
	waitForReadyEventWorker(t, httpServer.URL, "idle", "", false)
	assertGaugeValueRemains(t, httpServer.URL, "matrix_proxy_event_queue_depth", 0, 150*time.Millisecond)
	assertGaugeValueRemains(t, httpServer.URL, "matrix_proxy_event_worker_inflight", 0, 150*time.Millisecond)
}

func TestReadyAndMetricsExposeOutcomeRecordingPanics(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	resp, err := http.Get(httpServer.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /readyz status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	recordingPanics, ok := body["outcome_recording_panics"]
	if !ok {
		t.Fatalf("/readyz missing outcome_recording_panics in %#v", body)
	}
	if recordingPanics != float64(0) {
		t.Fatalf("/readyz outcome_recording_panics = %v, want 0", recordingPanics)
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_item_outcome_recording_panics_total", " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_item_outcomes_dropped_total", " 0")
}

func TestMetricsExposeSchedulerReconnectProbeAndConnectedTransitions(t *testing.T) {
	matrixAddr := reserveTCPAddr(t)
	application, err := app.New(newHTTPMatrixTestConfig(t, matrixAddr), slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="scheduler_backoff"`, `error_kind="retryable"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_delay_seconds_count",
		`source="scheduler_backoff"`)

	matrixServer := newFakeESPServerAt(t, matrixAddr)
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="scheduler_backoff"`, `state="ready"`)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_matrix_connected", "1")

	matrixServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_matrix_connected", "0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_probe_failures_total",
		`error_kind="retryable"`, `reason="transport"`)

	matrixServer = newFakeESPServerAt(t, matrixAddr)
	defer matrixServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_matrix_connected", "1")
}

func TestMetricsExposeProbeTimeoutReason(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].HeartbeatInterval = 10 * time.Millisecond
	cfg.Devices["default"].ProbeTimeout = 20 * time.Millisecond
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		matrixServer.ResumePausedResponse()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	pausedPingResponse := matrixServer.PauseNextCommandResponse(testCommandPing)
	select {
	case <-pausedPingResponse:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ping response to pause")
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_probe_failures_total",
		`error_kind="retryable"`, `reason="probe_timeout"`)
}

func TestReadyAndMetricsExposeMatrixObservabilityCallbackPanics(t *testing.T) {
	matrixAddr := reserveTCPAddr(t)
	application, err := app.New(
		newHTTPMatrixTestConfig(t, matrixAddr),
		slog.New(&panickingLogHandler{message: "matrix reconnect attempt"}),
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

	waitForReadyObservabilityCallbackPanicsAtLeast(t, httpServer.URL, 1)
	waitForMetricLineValueAtLeast(t, httpServer.URL,
		"matrix_proxy_matrix_observability_callback_panics_total", 1,
		`source="scheduler_backoff"`,
		`callback="reconnect_delay"`)
}

func TestReadyAndMetricsExposeTCPImmediateObservabilityCallbackPanics(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(
		newHTTPMatrixTestConfig(t, matrixServer.Addr()),
		slog.New(&panickingLogHandler{message: "matrix reconnect attempt"}),
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.CloseActiveConnections()
	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="tcp_immediate"`, `error_kind="retryable"`)
	waitForReadyObservabilityCallbackPanicsAtLeast(t, httpServer.URL, 1)
	ready, _ := getReadyDetails(t, httpServer.URL)
	if got := ready.ObservabilityCallbackCounts["reconnect_attempt"]; got == 0 {
		t.Fatalf("/readyz reconnect_attempt observability callback panics = %d, want nonzero; counts = %v", got, ready.ObservabilityCallbackCounts)
	}
	waitForMetricLineValueAtLeast(t, httpServer.URL,
		"matrix_proxy_matrix_observability_callback_panics_total", 1,
		`source="tcp_immediate"`,
		`callback="reconnect_attempt"`)
}

func TestReadyAndMetricsExposeTCPImmediateReconnectFailureCallbackPanics(t *testing.T) {
	matrixServer := newFakeESPServer(t)

	application, err := app.New(
		newHTTPMatrixTestConfig(t, matrixServer.Addr()),
		slog.New(&panickingLogHandler{message: "matrix reconnect failure"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		matrixServer.Close()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.Close()
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer reqCancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, httpServer.URL+"/api/v1/devices/default/matrix/fill", bytes.NewBufferString(`{"r":1,"g":2,"b":3}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
		`source="tcp_immediate"`, `error_kind="retryable"`, `outcome="failed"`)
	assertMetricHelpLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
		"before firmware-verified replacement connectivity",
		`outcome=verification_failed`)
	waitForReadyObservabilityCallbackCountAtLeast(t, httpServer.URL, "reconnect_failure", 1)
	waitForMetricLineValueAtLeast(t, httpServer.URL,
		"matrix_proxy_matrix_observability_callback_panics_total", 1,
		`source="tcp_immediate"`,
		`callback="reconnect_failure"`)
}

func TestTCPImmediateReconnectLoggingDoesNotBlockMatrixCommand(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	logHandler := newBlockingMessageLogHandler("matrix reconnect attempt")
	application, err := app.New(
		newHTTPMatrixTestConfig(t, matrixServer.Addr()),
		slog.New(logHandler),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		logHandler.release()
		cancel()
		waitAppWorkers(t, done)
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.CloseActiveConnections()
	statuses := make(chan int, 1)
	go func() {
		statuses <- postJSONStatus(httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`)
	}()

	var status int
	select {
	case <-logHandler.entered:
		select {
		case status = <-statuses:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("matrix command waited on blocked reconnect logging")
		}
	case status = <-statuses:
		select {
		case <-logHandler.entered:
		case <-time.After(time.Second):
			t.Fatal("reconnect log handler was not invoked")
		}
	case <-time.After(time.Second):
		t.Fatal("matrix command did not return")
	}
	if status != http.StatusOK {
		t.Fatalf("POST /matrix/fill status = %d, want %d", status, http.StatusOK)
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="tcp_immediate"`, `error_kind="retryable"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="ok"`)
}

func TestReadyAndMetricsExposeTCPReconnectLogDrops(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	logHandler := newBlockingMessageLogHandler("matrix reconnect attempt")
	application, err := app.New(
		newHTTPMatrixTestConfig(t, matrixServer.Addr()),
		slog.New(logHandler),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	drainDone := make(chan struct{})
	defer func() {
		logHandler.release()
		close(drainDone)
		cancel()
		waitAppWorkers(t, done)
	}()
	go func() {
		for {
			select {
			case <-matrixServer.frames:
			case <-matrixServer.responses:
			case <-drainDone:
				return
			}
		}
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.CloseActiveConnections()
	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)
	select {
	case <-logHandler.entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect log handler was not invoked")
	}

	for i := 0; i < 40; i++ {
		matrixServer.CloseActiveConnections()
		postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)
	}

	waitForMetricLine(t, httpServer.URL, "# HELP matrix_proxy_tcp_reconnect_log_events_dropped_total",
		"best-effort TCP reconnect log events dropped before slog handling")
	waitForMetricLineValueAtLeast(t, httpServer.URL, "matrix_proxy_tcp_reconnect_log_events_dropped_total", 1, `device="default"`)
	waitForReadyTCPReconnectLogDropsAtLeast(t, httpServer.URL, 1)
}

func TestMetricsSourceLabelDistinguishesSharedObservabilityCallbackPanics(t *testing.T) {
	matrixAddr := reserveTCPAddr(t)
	application, err := app.New(
		newHTTPMatrixTestConfig(t, matrixAddr),
		slog.New(&panickingLogHandler{message: "matrix reconnect recovered"}),
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

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusServiceUnavailable)
	matrixServer := newFakeESPServerAt(t, matrixAddr)
	defer matrixServer.Close()

	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	waitForMetricLineValueAtLeast(t, httpServer.URL,
		"matrix_proxy_matrix_observability_callback_panics_total", 1,
		`source="scheduler_backoff"`,
		`callback="reconnect_recovered"`)

	matrixServer.CloseActiveConnections()
	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)

	waitForMetricLineValueAtLeast(t, httpServer.URL,
		"matrix_proxy_matrix_observability_callback_panics_total", 1,
		`source="tcp_immediate"`,
		`callback="reconnect_recovered"`)
	waitForReadyObservabilityCallbackCountAtLeast(t, httpServer.URL, "reconnect_recovered", 2)
}

func TestMetricsDocumentReconnectRecoveryVersusRetriedCommandFailure(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	matrixServer.CloseActiveConnections()
	matrixServer.FailNextStatus(testCommandFill, testStatusUnknownCommand)
	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusBadGateway)

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		`source="tcp_immediate"`, `error_kind="retryable"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_reconnects_total",
		[]string{"device", "error_kind", "source"}, `source="tcp_immediate"`, `error_kind="retryable"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		`source="tcp_immediate"`, `state="ready"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		[]string{"device", "source", "state"}, `source="tcp_immediate"`, `state="ready"`)
	assertMetricHelpLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_recoveries_total",
		"firmware-verified replacement connectivity",
		"retried-command transport failure is command telemetry")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="transport_error"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		`command="fill"`, `status="unknown_command"`)
	assertMetricLabelKeys(t, httpServer.URL, "matrix_proxy_matrix_commands_total",
		[]string{"device", "command", "status"}, `command="fill"`, `status="unknown_command"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_matrix_reconnect_failures_total",
		`source="tcp_immediate"`)
}

func TestMetricsReportSchedulerStoppedForActiveAnimationShutdown(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	workersStopped := false
	defer func() {
		cancel()
		if !workersStopped {
			waitAppWorkers(t, done)
		}
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/play", `{"animation":"notification","duration":"2s","restore":"leave"}`, http.StatusAccepted)
	waitForMatrixResponse(t, matrixServer, testCommandSetFrame)

	cancel()
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_play_items_total",
		`item_kind="animation"`, `item="notification"`, `outcome="scheduler_stopped"`, " 1")
	waitAppWorkers(t, done)
	workersStopped = true

	outcomes := playItemOutcomeMetrics(t, httpServer.URL, "animation", "notification")
	if got := outcomes["scheduler_stopped"]; got != 1 {
		t.Fatalf("scheduler_stopped metric = %g, want 1; outcomes: %#v", got, outcomes)
	}
	if got := terminalOutcomeTotal(outcomes); got != 1 {
		t.Fatalf("terminal animation metric total = %g, want 1; outcomes: %#v", got, outcomes)
	}
}

func TestAppCloseIdempotentlyClosesNeverRunResources(t *testing.T) {
	application, err := app.New(newNeverRunAppTestConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	if err := application.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}

	ready, status := getReadyDetails(t, httpServer.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusServiceUnavailable, ready)
	}
	if ready.Status != "not_ready" {
		t.Fatalf("/readyz status = %q, want not_ready", ready.Status)
	}
	if ready.WorkersRunning {
		t.Fatal("/readyz workers_running = true, want false")
	}
	if !ready.Draining {
		t.Fatal("/readyz draining = false, want true")
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"close-test"}`, http.StatusServiceUnavailable)
	if err := application.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("RunWorkers() after Close() error = %v, want ErrAppClosed", err)
	}
}

func TestAppCloseWhileWorkersActiveReturnsErrAndLeavesWorkersRunning(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	workersStopped := false
	defer func() {
		cancel()
		if !workersStopped {
			waitAppWorkers(t, done)
		}
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
	ready, status := getReadyDetails(t, httpServer.URL)
	if status != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusOK, ready)
	}
	if !ready.WorkersRunning {
		t.Fatal("/readyz workers_running = false, want true")
	}
	if ready.Draining {
		t.Fatal("/readyz draining = true, want false")
	}

	if err := application.Close(); !errors.Is(err, app.ErrAppRunning) {
		t.Fatalf("Close() error = %v, want ErrAppRunning", err)
	}

	select {
	case err := <-done:
		t.Fatalf("RunWorkers() stopped after Close() with error %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"close-test"}`, http.StatusAccepted)
	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)

	cancel()
	waitAppWorkers(t, done)
	workersStopped = true

	if err := application.Close(); err != nil {
		t.Fatalf("Close() after RunWorkers stopped error = %v, want nil", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close() after RunWorkers stopped error = %v, want nil", err)
	}
}

func TestAppRunWorkersAfterExternalContextStopReturnsErrAppClosed(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	workersStopped := false
	defer func() {
		cancel()
		if !workersStopped {
			waitAppWorkers(t, done)
		}
	}()

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")

	cancel()
	waitAppWorkers(t, done)
	workersStopped = true

	ready, status := getReadyDetails(t, httpServer.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusServiceUnavailable, ready)
	}
	if ready.Status != "not_ready" {
		t.Fatalf("/readyz status = %q, want not_ready", ready.Status)
	}
	if ready.WorkersRunning {
		t.Fatal("/readyz workers_running = true, want false")
	}
	if !ready.Draining {
		t.Fatal("/readyz draining = false, want true")
	}

	if err := application.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("RunWorkers() after external context stop error = %v, want ErrAppClosed", err)
	}

	if err := application.Close(); err != nil {
		t.Fatalf("Close() after external context stop error = %v, want nil", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close() after external context stop error = %v, want nil", err)
	}
	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"post-stop-close-test"}`, http.StatusServiceUnavailable)
	waitForGaugeValue(t, httpServer.URL, "matrix_proxy_play_queue_depth", "0")
}

func TestAppCloseRacingRunWorkersStartupDoesNotCloseResourcesUnderWorkers(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	for i := 0; i < 25; i++ {
		application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}

		httpServer := httptest.NewServer(application.Handler())
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		runDone := make(chan error, 1)
		closeDone := make(chan error, 1)

		go func() {
			<-start
			runDone <- application.RunWorkers(ctx)
		}()
		go func() {
			<-start
			closeDone <- application.Close()
		}()
		close(start)

		closeErr := waitCloseResult(t, closeDone)
		switch {
		case errors.Is(closeErr, app.ErrAppRunning):
			waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)
			postJSON(t, httpServer.URL+"/api/v1/devices/default/matrix/fill", `{"r":3,"g":4,"b":5}`, http.StatusOK)

			cancel()
			waitAppWorkers(t, runDone)
			if err := application.Close(); err != nil {
				t.Fatalf("iteration %d: Close() after workers stopped error = %v, want nil", i, err)
			}
		case closeErr == nil:
			err := waitRunWorkersResult(t, runDone)
			if !errors.Is(err, app.ErrAppClosed) {
				t.Fatalf("iteration %d: RunWorkers() error = %v, want ErrAppClosed after Close() won race", i, err)
			}
			postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"close-race-test"}`, http.StatusServiceUnavailable)
		default:
			t.Fatalf("iteration %d: Close() error = %v, want nil or ErrAppRunning", i, closeErr)
		}

		cancel()
		httpServer.Close()
		if err := application.Shutdown(context.Background()); err != nil && !errors.Is(err, app.ErrAppClosed) {
			t.Fatalf("iteration %d: cleanup Shutdown() error = %v", i, err)
		}
	}
}

func TestAppShutdownRunningCancelsWorkersClosesResourcesAndPreventsRestart(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := runAppWorkers(t, application, context.Background())
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	waitForStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := application.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWorkers() error after Shutdown() = %v, want nil", err)
		}
	default:
		t.Fatal("Shutdown() returned before RunWorkers() stopped")
	}

	ready, status := getReadyDetails(t, httpServer.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusServiceUnavailable, ready)
	}
	if ready.Status != "not_ready" {
		t.Fatalf("/readyz status = %q, want not_ready", ready.Status)
	}
	if ready.WorkersRunning {
		t.Fatal("/readyz workers_running = true, want false")
	}
	if !ready.Draining {
		t.Fatal("/readyz draining = false, want true")
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"shutdown-test"}`, http.StatusServiceUnavailable)
	if err := application.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("RunWorkers() after Shutdown() error = %v, want ErrAppClosed", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v, want nil", err)
	}
}

func TestAppRunContextCancellationClosesResourcesAndPreventsRestart(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Server.Addr = reserveTCPAddr(t)
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runApp(t, application, ctx)
	baseURL := "http://" + cfg.Server.Addr
	waitForStatus(t, baseURL+"/readyz", http.StatusOK)
	waitForActiveMatrixConnections(t, matrixServer, 1)

	postJSON(t, baseURL+"/api/v1/devices/default/matrix/fill", `{"r":1,"g":2,"b":3}`, http.StatusOK)
	waitForGaugeValue(t, baseURL, "matrix_proxy_play_queue_depth", "0")

	cancel()
	waitAppRun(t, done)
	waitForClosedMatrixConnections(t, matrixServer, 1)

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	ready, status := getReadyDetails(t, httpServer.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusServiceUnavailable, ready)
	}
	if ready.Status != "not_ready" {
		t.Fatalf("/readyz status = %q, want not_ready", ready.Status)
	}
	if ready.WorkersRunning {
		t.Fatal("/readyz workers_running = true, want false")
	}
	if !ready.Draining {
		t.Fatal("/readyz draining = false, want true")
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"run-context-cancel-test"}`, http.StatusServiceUnavailable)
	if err := application.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("RunWorkers() after Run context cancellation error = %v, want ErrAppClosed", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after Run context cancellation error = %v, want nil", err)
	}
}

func TestAppRunListenFailureClosesResourcesAndPreventsRestart(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Server.Addr = occupied.Addr().String()
	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	err = application.Run(context.Background())
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("Run() error = %v, want address already in use", err)
	}
	waitForActiveMatrixConnections(t, matrixServer, 0)

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	ready, status := getReadyDetails(t, httpServer.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body = %#v", status, http.StatusServiceUnavailable, ready)
	}
	if ready.Status != "not_ready" {
		t.Fatalf("/readyz status = %q, want not_ready", ready.Status)
	}
	if ready.WorkersRunning {
		t.Fatal("/readyz workers_running = true, want false")
	}
	if !ready.Draining {
		t.Fatal("/readyz draining = false, want true")
	}

	postJSON(t, httpServer.URL+"/api/v1/devices/default/events", `{"type":"run-listen-failure-test"}`, http.StatusServiceUnavailable)
	if err := application.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("RunWorkers() after Run listen failure error = %v, want ErrAppClosed", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after Run listen failure error = %v, want nil", err)
	}
}

func TestAppShutdownIdempotentForNeverRunAndAlreadyClosedApps(t *testing.T) {
	var nilApp *app.App
	if err := nilApp.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Shutdown() error = %v, want nil", err)
	}

	neverRun, err := app.New(newNeverRunAppTestConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := neverRun.Shutdown(context.Background()); err != nil {
		t.Fatalf("never-run Shutdown() error = %v, want nil", err)
	}
	if err := neverRun.Shutdown(context.Background()); err != nil {
		t.Fatalf("second never-run Shutdown() error = %v, want nil", err)
	}
	if err := neverRun.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("never-run RunWorkers() after Shutdown() error = %v, want ErrAppClosed", err)
	}

	alreadyClosed, err := app.New(newNeverRunAppTestConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := alreadyClosed.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := alreadyClosed.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() after Close() error = %v, want nil", err)
	}
	if err := alreadyClosed.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() after Close() error = %v, want nil", err)
	}
	if err := alreadyClosed.RunWorkers(context.Background()); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("already-closed RunWorkers() error = %v, want ErrAppClosed", err)
	}
}

func TestAppNewRollsBackResourcesWhenHTTPAPIFails(t *testing.T) {
	cfg := newNeverRunAppTestConfig(t)
	cfg.Server.Addr = "0.0.0.0:0"
	cfg.Server.AdminTokenEnv = ""

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		if application != nil {
			_ = application.Close()
		}
		t.Fatal("app.New() error = nil, want non-local admin token error")
	}
	if application != nil {
		_ = application.Close()
		t.Fatal("app.New() returned application on error")
	}
	if !strings.Contains(err.Error(), "server.admin_token_env is required") {
		t.Fatalf("app.New() error = %q, want server.admin_token_env is required", err)
	}
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

	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Devices["default"].Host = host
	cfg.Devices["default"].Port = port
	cfg.Devices["default"].ConnectTimeout = 20 * time.Millisecond
	cfg.Devices["default"].ResponseTimeout = time.Second
	cfg.Devices["default"].HeartbeatInterval = 20 * time.Millisecond
	cfg.Devices["default"].ProbeTimeout = 50 * time.Millisecond
	cfg.Devices["default"].ReconnectMinDelay = 10 * time.Millisecond
	cfg.Devices["default"].ReconnectMaxDelay = 50 * time.Millisecond
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = writeRulesFile(t)
	return cfg
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

func loadMatrixRainBackgroundTestConfig(t *testing.T, matrixAddr string) config.Config {
	t.Helper()
	t.Setenv("MATRIX_PROXY_CONFIG", "")

	host, portText, err := net.SplitHostPort(matrixAddr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	animationsPath := findRepoFile(t, "configs/animations.example.yaml")
	rulesPath := writeRulesFile(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf(`server:
  addr: "127.0.0.1:0"
  admin_token_env: ""
matrix:
  host: %q
  port: %d
  connect_timeout: 20ms
  response_timeout: 1s
  heartbeat_interval: 20ms
  probe_timeout: 50ms
  reconnect_min_delay: 10ms
  reconnect_max_delay: 50ms
queue:
  events_buffer: 16
  play_buffer: 16
background:
  animation: "matrix_rain_background"
  restore_on_idle: true
animations_file: %q
rules_file: %q
`, host, port, animationsPath, rulesPath)
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func loadFramePlaybackTestConfig(t *testing.T, matrixAddr string) config.Config {
	t.Helper()
	t.Setenv("MATRIX_PROXY_CONFIG", "")

	host, portText, err := net.SplitHostPort(matrixAddr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	animationsPath := filepath.Join(dir, "animations.yaml")
	if err := os.WriteFile(animationsPath, []byte(`
animations:
  orientation_badge:
    type: frames
    palette:
      ".": "#000000"
      A: "#110203"
      B: "#220506"
      C: "#330809"
      D: "#440B0C"
      E: "#550E0F"
      F: "#661112"
      G: "#771415"
      H: "#881718"
    frames:
      - delay: 1ms
        rows:
          - "A......B"
          - ".C....D."
          - "..E..F.."
          - "...GH..."
          - "H...G..."
          - "..F..E.."
          - ".D....C."
          - "B......A"
      - delay: 1ms
        rows:
          - "B......A"
          - "..D..C.."
          - ".F....E."
          - "G......H"
          - "A......B"
          - ".E....F."
          - "..C..D.."
          - "H......G"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	rulesPath := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(rulesPath, []byte("rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	data := fmt.Sprintf(`server:
  addr: "127.0.0.1:0"
  admin_token_env: ""
matrix:
  host: %q
  port: %d
  connect_timeout: 20ms
  response_timeout: 1s
  heartbeat_interval: 20ms
  probe_timeout: 50ms
  reconnect_min_delay: 10ms
  reconnect_max_delay: 50ms
queue:
  events_buffer: 16
  play_buffer: 16
animations_file: %q
rules_file: %q
`, host, port, animationsPath, rulesPath)
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func findRepoFile(t *testing.T, relativePath string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		path := filepath.Join(dir, relativePath)
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find %s from %s", relativePath, dir)
		}
		dir = parent
	}
}

func newNeverRunAppTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.AdminTokenEnv = ""
	cfg.Devices["default"].Host = "127.0.0.1"
	cfg.Devices["default"].Port = 1
	cfg.Queue.EventsBuffer = 16
	cfg.Queue.PlayBuffer = 16
	cfg.RulesFile = writeRulesFile(t)
	return cfg
}

func runAppWorkers(t *testing.T, application *app.App, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- application.RunWorkers(ctx)
	}()
	return done
}

func runApp(t *testing.T, application *app.App, ctx context.Context) <-chan error {
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
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop")
	}
}

func reserveTCPAddr(t *testing.T) string {
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

type blockingOutcomeLogHandler struct {
	entered     chan struct{}
	released    chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingOutcomeLogHandler() *blockingOutcomeLogHandler {
	return &blockingOutcomeLogHandler{
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (h *blockingOutcomeLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *blockingOutcomeLogHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "matrix item outcome" {
		h.enterOnce.Do(func() {
			close(h.entered)
		})
		<-h.released
	}
	return nil
}

func (h *blockingOutcomeLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *blockingOutcomeLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *blockingOutcomeLogHandler) release() {
	h.releaseOnce.Do(func() {
		close(h.released)
	})
}

func postJSON(t *testing.T, url, body string, want int) {
	t.Helper()
	if status := postJSONStatus(url, body); status != want {
		t.Fatalf("POST %s status = %d, want %d", url, status, want)
	}
}

func postJSONStatus(url, body string) int {
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

type httpPostResult struct {
	status int
	err    error
}

func postJSONStatusWithContext(ctx context.Context, url, body string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func assertHTTPPostStillBlocked(t *testing.T, statuses <-chan int) {
	t.Helper()
	select {
	case status := <-statuses:
		t.Fatalf("POST /events returned before subscriber buffer drained with status %d", status)
	case <-time.After(25 * time.Millisecond):
	}
}

func assertHTTPPostStillPending(t *testing.T, results <-chan httpPostResult) {
	t.Helper()
	select {
	case result := <-results:
		t.Fatalf("POST /events returned before request context timeout: status = %d, error = %v", result.status, result.err)
	case <-time.After(25 * time.Millisecond):
	}
}

func assertShutdownStillPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Shutdown() returned before blocked event worker was released: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
}

func waitForStatus(t *testing.T, url string, want int) {
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

func getReadyDetails(t *testing.T, baseURL string) (readyDetails, int) {
	t.Helper()
	data, status := getReadyBodyBytes(t, baseURL)
	var body readyDetails
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode /readyz response %q: %v", data, err)
	}
	return body, status
}

func getReadyBody(t *testing.T, baseURL string) string {
	t.Helper()
	data, _ := getReadyBodyBytes(t, baseURL)
	return string(data)
}

func getReadyBodyBytes(t *testing.T, baseURL string) ([]byte, int) {
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
	return data, resp.StatusCode
}

func waitCloseResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("Close() did not return")
		return nil
	}
}

func waitRunWorkersResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("RunWorkers() did not return")
		return nil
	}
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

func waitForQueueDepth(t *testing.T, baseURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last queueResponse
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/v1/devices/default/queue")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &last); err != nil {
			t.Fatalf("decode queue response %q: %v", data, err)
		}
		if last.Depth == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue depth = %d, want %d", last.Depth, want)
}

func waitForReadyObservabilityCallbackPanicsAtLeast(t *testing.T, baseURL string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	var lastStatus int
	for time.Now().Before(deadline) {
		last, lastStatus = getReadyDetails(t, baseURL)
		if last.ObservabilityCallbackPanics >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz observability_callback_panics = %d, want at least %d; status = %d; body = %#v",
		last.ObservabilityCallbackPanics, want, lastStatus, last)
}

func waitForReadyObservabilityCallbackCountAtLeast(t *testing.T, baseURL, callback string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	var lastStatus int
	for time.Now().Before(deadline) {
		last, lastStatus = getReadyDetails(t, baseURL)
		if last.ObservabilityCallbackCounts[callback] >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz observability callback %q count = %d, want at least %d; status = %d; counts = %v",
		callback, last.ObservabilityCallbackCounts[callback], want, lastStatus, last.ObservabilityCallbackCounts)
}

func waitForReadyTCPReconnectLogDropsAtLeast(t *testing.T, baseURL string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	var lastStatus int
	for time.Now().Before(deadline) {
		last, lastStatus = getReadyDetails(t, baseURL)
		if last.TCPReconnectLogEventsDropped >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz tcp_reconnect_log_events_dropped = %d, want at least %d; status = %d; body = %#v",
		last.TCPReconnectLogEventsDropped, want, lastStatus, last)
}

func waitForReadyEventWorker(t *testing.T, baseURL, wantState, wantStage string, wantDuration bool) readyDetails {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	var lastStatus int
	for time.Now().Before(deadline) {
		last, lastStatus = getReadyDetails(t, baseURL)
		durationPresent := last.EventWorker.ActiveDurationSeconds != nil
		if last.EventWorker.State == wantState &&
			last.EventWorker.Stage == wantStage &&
			durationPresent == wantDuration {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz event_worker = %#v, want state=%q stage=%q duration_present=%v; status = %d",
		last.EventWorker, wantState, wantStage, wantDuration, lastStatus)
	return readyDetails{}
}

func waitForReadyBackground(t *testing.T, baseURL, wantID, wantKind, wantState string) (readyDetails, int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	var lastStatus int
	for time.Now().Before(deadline) {
		last, lastStatus = getReadyDetails(t, baseURL)
		if last.DefaultDevice().Background.ConfiguredID == wantID &&
			last.DefaultDevice().Background.Kind == wantKind &&
			last.DefaultDevice().Background.State == wantState {
			return last, lastStatus
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz background = %#v, want id=%q kind=%q state=%q; status = %d; body = %#v",
		last.DefaultDevice().Background, wantID, wantKind, wantState, lastStatus, last)
	return readyDetails{}, 0
}

func waitForSchedulerState(t *testing.T, baseURL, want string) readyDetails {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	var lastStatus int
	for time.Now().Before(deadline) {
		last, lastStatus = getReadyDetails(t, baseURL)
		if last.DefaultDevice().SchedulerState == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz scheduler_state = %q, want %q; status = %d; body = %#v",
		last.DefaultDevice().SchedulerState, want, lastStatus, last)
	return readyDetails{}
}

func deleteQueue(t *testing.T, baseURL string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/devices/default/queue", nil)
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

func assertMetricHelpLine(t *testing.T, baseURL, metric string, parts ...string) {
	t.Helper()
	body := getMetrics(t, baseURL)
	prefix := "# HELP " + metric + " "
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		for _, part := range parts {
			if !strings.Contains(line, part) {
				t.Fatalf("metric help for %s missing %q in line:\n%s", metric, part, line)
			}
		}
		return
	}
	t.Fatalf("metrics missing help for %s in:\n%s", metric, body)
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

func assertNoRenderableBackgroundMetrics(t *testing.T, baseURL string) {
	t.Helper()
	for _, metric := range []string{
		"matrix_proxy_background_restore_attempts_total",
		"matrix_proxy_background_restore_failures_total",
		"matrix_proxy_background_dirty",
		"matrix_proxy_background_converged",
		"matrix_proxy_background_next_retry_seconds",
		"matrix_proxy_background_state",
	} {
		assertNoMetricLine(t, baseURL, metric, `kind="renderable"`)
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

func waitForGaugeValue(t *testing.T, baseURL, metric, want string) {
	t.Helper()
	waitForMetricLine(t, baseURL, metric, " "+want)
}

func waitForMetricValueAtLeast(t *testing.T, baseURL, metric string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var body string
	var last float64
	for time.Now().Before(deadline) {
		body = getMetrics(t, baseURL)
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, metric+" ") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatalf("parse metric value from %q: %v", line, err)
			}
			last = value
			if value >= want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("metric %s = %g, want at least %g in:\n%s", metric, last, want, body)
}

func assertGaugeValueRemains(t *testing.T, baseURL, metric string, want float64, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if got := currentMetricValue(t, baseURL, metric); got != want {
			t.Fatalf("metric %s = %g after cleanup, want it to remain %g", metric, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func currentMetricValue(t *testing.T, baseURL, metric string) float64 {
	t.Helper()
	body := getMetrics(t, baseURL)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric value from %q: %v", line, err)
		}
		return value
	}
	t.Fatalf("metrics missing %s in:\n%s", metric, body)
	return 0
}

func currentMetricLineValue(t *testing.T, baseURL, metric string, parts ...string) float64 {
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
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric value from %q: %v", line, err)
		}
		return value
	}
	t.Fatalf("metrics missing %s with %v in:\n%s", metric, parts, body)
	return 0
}

func waitForMetricLineValueAtLeast(t *testing.T, baseURL, metric string, want float64, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var body string
	var last float64
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
			if !matched {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatalf("parse metric value from %q: %v", line, err)
			}
			last = value
			if value >= want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("metric %s with %v = %g, want at least %g in:\n%s", metric, parts, last, want, body)
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

func waitForMatrixCommandSequenceUntil(t *testing.T, server *fakeESPServer, command byte) []recordedFrame {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var frames []recordedFrame
	for {
		select {
		case frame := <-server.frames:
			frames = append(frames, frame)
			if frame.Command == command {
				return frames
			}
		case <-deadline:
			t.Fatalf("timed out waiting for matrix command 0x%02x; saw commands %v", command, commandBytes(frames))
		}
	}
}

func waitForExactSetFramePayloads(t *testing.T, server *fakeESPServer, want [][]byte) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var commands []byte
	setFrameIndex := 0
	for setFrameIndex < len(want) {
		select {
		case frame := <-server.frames:
			commands = append(commands, frame.Command)
			switch frame.Command {
			case testCommandPing:
				continue
			case testCommandSetFrame:
				if len(frame.Payload) != testFramePayloadSize {
					t.Fatalf("SetFullFrame payload length = %d, want %d", len(frame.Payload), testFramePayloadSize)
				}
				if !bytes.Equal(frame.Payload, want[setFrameIndex]) {
					t.Fatalf("SetFullFrame payload %d mismatch\n got: %v\nwant: %v", setFrameIndex, frame.Payload, want[setFrameIndex])
				}
				setFrameIndex++
			case testCommandSetPreset:
				t.Fatalf("unexpected firmware preset command while waiting for generated background frames; commands %v", commands)
			default:
				t.Fatalf("unexpected matrix command 0x%02x while waiting for generated background frames; commands %v", frame.Command, commands)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %d generated background SetFullFrame commands; saw commands %v", len(want), commands)
		}
	}
}

func waitForGeneratedBackgroundAfterNotification(t *testing.T, server *fakeESPServer, backgroundPayloads [][]byte) int {
	t.Helper()
	deadline := time.After(4 * time.Second)
	var commands []byte
	var notificationSetFrames int
	backgroundIndex := 0
	for {
		select {
		case frame := <-server.frames:
			commands = append(commands, frame.Command)
			switch frame.Command {
			case testCommandPing:
				continue
			case testCommandSetFrame:
				if len(frame.Payload) != testFramePayloadSize {
					t.Fatalf("SetFullFrame payload length = %d, want %d", len(frame.Payload), testFramePayloadSize)
				}
				if bytes.Equal(frame.Payload, backgroundPayloads[backgroundIndex]) {
					backgroundIndex++
					if backgroundIndex == len(backgroundPayloads) {
						return notificationSetFrames
					}
					continue
				}
				if backgroundIndex > 0 {
					t.Fatalf("generated background SetFullFrame sequence interrupted at index %d by payload %v; commands %v",
						backgroundIndex, frame.Payload, commands)
				}
				if allZero(frame.Payload) {
					t.Fatal("notification SetFullFrame payload is all zero, want generated notification pixels")
				}
				if anyPayloadEqual(frame.Payload, backgroundPayloads) {
					t.Fatalf("notification SetFullFrame payload unexpectedly matched a generated background frame: %v", frame.Payload)
				}
				notificationSetFrames++
			case testCommandSetPreset:
				t.Fatalf("unexpected firmware preset command while waiting for generated background restore; commands %v", commands)
			default:
				t.Fatalf("unexpected matrix command 0x%02x while waiting for generated background restore; commands %v", frame.Command, commands)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for generated background restore after notification; saw commands %v", commands)
		}
	}
}

func drainMatrixFrames(server *fakeESPServer) {
	for {
		select {
		case <-server.frames:
		default:
			return
		}
	}
}

func assertNoBufferedSetFrame(t *testing.T, server *fakeESPServer) {
	t.Helper()
	var commands []byte
	for {
		select {
		case frame := <-server.frames:
			commands = append(commands, frame.Command)
			if frame.Command == testCommandSetFrame {
				t.Fatalf("unexpected buffered SetFullFrame after scheduler returned to idle; commands %v", commands)
			}
			if frame.Command == testCommandSetPreset {
				t.Fatalf("unexpected buffered SetPreset after generated background restore; commands %v", commands)
			}
		default:
			return
		}
	}
}

func assertMatrixRainPresetPayload(t *testing.T, payload []byte) {
	t.Helper()
	want := []byte{12, 90, 0, 0, 255, 85}
	if !bytes.Equal(payload, want) {
		t.Fatalf("matrix_rain_background preset payload = %v, want %v", payload, want)
	}
}

func generatedBackgroundFrameFixture(t *testing.T) []animations.Frame {
	t.Helper()
	frames := make([]animations.Frame, 2)
	for frameIndex := range frames {
		frame := animations.NewFrame(time.Millisecond)
		for y := 0; y < animations.CanvasHeight; y++ {
			for x := 0; x < animations.CanvasWidth; x++ {
				color := animations.RGB{
					R: byte(3 + frameIndex*80 + x*7 + y),
					G: byte(5 + frameIndex*80 + y*9 + x),
					B: byte(11 + frameIndex*80 + y*animations.CanvasWidth + x),
				}
				if err := frame.SetPixel(x, y, color); err != nil {
					t.Fatal(err)
				}
			}
		}
		frames[frameIndex] = frame
	}
	return frames
}

func expectedPackedPayloads(t *testing.T, cfg config.Config, frames []animations.Frame) [][]byte {
	t.Helper()
	layout, err := animations.NewLayout(
		cfg.Devices["default"].Layout.Width,
		cfg.Devices["default"].Layout.Height,
		cfg.Devices["default"].Layout.Wiring,
		cfg.Devices["default"].Layout.OddRowDisplayFlip,
		cfg.Devices["default"].Layout.Rotation,
	)
	if err != nil {
		t.Fatal(err)
	}
	packer, err := animations.NewPacker(layout)
	if err != nil {
		t.Fatal(err)
	}
	payloads := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		packed := packer.Pack(frame)
		payloads = append(payloads, append([]byte(nil), packed[:]...))
	}
	return payloads
}

func assertGeneratedBackgroundFixtureCatchesRawDisplayBypass(t *testing.T, frames []animations.Frame, packedPayloads [][]byte) {
	t.Helper()
	for i, frame := range frames {
		raw := rawDisplayPayload(frame)
		if bytes.Equal(raw, packedPayloads[i]) {
			t.Fatalf("generated background fixture frame %d raw display payload equals packed payload; test would not catch layout bypass", i)
		}
	}
}

func assertFramePlaybackFixtureCatchesUncompensatedLayout(t *testing.T, frames []animations.Frame, packedPayloads [][]byte) {
	t.Helper()
	uncompensatedLayout, err := animations.NewLayout(
		animations.CanvasWidth,
		animations.CanvasHeight,
		animations.WiringHorizontalTopLeft,
		false,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	uncompensatedPacker, err := animations.NewPacker(uncompensatedLayout)
	if err != nil {
		t.Fatal(err)
	}
	for i, frame := range frames {
		uncompensated := uncompensatedPacker.Pack(frame)
		if bytes.Equal(uncompensated[:], packedPayloads[i]) {
			t.Fatalf("frame playback fixture frame %d matches uncompensated h-tl packing; test would not catch missing odd-row display compensation", i)
		}
	}
}

func rawDisplayPayload(frame animations.Frame) []byte {
	payload := make([]byte, 0, testFramePayloadSize)
	for _, pixel := range frame.Pixels {
		payload = append(payload, pixel.R, pixel.G, pixel.B)
	}
	return payload
}

func anyPayloadEqual(payload []byte, candidates [][]byte) bool {
	for _, candidate := range candidates {
		if bytes.Equal(payload, candidate) {
			return true
		}
	}
	return false
}

func commandBytes(frames []recordedFrame) []byte {
	commands := make([]byte, 0, len(frames))
	for _, frame := range frames {
		commands = append(commands, frame.Command)
	}
	return commands
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func waitForMatrixResponse(t *testing.T, server *fakeESPServer, command byte) recordedFrame {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame := <-server.responses:
			if frame.Command == command {
				return frame
			}
		case <-deadline:
			t.Fatalf("timed out waiting for matrix response to command 0x%02x", command)
		}
	}
}

func waitForActiveMatrixConnections(t *testing.T, server *fakeESPServer, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last int
	for time.Now().Before(deadline) {
		last = server.ActiveConnections()
		if last == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active matrix connections = %d, want %d", last, want)
}

func waitForClosedMatrixConnections(t *testing.T, server *fakeESPServer, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last int
	for time.Now().Before(deadline) {
		last = server.ClosedConnections()
		if last == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("closed matrix connections = %d, want %d", last, want)
}

func playItemOutcomeMetrics(t *testing.T, baseURL, itemKind, item string) map[string]float64 {
	t.Helper()
	body := getMetrics(t, baseURL)
	outcomes := make(map[string]float64)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "matrix_proxy_play_items_total") {
			continue
		}
		if !strings.Contains(line, `item_kind="`+itemKind+`"`) || !strings.Contains(line, `item="`+item+`"`) {
			continue
		}
		outcome, ok := metricLabelValue(line, "outcome")
		if !ok {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric value from %q: %v", line, err)
		}
		outcomes[outcome] = value
	}
	return outcomes
}

func metricLabelValue(line, label string) (string, bool) {
	prefix := label + `="`
	start := strings.Index(line, prefix)
	if start < 0 {
		return "", false
	}
	start += len(prefix)
	end := strings.IndexByte(line[start:], '"')
	if end < 0 {
		return "", false
	}
	return line[start : start+end], true
}

func terminalOutcomeTotal(outcomes map[string]float64) float64 {
	var total float64
	for _, outcome := range []string{
		"executed",
		"expired",
		"canceled",
		"dropped",
		"queue_cleared",
		"scheduler_stopped",
		"permanent_error",
	} {
		total += outcomes[outcome]
	}
	return total
}

type queueResponse struct {
	Depth int `json:"depth"`
}

type queueClearResponse struct {
	Cleared int `json:"cleared"`
}

type readyDetails struct {
	Status         string                      `json:"status"`
	WorkersRunning bool                        `json:"workers_running"`
	Draining       bool                        `json:"draining"`
	EventWorker    readyEventWorker            `json:"event_worker"`
	Devices        map[string]deviceReadyEntry `json:"devices"`
	// Aggregate diagnostic fields.
	TCPReconnectLogEventsDropped uint64            `json:"tcp_reconnect_log_events_dropped"`
	ObservabilityCallbackPanics  uint64            `json:"observability_callback_panics"`
	ObservabilityCallbackCounts  map[string]uint64 `json:"observability_callback_panic_counts"`
}

func (r readyDetails) DefaultDevice() deviceReadyEntry {
	return r.Devices["default"]
}

type deviceReadyEntry struct {
	SchedulerState  string        `json:"scheduler_state"`
	MatrixConnected bool          `json:"matrix_connected"`
	Background      readyBackground `json:"background"`
	LastSuccess     *time.Time    `json:"last_success"`
	LastFailure     *time.Time    `json:"last_failure"`
}

type readyBackground struct {
	ConfiguredID   string     `json:"configured_id"`
	Kind           string     `json:"kind"`
	State          string     `json:"state"`
	Dirty          bool       `json:"dirty"`
	Converged      bool       `json:"converged"`
	LastAttempt    *time.Time `json:"last_attempt"`
	LastSuccess    *time.Time `json:"last_success"`
	NextRetry      *time.Time `json:"next_retry"`
	FailureCount   int        `json:"failure_count"`
	LastError      string     `json:"last_error"`
	LastErrorClass string     `json:"last_error_class"`
}

type readyEventWorker struct {
	State                 string   `json:"state"`
	Stage                 string   `json:"stage"`
	ActiveDurationSeconds *float64 `json:"active_duration_seconds"`
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
	listener  net.Listener
	frames    chan recordedFrame
	responses chan recordedFrame

	mu                   sync.Mutex
	closed               bool
	conns                map[net.Conn]struct{}
	closedConnections    int
	statuses             map[byte][]byte
	forcedStatuses       map[byte]byte
	pauseResponseArmed   bool
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
	return newFakeESPServerAt(t, "127.0.0.1:0")
}

func newFakeESPServerAt(t *testing.T, addr string) *fakeESPServer {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeESPServer{
		listener:       listener,
		frames:         make(chan recordedFrame, 64),
		responses:      make(chan recordedFrame, 64),
		conns:          make(map[net.Conn]struct{}),
		statuses:       make(map[byte][]byte),
		forcedStatuses: make(map[byte]byte),
	}
	go server.serve()
	return server
}

type panickingLogHandler struct {
	message string
}

func (h *panickingLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *panickingLogHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == h.message {
		panic("test observability callback panic")
	}
	return nil
}

func (h *panickingLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *panickingLogHandler) WithGroup(string) slog.Handler {
	return h
}

type blockingMessageLogHandler struct {
	message     string
	entered     chan struct{}
	released    chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingMessageLogHandler(message string) *blockingMessageLogHandler {
	return &blockingMessageLogHandler{
		message:  message,
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (h *blockingMessageLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *blockingMessageLogHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == h.message {
		h.enterOnce.Do(func() {
			close(h.entered)
		})
		<-h.released
	}
	return nil
}

func (h *blockingMessageLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *blockingMessageLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *blockingMessageLogHandler) release() {
	h.releaseOnce.Do(func() {
		close(h.released)
	})
}

func (s *fakeESPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *fakeESPServer) ActiveConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *fakeESPServer) ClosedConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedConnections
}

func (s *fakeESPServer) CloseActiveConnections() {
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

func (s *fakeESPServer) FailNextStatus(command, status byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[command] = append(s.statuses[command], status)
}

func (s *fakeESPServer) FailCommandStatus(command, status byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forcedStatuses[command] = status
}

func (s *fakeESPServer) RecoverCommandStatus(command byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.forcedStatuses, command)
}

func (s *fakeESPServer) PauseNextFrameResponse() <-chan struct{} {
	return s.PauseNextCommandResponse(testCommandSetFrame)
}

func (s *fakeESPServer) PauseNextCommandResponse(command byte) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pauseResponseArmed = true
	s.pauseResponseCommand = command
	s.pauseResponse = make(chan struct{})
	s.pausedResponse = make(chan struct{})
	return s.pausedResponse
}

func (s *fakeESPServer) ResumePausedFrameResponse() {
	s.ResumePausedResponse()
}

func (s *fakeESPServer) ResumePausedResponse() {
	s.mu.Lock()
	ch := s.pauseResponse
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
		s.closedConnections++
		s.mu.Unlock()
		_ = conn.Close()
	}()

	reader := bufio.NewReader(conn)
	for {
		frame, err := readMatrixFrame(reader, conn)
		if err != nil {
			return
		}
		s.frames <- frame

		if pause := s.pauseBeforeResponse(frame); pause != nil {
			<-pause
			s.clearPausedResponse(pause)
		}

		response := []byte{testMagic0, testMagic1, testProtocolVersion, testResponseCommand, s.nextStatus(frame.Command)}
		response = append(response, xorChecksum(response))
		if _, err := conn.Write(response); err != nil {
			return
		}
		select {
		case s.responses <- frame:
		default:
		}
	}
}

func (s *fakeESPServer) nextStatus(command byte) byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status, ok := s.forcedStatuses[command]; ok {
		return status
	}
	statuses := s.statuses[command]
	if len(statuses) == 0 {
		return testStatusOK
	}
	status := statuses[0]
	s.statuses[command] = statuses[1:]
	return status
}

func (s *fakeESPServer) pauseBeforeResponse(frame recordedFrame) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pauseResponseArmed || frame.Command != s.pauseResponseCommand || s.pauseResponse == nil {
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
		s.pauseResponseArmed = false
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
