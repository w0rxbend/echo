package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestClientFakeTCPServerContract(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	client := newTestClient(t, server.Addr())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Ping(ctx)
	}()

	frame := server.ExpectFrame(t)
	if frame.Command != byte(commandPing) {
		t.Fatalf("command = 0x%02x, want ping", frame.Command)
	}
	if len(frame.Payload) != 0 {
		t.Fatalf("payload length = %d, want 0", len(frame.Payload))
	}
	server.RespondOK()

	if err := <-errCh; err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	server.WaitForResponseWrites(t, 1)
}

func TestClientCommandPayloads(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	client := newTestClient(t, server.Addr())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	run := func(name string, call func() error) recordedFrame {
		t.Helper()
		errCh := make(chan error, 1)
		go func() {
			errCh <- call()
		}()
		frame := server.ExpectFrame(t)
		server.RespondOK()
		if err := <-errCh; err != nil {
			t.Fatalf("%s error = %v", name, err)
		}
		return frame
	}

	frame := run("Ping", func() error { return client.Ping(ctx) })
	assertFrame(t, frame, commandPing, nil)

	frame = run("Clear", func() error { return client.Clear(ctx) })
	assertFrame(t, frame, commandClear, nil)

	frame = run("SetBrightness", func() error { return client.SetBrightness(ctx, 42) })
	assertFrame(t, frame, commandSetBrightness, []byte{42})

	color := RGB{R: 1, G: 2, B: 3}
	frame = run("Fill", func() error { return client.Fill(ctx, color) })
	assertFrame(t, frame, commandFill, []byte{1, 2, 3})

	frame = run("SetPixel", func() error { return client.SetPixel(ctx, 4, 5, color) })
	assertFrame(t, frame, commandSetPixel, []byte{4, 5, 1, 2, 3})

	packed := PackedFrame{}
	for i := range packed {
		packed[i] = byte(i)
	}
	frame = run("SetFrame", func() error { return client.SetFrame(ctx, packed) })
	assertFrame(t, frame, commandSetFrame, packed[:])

	frame = run("SetPanelEnabled(false)", func() error { return client.SetPanelEnabled(ctx, false) })
	assertFrame(t, frame, commandSetPanelEnabled, []byte{0})

	frame = run("SetPanelEnabled(true)", func() error { return client.SetPanelEnabled(ctx, true) })
	assertFrame(t, frame, commandSetPanelEnabled, []byte{1})

	frame = run("SetStaticColor", func() error { return client.SetStaticColor(ctx, color) })
	assertFrame(t, frame, commandSetStaticColor, []byte{1, 2, 3})

	frame = run("SetPreset", func() error { return client.SetPreset(ctx, 12, 513*time.Millisecond, color) })
	assertFrame(t, frame, commandSetPresetEffect, []byte{12, 1, 2, 1, 2, 3})

	frame = run("UploadCustomFrame", func() error {
		return client.UploadCustomFrame(ctx, 2, 8, 1025*time.Millisecond, packed)
	})
	wantUpload := make([]byte, customFramePayloadSize)
	wantUpload[0] = 2
	wantUpload[1] = 8
	wantUpload[2] = 1
	wantUpload[3] = 4
	copy(wantUpload[4:], packed[:])
	assertFrame(t, frame, commandUploadCustomFrame, wantUpload)

	frame = run("StopEffect", func() error { return client.StopEffect(ctx) })
	assertFrame(t, frame, commandStopEffect, nil)
}

func TestClientReturnsTypedStatusError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	client := newTestClient(t, server.Addr())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Ping(ctx)
	}()
	_ = server.ExpectFrame(t)
	server.Respond(StatusInvalidLength)

	err := <-errCh
	if !errors.Is(err, ErrStatusInvalidLength) {
		t.Fatalf("Ping() error = %v, want ErrStatusInvalidLength", err)
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("Ping() error = %T, want *StatusError", err)
	}
}

func TestClientCommandDoneReportsSuccessfulCommandAttempt(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)
	recorder.Reset()

	errCh := make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() error = %v", err)
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandClear, StatusOK.Label()})
}

func TestClientCommandDoneReportsFirmwareStatusAttempt(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)
	recorder.Reset()

	errCh := make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.Respond(StatusInvalidLength)
	if err := <-errCh; !errors.Is(err, ErrStatusInvalidLength) {
		t.Fatalf("Clear() error = %v, want ErrStatusInvalidLength", err)
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandClear, StatusInvalidLength.Label()})
}

func TestClientCommandDoneReportsResponseValidationAttempt(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)
	recorder.Reset()

	errCh := make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	response := testResponse(StatusOK)
	response[5] ^= 0xff
	server.RespondRaw(response)
	if err := <-errCh; !errors.Is(err, ErrProtocol) {
		t.Fatalf("Clear() error = %v, want ErrProtocol", err)
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandClear, "protocol_error"})
}

func TestClientCommandDoneReportsTransportAttempt(t *testing.T) {
	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         closedListenerAddr(t),
		ConnectTimeout:  100 * time.Millisecond,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()
	client.conn = errorConn{writeErr: io.ErrUnexpectedEOF}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Clear(ctx); err == nil {
		t.Fatal("Clear() error = nil, want transport error")
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandClear, "transport_error"})
}

func TestClientCommandDoneReportsContextCanceledAttempt(t *testing.T) {
	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         closedListenerAddr(t),
		ConnectTimeout:  100 * time.Millisecond,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()
	client.conn = errorConn{writeErr: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Clear(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Clear() error = %v, want context.Canceled", err)
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandClear, "canceled"})
}

func TestClientCommandDoneReportsContextDeadlineAttempt(t *testing.T) {
	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         closedListenerAddr(t),
		ConnectTimeout:  100 * time.Millisecond,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()
	client.conn = errorConn{writeErr: context.DeadlineExceeded}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	if err := client.Clear(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Clear() error = %v, want context.DeadlineExceeded", err)
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandClear, "deadline_exceeded"})
}

func TestClientCommandDoneReportsPingHeartbeatAttempt(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Ping() heartbeat error = %v", err)
	}

	assertCommandResults(t, recorder.Snapshot(), commandResultWant{commandPing, StatusOK.Label()})
}

func TestClientCommandDoneReportsRetrySuccessFrameAttempts(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder commandResultRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone:   recorder.Record,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)
	recorder.Reset()

	errCh := make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() after reconnect error = %v", err)
	}

	assertCommandResults(t, recorder.Snapshot(),
		commandResultWant{commandClear, "transport_error"},
		commandResultWant{commandPing, StatusOK.Label()},
		commandResultWant{commandClear, StatusOK.Label()},
	)
}

func TestClientCommandDoneCallbackRunsWhileSerializationHeld(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	callbackEntered := make(chan CommandResult, 1)
	releaseCallback := make(chan struct{})
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone: func(result CommandResult) {
			if result.Command != commandPing.String() || result.Status != StatusOK.Label() {
				return
			}
			callbackEntered <- result
			<-releaseCallback
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pingErrCh := make(chan error, 1)
	go func() { pingErrCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()

	select {
	case result := <-callbackEntered:
		if result.Command != commandPing.String() || result.Status != StatusOK.Label() {
			t.Fatalf("callback result = %+v, want successful ping", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command done callback")
	}

	clearErrCh := make(chan error, 1)
	go func() { clearErrCh <- client.Clear(ctx) }()

	select {
	case err := <-clearErrCh:
		t.Fatalf("Clear() completed while command done callback was blocked: %v", err)
	case frame := <-server.frames:
		t.Fatalf("received command 0x%02x while command done callback was blocked", frame.Command)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseCallback)
	if err := <-pingErrCh; err != nil {
		t.Fatalf("Ping() error = %v", err)
	}

	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-clearErrCh; err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
}

func TestClientReconnectsAfterSocketError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	client := newTestClient(t, server.Addr())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Ping(ctx)
	}()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() {
		errCh <- client.Clear(ctx)
	}()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()

	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()

	if err := <-errCh; err != nil {
		t.Fatalf("Clear() after reconnect error = %v", err)
	}
}

func TestClientImmediateReconnectObservationSuccess(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var mu sync.Mutex
	var attempts []ReconnectAttempt
	var recoveries []ReconnectRecovery
	var commandResults []CommandResult
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectAttempt: func(attempt ReconnectAttempt) {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, attempt)
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			mu.Lock()
			defer mu.Unlock()
			recoveries = append(recoveries, recovery)
		},
		OnCommandDone: func(result CommandResult) {
			mu.Lock()
			defer mu.Unlock()
			commandResults = append(commandResults, result)
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Ping(ctx)
	}()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() {
		errCh <- client.Clear(ctx)
	}()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() after reconnect error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 1 {
		t.Fatalf("reconnect attempts = %d, want 1", len(attempts))
	}
	if attempts[0].Source != ReconnectSourceTCPImmediate || attempts[0].Attempt != 1 ||
		attempts[0].ErrorKind != ErrorKindRetryable || attempts[0].Error == "" {
		t.Fatalf("reconnect attempt = %+v, want tcp immediate retryable attempt", attempts[0])
	}
	if len(recoveries) != 1 {
		t.Fatalf("reconnect recoveries = %d, want 1", len(recoveries))
	}
	if recoveries[0] != (ReconnectRecovery{Source: ReconnectSourceTCPImmediate, Attempt: 1, State: StateReady}) {
		t.Fatalf("reconnect recovery = %+v, want tcp immediate ready recovery", recoveries[0])
	}
	if len(commandResults) != 4 {
		t.Fatalf("command results = %d, want 4", len(commandResults))
	}
	if commandResults[1].Command != commandClear.String() || commandResults[1].Status != "transport_error" {
		t.Fatalf("first clear command result = %+v, want transport error", commandResults[1])
	}
	if commandResults[3].Command != commandClear.String() || commandResults[3].Status != StatusOK.Label() {
		t.Fatalf("retried clear command result = %+v, want ok", commandResults[3])
	}
}

func TestClientImmediateReconnectRecoveryThenFirmwareStatusError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var mu sync.Mutex
	var attempts []ReconnectAttempt
	var recoveries []ReconnectRecovery
	var failures []ReconnectFailure
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectAttempt: func(attempt ReconnectAttempt) {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, attempt)
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			mu.Lock()
			defer mu.Unlock()
			recoveries = append(recoveries, recovery)
		},
		OnReconnectFailure: func(failure ReconnectFailure) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, failure)
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.Respond(StatusInvalidLength)
	if err := <-errCh; !errors.Is(err, ErrStatusInvalidLength) {
		t.Fatalf("Clear() error = %v, want ErrStatusInvalidLength", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 1 {
		t.Fatalf("reconnect attempts = %d, want 1", len(attempts))
	}
	if len(recoveries) != 1 {
		t.Fatalf("reconnect recoveries = %d, want 1", len(recoveries))
	}
	if recoveries[0] != (ReconnectRecovery{Source: ReconnectSourceTCPImmediate, Attempt: 1, State: StateReady}) {
		t.Fatalf("reconnect recovery = %+v, want tcp immediate ready recovery", recoveries[0])
	}
	if len(failures) != 0 {
		t.Fatalf("reconnect failures = %d, want 0 after status error on retry: %+v", len(failures), failures)
	}
}

func TestClientImmediateReconnectRecoveryThenProtocolError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var mu sync.Mutex
	var recoveries []ReconnectRecovery
	var failures []ReconnectFailure
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			mu.Lock()
			defer mu.Unlock()
			recoveries = append(recoveries, recovery)
		},
		OnReconnectFailure: func(failure ReconnectFailure) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, failure)
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	response := testResponse(StatusOK)
	response[3] = 0
	response[5] = checksum(response[:5])
	server.RespondRaw(response)
	if err := <-errCh; !errors.Is(err, ErrProtocol) {
		t.Fatalf("Clear() error = %v, want ErrProtocol", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recoveries) != 1 {
		t.Fatalf("reconnect recoveries = %d, want 1", len(recoveries))
	}
	if recoveries[0] != (ReconnectRecovery{Source: ReconnectSourceTCPImmediate, Attempt: 1, State: StateReady}) {
		t.Fatalf("reconnect recovery = %+v, want tcp immediate ready recovery", recoveries[0])
	}
	if len(failures) != 0 {
		t.Fatalf("reconnect failures = %d, want 0 after protocol error on retry: %+v", len(failures), failures)
	}
}

func TestClientPingImmediateReconnectRecoveryAfterValidRetryPing(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder reconnectObservationRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:              server.Addr(),
		ConnectTimeout:       time.Second,
		ResponseTimeout:      time.Second,
		OnReconnectAttempt:   recorder.RecordAttempt,
		OnReconnectRecovered: recorder.RecordRecovery,
		OnReconnectFailure:   recorder.RecordFailure,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Ping() after reconnect error = %v", err)
	}

	attempts, recoveries, failures := recorder.Snapshot()
	assertSingleTCPImmediateAttempt(t, attempts)
	if len(recoveries) != 1 {
		t.Fatalf("reconnect recoveries = %d, want 1", len(recoveries))
	}
	if recoveries[0] != (ReconnectRecovery{Source: ReconnectSourceTCPImmediate, Attempt: 1, State: StateReady}) {
		t.Fatalf("reconnect recovery = %+v, want tcp immediate ready recovery", recoveries[0])
	}
	if len(failures) != 0 {
		t.Fatalf("reconnect failures = %d, want 0: %+v", len(failures), failures)
	}
}

func TestClientPingImmediateReconnectNoRecoveryOnFirmwareStatusError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder reconnectObservationRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:              server.Addr(),
		ConnectTimeout:       time.Second,
		ResponseTimeout:      time.Second,
		OnReconnectAttempt:   recorder.RecordAttempt,
		OnReconnectRecovered: recorder.RecordRecovery,
		OnReconnectFailure:   recorder.RecordFailure,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.Respond(StatusInvalidLength)
	if err := <-errCh; !errors.Is(err, ErrStatusInvalidLength) {
		t.Fatalf("Ping() error = %v, want ErrStatusInvalidLength", err)
	}

	errCh = make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() after failed ping verification error = %v", err)
	}

	attempts, recoveries, failures := recorder.Snapshot()
	assertSingleTCPImmediateAttempt(t, attempts)
	if len(recoveries) != 0 {
		t.Fatalf("reconnect recoveries = %d, want 0: %+v", len(recoveries), recoveries)
	}
	if len(failures) != 1 {
		t.Fatalf("reconnect failures = %d, want 1: %+v", len(failures), failures)
	}
	if failures[0].Source != ReconnectSourceTCPImmediate || failures[0].Attempt != 1 ||
		failures[0].ErrorKind != ErrorKindPermanent || failures[0].Outcome != ReconnectFailureVerificationFailed ||
		failures[0].Error == "" {
		t.Fatalf("reconnect failure = %+v, want permanent verification failure", failures[0])
	}
}

func TestClientPingImmediateReconnectNoRecoveryOnProtocolError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder reconnectObservationRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:              server.Addr(),
		ConnectTimeout:       time.Second,
		ResponseTimeout:      time.Second,
		OnReconnectAttempt:   recorder.RecordAttempt,
		OnReconnectRecovered: recorder.RecordRecovery,
		OnReconnectFailure:   recorder.RecordFailure,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	response := testResponse(StatusOK)
	response[3] = 0
	response[5] = checksum(response[:5])
	server.RespondRaw(response)
	if err := <-errCh; !errors.Is(err, ErrProtocol) {
		t.Fatalf("Ping() error = %v, want ErrProtocol", err)
	}

	errCh = make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() after failed ping verification error = %v", err)
	}

	attempts, recoveries, failures := recorder.Snapshot()
	assertSingleTCPImmediateAttempt(t, attempts)
	if len(recoveries) != 0 {
		t.Fatalf("reconnect recoveries = %d, want 0: %+v", len(recoveries), recoveries)
	}
	if len(failures) != 1 {
		t.Fatalf("reconnect failures = %d, want 1: %+v", len(failures), failures)
	}
	if failures[0].Source != ReconnectSourceTCPImmediate || failures[0].Attempt != 1 ||
		failures[0].ErrorKind != ErrorKindPermanent || failures[0].Outcome != ReconnectFailureVerificationFailed ||
		failures[0].Error == "" {
		t.Fatalf("reconnect failure = %+v, want permanent verification failure", failures[0])
	}
}

func TestClientPingImmediateReconnectFailureOnSecondTransportError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recorder reconnectObservationRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:              server.Addr(),
		ConnectTimeout:       time.Second,
		ResponseTimeout:      time.Second,
		OnReconnectAttempt:   recorder.RecordAttempt,
		OnReconnectRecovered: recorder.RecordRecovery,
		OnReconnectFailure:   recorder.RecordFailure,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	establishTestConnection(t, client, server, ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.CloseConnection()
	if err := <-errCh; err == nil {
		t.Fatal("Ping() error = nil, want reconnect failure")
	}

	attempts, recoveries, failures := recorder.Snapshot()
	assertSingleTCPImmediateAttempt(t, attempts)
	if len(recoveries) != 0 {
		t.Fatalf("reconnect recoveries = %d, want 0: %+v", len(recoveries), recoveries)
	}
	if len(failures) != 1 {
		t.Fatalf("reconnect failures = %d, want 1: %+v", len(failures), failures)
	}
	if failures[0].Source != ReconnectSourceTCPImmediate || failures[0].Attempt != 1 ||
		failures[0].ErrorKind != ErrorKindRetryable || failures[0].Outcome != ReconnectFailureFailed ||
		failures[0].Error == "" {
		t.Fatalf("reconnect failure = %+v, want retryable failed tcp immediate failure", failures[0])
	}
}

func TestClientDoesNotReconnectAfterProtocolError(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	client := newTestClient(t, server.Addr())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Ping(ctx)
	}()

	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	response := testResponse(StatusOK)
	response[0] = 0
	response[5] = checksum(response[:5])
	server.RespondRaw(response)

	err := <-errCh
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Ping() error = %v, want ErrProtocol", err)
	}
	if !IsPermanentError(ctx, err) {
		t.Fatalf("Ping() error = %v, want permanent classification", err)
	}

	select {
	case frame := <-server.frames:
		t.Fatalf("received retry command 0x%02x after protocol error", frame.Command)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientDoesNotObserveReconnectAfterPermanentErrors(t *testing.T) {
	t.Run("protocol", func(t *testing.T) {
		server := newScriptedMatrixServer(t)
		defer server.Close()

		var recorder reconnectObservationRecorder
		client := newTestClientWithOptions(t, ClientOptions{
			Address:              server.Addr(),
			ConnectTimeout:       time.Second,
			ResponseTimeout:      time.Second,
			OnReconnectAttempt:   recorder.RecordAttempt,
			OnReconnectRecovered: recorder.RecordRecovery,
			OnReconnectFailure:   recorder.RecordFailure,
		})
		defer client.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- client.Ping(ctx) }()
		assertFrame(t, server.ExpectFrame(t), commandPing, nil)
		response := testResponse(StatusOK)
		response[0] = 0
		response[5] = checksum(response[:5])
		server.RespondRaw(response)
		if err := <-errCh; !errors.Is(err, ErrProtocol) {
			t.Fatalf("Ping() error = %v, want ErrProtocol", err)
		}
		assertNoReconnectObservations(t, &recorder, "protocol error")
	})

	t.Run("status", func(t *testing.T) {
		server := newScriptedMatrixServer(t)
		defer server.Close()

		var recorder reconnectObservationRecorder
		client := newTestClientWithOptions(t, ClientOptions{
			Address:              server.Addr(),
			ConnectTimeout:       time.Second,
			ResponseTimeout:      time.Second,
			OnReconnectAttempt:   recorder.RecordAttempt,
			OnReconnectRecovered: recorder.RecordRecovery,
			OnReconnectFailure:   recorder.RecordFailure,
		})
		defer client.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- client.Ping(ctx) }()
		assertFrame(t, server.ExpectFrame(t), commandPing, nil)
		server.Respond(StatusInvalidLength)
		if err := <-errCh; !errors.Is(err, ErrStatusInvalidLength) {
			t.Fatalf("Ping() error = %v, want ErrStatusInvalidLength", err)
		}
		assertNoReconnectObservations(t, &recorder, "status error")
	})
}

func TestClientReconnectObservationCallbackPanicsRecovered(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var recovered bool
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectAttempt: func(ReconnectAttempt) {
			panic("attempt observer failed")
		},
		OnReconnectRecovered: func(ReconnectRecovery) {
			recovered = true
			panic("recovery observer failed")
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if !recovered {
		t.Fatal("reconnect recovery callback was not invoked")
	}
	counts := client.ObservabilityCallbackPanicCounts()
	if got := counts[ObservabilityCallbackReconnectAttempt]; got != 1 {
		t.Fatalf("reconnect attempt panic count = %d, want 1; counts = %v", got, counts)
	}
	if got := counts[ObservabilityCallbackReconnectRecovered]; got != 1 {
		t.Fatalf("reconnect recovered panic count = %d, want 1; counts = %v", got, counts)
	}
	if got := client.ObservabilityCallbackPanics(); got != 2 {
		t.Fatalf("ObservabilityCallbackPanics() = %d, want 2", got)
	}
}

func TestClientCommandDoneCallbackPanicCountedUnitScoped(t *testing.T) {
	// App construction intentionally exposes no hook for replacing the command
	// metrics callback, so command_done panic accounting is covered at the TCP
	// client layer where the callback is injected.
	server := newScriptedMatrixServer(t)
	defer server.Close()

	results := make(chan CommandResult, 2)
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnCommandDone: func(result CommandResult) {
			results <- result
			panic("command observer failed")
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 1)

	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	result := receiveCommandResult(t, results)
	if result.Command != commandPing.String() || result.Status != StatusOK.Label() {
		t.Fatalf("ping callback result = %+v, want command=%q status=%q",
			result, commandPing.String(), StatusOK.Label())
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.Respond(StatusInvalidLength)
	if err := <-errCh; !errors.Is(err, ErrStatusInvalidLength) {
		t.Fatalf("Clear() error = %v, want ErrStatusInvalidLength", err)
	}
	result = receiveCommandResult(t, results)
	if result.Command != commandClear.String() || result.Status != StatusInvalidLength.Label() {
		t.Fatalf("clear callback result = %+v, want command=%q status=%q",
			result, commandClear.String(), StatusInvalidLength.Label())
	}

	counts := client.ObservabilityCallbackPanicCounts()
	if got := counts[ObservabilityCallbackCommandDone]; got != 2 {
		t.Fatalf("command done panic count = %d, want 2; counts = %v", got, counts)
	}
	if got := client.ObservabilityCallbackPanics(); got != 2 {
		t.Fatalf("ObservabilityCallbackPanics() = %d, want 2", got)
	}
}

func TestClientReconnectFailureCallbackPanicCounted(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var cancel context.CancelFunc
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectAttempt: func(ReconnectAttempt) {
			cancel()
		},
		OnReconnectFailure: func(ReconnectFailure) {
			panic("failure observer failed")
		},
	})
	defer client.Close()

	ctx, cancelFunc := context.WithCancel(context.Background())
	cancel = cancelFunc
	defer cancelFunc()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	if err := <-errCh; err == nil {
		t.Fatal("Clear() error = nil, want retried command transport failure")
	}

	counts := client.ObservabilityCallbackPanicCounts()
	if got := counts[ObservabilityCallbackReconnectFailure]; got != 1 {
		t.Fatalf("reconnect failure panic count = %d, want 1; counts = %v", got, counts)
	}
	if got := client.ObservabilityCallbackPanics(); got != 1 {
		t.Fatalf("ObservabilityCallbackPanics() = %d, want 1", got)
	}
}

func TestClientImmediateReconnectObservationFailure(t *testing.T) {
	server := newScriptedMatrixServer(t)

	var mu sync.Mutex
	var attempts []ReconnectAttempt
	var recoveries []ReconnectRecovery
	var failures []ReconnectFailure
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectAttempt: func(attempt ReconnectAttempt) {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, attempt)
		},
		OnReconnectFailure: func(failure ReconnectFailure) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, failure)
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			mu.Lock()
			defer mu.Unlock()
			recoveries = append(recoveries, recovery)
		},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	if err := <-errCh; err == nil {
		t.Fatal("Clear() error = nil, want reconnect failure")
	}
	server.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 1 {
		t.Fatalf("reconnect attempts = %d, want 1", len(attempts))
	}
	if len(recoveries) != 1 {
		t.Fatalf("reconnect recoveries = %d, want 1", len(recoveries))
	}
	if recoveries[0] != (ReconnectRecovery{Source: ReconnectSourceTCPImmediate, Attempt: 1, State: StateReady}) {
		t.Fatalf("reconnect recovery = %+v, want tcp immediate ready recovery", recoveries[0])
	}
	if len(failures) != 0 {
		t.Fatalf("reconnect failures = %d, want 0 after verified recovery: %+v", len(failures), failures)
	}
}

func TestClientImmediateReconnectFailureOnContextCancellationBeforeReconnect(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var cancel context.CancelFunc
	var mu sync.Mutex
	var recoveries []ReconnectRecovery
	var failures []ReconnectFailure
	client := newTestClientWithOptions(t, ClientOptions{
		Address:         server.Addr(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		OnReconnectAttempt: func(ReconnectAttempt) {
			cancel()
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			mu.Lock()
			defer mu.Unlock()
			recoveries = append(recoveries, recovery)
		},
		OnReconnectFailure: func(failure ReconnectFailure) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, failure)
		},
	})
	defer client.Close()

	ctx, cancelFunc := context.WithCancel(context.Background())
	cancel = cancelFunc
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Clear() error = %v, want context.Canceled", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recoveries) != 0 {
		t.Fatalf("reconnect recoveries = %d, want 0 after cancellation: %+v", len(recoveries), recoveries)
	}
	if len(failures) != 1 {
		t.Fatalf("reconnect failures = %d, want 1", len(failures))
	}
	if failures[0].Source != ReconnectSourceTCPImmediate || failures[0].Attempt != 1 ||
		failures[0].ErrorKind != ErrorKindPermanent || failures[0].Outcome != ReconnectFailureCanceled {
		t.Fatalf("reconnect failure = %+v, want canceled tcp immediate failure", failures[0])
	}
}

func TestClientImmediateReconnectCancellationAfterVerificationIsRequestCancellation(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	var cancel context.CancelFunc
	var recorder reconnectObservationRecorder
	client := newTestClientWithOptions(t, ClientOptions{
		Address:            server.Addr(),
		ConnectTimeout:     time.Second,
		ResponseTimeout:    time.Second,
		OnReconnectAttempt: recorder.RecordAttempt,
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			recorder.RecordRecovery(recovery)
			cancel()
		},
		OnReconnectFailure: recorder.RecordFailure,
	})
	defer client.Close()

	ctx, cancelFunc := context.WithCancel(context.Background())
	cancel = cancelFunc
	defer cancelFunc()
	establishTestConnection(t, client, server, ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- client.Clear(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)
	server.CloseConnection()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Clear() error = %v, want context.Canceled", err)
	}

	attempts, recoveries, failures := recorder.Snapshot()
	assertSingleTCPImmediateAttempt(t, attempts)
	if len(recoveries) != 1 {
		t.Fatalf("reconnect recoveries = %d, want 1", len(recoveries))
	}
	if recoveries[0] != (ReconnectRecovery{Source: ReconnectSourceTCPImmediate, Attempt: 1, State: StateReady}) {
		t.Fatalf("reconnect recovery = %+v, want tcp immediate ready recovery", recoveries[0])
	}
	if len(failures) != 0 {
		t.Fatalf("reconnect failures = %d, want 0 after verified cancellation: %+v", len(failures), failures)
	}
}

func TestClientSerializesCommands(t *testing.T) {
	server := newScriptedMatrixServer(t)
	defer server.Close()

	client := newTestClient(t, server.Addr())
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 2)
	go func() {
		errCh <- client.Ping(ctx)
	}()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}

	go func() {
		errCh <- client.Clear(ctx)
	}()
	assertFrame(t, server.ExpectFrame(t), commandClear, nil)

	go func() {
		errCh <- client.Fill(ctx, RGB{R: 9, G: 8, B: 7})
	}()

	select {
	case frame := <-server.frames:
		t.Fatalf("received command 0x%02x while first command had no response", frame.Command)
	case <-time.After(100 * time.Millisecond):
	}

	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Clear() error = %v", err)
	}

	assertFrame(t, server.ExpectFrame(t), commandFill, []byte{9, 8, 7})
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("Fill() error = %v", err)
	}
}

func TestDurationValidation(t *testing.T) {
	if _, err := durationMilliseconds(-time.Millisecond, "test"); err == nil {
		t.Fatal("durationMilliseconds(negative) error = nil, want error")
	}
	if _, err := durationMilliseconds((maxMilliseconds+1)*time.Millisecond, "test"); err == nil {
		t.Fatal("durationMilliseconds(too large) error = nil, want error")
	}
	if got, err := durationMilliseconds(1500*time.Millisecond, "test"); err != nil || got != 1500 {
		t.Fatalf("durationMilliseconds() = %d, %v; want 1500, nil", got, err)
	}
}

func newTestClient(t *testing.T, addr string) *TCPClient {
	t.Helper()
	return newTestClientWithOptions(t, ClientOptions{
		Address:         addr,
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
	})
}

func newTestClientWithOptions(t *testing.T, options ClientOptions) *TCPClient {
	t.Helper()
	client, err := NewTCPClient(options)
	if err != nil {
		t.Fatalf("NewTCPClient() error = %v", err)
	}
	return client
}

func assertFrame(t *testing.T, frame recordedFrame, cmd command, payload []byte) {
	t.Helper()
	if frame.Command != byte(cmd) {
		t.Fatalf("command = 0x%02x, want 0x%02x", frame.Command, byte(cmd))
	}
	if string(frame.Payload) != string(payload) {
		t.Fatalf("payload = % X, want % X", frame.Payload, payload)
	}
	if got, want := frame.Raw[len(frame.Raw)-1], checksum(frame.Raw[:len(frame.Raw)-1]); got != want {
		t.Fatalf("frame checksum = 0x%02x, want 0x%02x", got, want)
	}
}

func establishTestConnection(t *testing.T, client *TCPClient, server *scriptedMatrixServer, ctx context.Context) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Ping(ctx) }()
	assertFrame(t, server.ExpectFrame(t), commandPing, nil)
	server.RespondOK()
	if err := <-errCh; err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}
}

type reconnectObservationRecorder struct {
	mu         sync.Mutex
	attempts   []ReconnectAttempt
	recoveries []ReconnectRecovery
	failures   []ReconnectFailure
}

func (r *reconnectObservationRecorder) RecordAttempt(attempt ReconnectAttempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, attempt)
}

func (r *reconnectObservationRecorder) RecordRecovery(recovery ReconnectRecovery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recoveries = append(r.recoveries, recovery)
}

func (r *reconnectObservationRecorder) RecordFailure(failure ReconnectFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, failure)
}

func (r *reconnectObservationRecorder) Snapshot() ([]ReconnectAttempt, []ReconnectRecovery, []ReconnectFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReconnectAttempt(nil), r.attempts...),
		append([]ReconnectRecovery(nil), r.recoveries...),
		append([]ReconnectFailure(nil), r.failures...)
}

func assertNoReconnectObservations(t *testing.T, recorder *reconnectObservationRecorder, reason string) {
	t.Helper()
	attempts, recoveries, failures := recorder.Snapshot()
	if len(attempts) != 0 {
		t.Fatalf("reconnect attempts = %d, want 0 after %s: %+v", len(attempts), reason, attempts)
	}
	if len(recoveries) != 0 {
		t.Fatalf("reconnect recoveries = %d, want 0 after %s: %+v", len(recoveries), reason, recoveries)
	}
	if len(failures) != 0 {
		t.Fatalf("reconnect failures = %d, want 0 after %s: %+v", len(failures), reason, failures)
	}
}

func assertSingleTCPImmediateAttempt(t *testing.T, attempts []ReconnectAttempt) {
	t.Helper()
	if len(attempts) != 1 {
		t.Fatalf("reconnect attempts = %d, want 1: %+v", len(attempts), attempts)
	}
	if attempts[0].Source != ReconnectSourceTCPImmediate || attempts[0].Attempt != 1 ||
		attempts[0].ErrorKind != ErrorKindRetryable || attempts[0].Error == "" {
		t.Fatalf("reconnect attempt = %+v, want tcp immediate retryable attempt", attempts[0])
	}
}

type commandResultRecorder struct {
	mu      sync.Mutex
	results []CommandResult
}

func (r *commandResultRecorder) Record(result CommandResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
}

func (r *commandResultRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = nil
}

func (r *commandResultRecorder) Snapshot() []CommandResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CommandResult(nil), r.results...)
}

type commandResultWant struct {
	command command
	status  string
}

func assertCommandResults(t *testing.T, got []CommandResult, want ...commandResultWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("command results = %d, want %d: got %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Command != want[i].command.String() || got[i].Status != want[i].status {
			t.Fatalf("command result[%d] = %+v, want command=%q status=%q",
				i, got[i], want[i].command.String(), want[i].status)
		}
		if got[i].Duration < 0 {
			t.Fatalf("command result[%d] duration = %v, want non-negative", i, got[i].Duration)
		}
	}
}

func receiveCommandResult(t *testing.T, results <-chan CommandResult) CommandResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command done callback")
	}
	return CommandResult{}
}

func closedListenerAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("listener close error = %v", err)
	}
	return addr
}

type errorConn struct {
	writeErr error
}

func (c errorConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c errorConn) Write([]byte) (int, error)        { return 0, c.writeErr }
func (c errorConn) Close() error                     { return nil }
func (c errorConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c errorConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c errorConn) SetDeadline(time.Time) error      { return nil }
func (c errorConn) SetReadDeadline(time.Time) error  { return nil }
func (c errorConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

type recordedFrame struct {
	Command byte
	Payload []byte
	Raw     []byte
}

type responseAction struct {
	status Status
	raw    []byte
	close  bool
}

type scriptedMatrixServer struct {
	t *testing.T

	ln       net.Listener
	frames   chan recordedFrame
	actions  chan responseAction
	errs     chan error
	done     chan struct{}
	wg       sync.WaitGroup
	writesMu sync.Mutex
	writes   int
}

func newScriptedMatrixServer(t *testing.T) *scriptedMatrixServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	server := &scriptedMatrixServer{
		t:       t,
		ln:      ln,
		frames:  make(chan recordedFrame, 32),
		actions: make(chan responseAction, 32),
		errs:    make(chan error, 32),
		done:    make(chan struct{}),
	}
	server.wg.Add(1)
	go server.accept()
	return server
}

func (s *scriptedMatrixServer) Addr() string {
	return s.ln.Addr().String()
}

func (s *scriptedMatrixServer) Close() {
	close(s.done)
	_ = s.ln.Close()
	s.wg.Wait()
	select {
	case err := <-s.errs:
		s.t.Fatalf("scripted matrix server error = %v", err)
	default:
	}
}

func (s *scriptedMatrixServer) ExpectFrame(t *testing.T) recordedFrame {
	t.Helper()
	select {
	case frame := <-s.frames:
		return frame
	case err := <-s.errs:
		t.Fatalf("scripted matrix server error = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for matrix command frame")
	}
	return recordedFrame{}
}

func (s *scriptedMatrixServer) RespondOK() {
	s.Respond(StatusOK)
}

func (s *scriptedMatrixServer) Respond(status Status) {
	s.actions <- responseAction{status: status}
}

func (s *scriptedMatrixServer) RespondRaw(response []byte) {
	s.actions <- responseAction{raw: response}
}

func (s *scriptedMatrixServer) CloseConnection() {
	s.actions <- responseAction{close: true}
}

func (s *scriptedMatrixServer) ResponseWrites() int {
	s.writesMu.Lock()
	defer s.writesMu.Unlock()
	return s.writes
}

func (s *scriptedMatrixServer) WaitForResponseWrites(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		got := s.ResponseWrites()
		if got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("response writes = %d, want %d", got, want)
		case <-ticker.C:
		}
	}
}

func (s *scriptedMatrixServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				s.reportErr(err)
				return
			}
		}

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *scriptedMatrixServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	for {
		frame, err := readTestFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return
			}
			s.reportErr(err)
			return
		}

		select {
		case s.frames <- frame:
		case <-s.done:
			return
		}

		var action responseAction
		select {
		case action = <-s.actions:
		case <-s.done:
			return
		}

		if action.close {
			return
		}
		response := action.raw
		if response == nil {
			response = testResponse(action.status)
		}
		n, err := conn.Write(response)
		if err != nil {
			s.reportErr(err)
			return
		}
		if n != len(response) {
			s.reportErr(fmt.Errorf("response write length = %d, want %d", n, len(response)))
			return
		}

		s.writesMu.Lock()
		s.writes++
		s.writesMu.Unlock()
	}
}

func (s *scriptedMatrixServer) reportErr(err error) {
	select {
	case s.errs <- err:
	default:
	}
}

func readTestFrame(conn net.Conn) (recordedFrame, error) {
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(conn, header); err != nil {
		return recordedFrame{}, err
	}
	payloadLen := int(header[4])
	tail := make([]byte, payloadLen+checksumSize)
	if _, err := io.ReadFull(conn, tail); err != nil {
		return recordedFrame{}, err
	}

	raw := append(append([]byte{}, header...), tail...)
	if header[0] != magic0 || header[1] != magic1 {
		return recordedFrame{}, fmt.Errorf("bad command magic: % X", raw)
	}
	if header[2] != protocolVersion {
		return recordedFrame{}, fmt.Errorf("bad command version: % X", raw)
	}
	if got, want := raw[len(raw)-1], checksum(raw[:len(raw)-1]); got != want {
		return recordedFrame{}, fmt.Errorf("bad command checksum 0x%02x, want 0x%02x", got, want)
	}

	payload := append([]byte{}, tail[:payloadLen]...)
	return recordedFrame{
		Command: header[3],
		Payload: payload,
		Raw:     raw,
	}, nil
}
