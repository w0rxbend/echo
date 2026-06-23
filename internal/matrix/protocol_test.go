package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestChecksum(t *testing.T) {
	frame := []byte{magic0, magic1, protocolVersion, byte(commandPing), 0}
	if got, want := checksum(frame), byte(0); got != want {
		t.Fatalf("checksum() = 0x%02x, want 0x%02x", got, want)
	}
}

func TestBuildCommandFrame(t *testing.T) {
	frame, err := buildCommandFrame(commandFill, []byte{0x11, 0x22, 0x33})
	if err != nil {
		t.Fatalf("buildCommandFrame() error = %v", err)
	}

	wantPrefix := []byte{magic0, magic1, protocolVersion, byte(commandFill), 3, 0x11, 0x22, 0x33}
	if len(frame) != len(wantPrefix)+1 {
		t.Fatalf("frame length = %d, want %d", len(frame), len(wantPrefix)+1)
	}
	for i := range wantPrefix {
		if frame[i] != wantPrefix[i] {
			t.Fatalf("frame[%d] = 0x%02x, want 0x%02x", i, frame[i], wantPrefix[i])
		}
	}
	if got, want := frame[len(frame)-1], checksum(frame[:len(frame)-1]); got != want {
		t.Fatalf("checksum byte = 0x%02x, want 0x%02x", got, want)
	}
}

func TestBuildCommandFrameAllowsCustomFramePayload(t *testing.T) {
	payload := make([]byte, customFramePayloadSize)
	frame, err := buildCommandFrame(commandUploadCustomFrame, payload)
	if err != nil {
		t.Fatalf("buildCommandFrame() error = %v", err)
	}
	if got, want := len(frame), headerSize+customFramePayloadSize+checksumSize; got != want {
		t.Fatalf("frame length = %d, want %d", got, want)
	}
	if got, want := frame[4], byte(customFramePayloadSize); got != want {
		t.Fatalf("payload length byte = %d, want %d", got, want)
	}
}

func TestBuildCommandFrameRejectsPayloadOver255(t *testing.T) {
	_, err := buildCommandFrame(commandSetFrame, make([]byte, maxPayloadSize+1))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("buildCommandFrame() error = %v, want ErrPayloadTooLarge", err)
	}
}

func TestParseResponseValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func([]byte)
	}{
		{
			name: "bad magic",
			edit: func(response []byte) {
				response[0] = 0
				response[5] = checksum(response[:5])
			},
		},
		{
			name: "bad version",
			edit: func(response []byte) {
				response[2] = 2
				response[5] = checksum(response[:5])
			},
		},
		{
			name: "bad response command",
			edit: func(response []byte) {
				response[3] = byte(commandPing)
				response[5] = checksum(response[:5])
			},
		},
		{
			name: "bad checksum",
			edit: func(response []byte) {
				response[5] ^= 0xFF
			},
		},
	}

	if err := parseResponse(testResponse(StatusOK)); err != nil {
		t.Fatalf("parseResponse(ok) error = %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := testResponse(StatusOK)
			tt.edit(response)
			if err := parseResponse(response); !errors.Is(err, ErrProtocol) {
				t.Fatalf("parseResponse() error = %v, want ErrProtocol", err)
			}
		})
	}
	if err := parseResponse([]byte{magic0}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("parseResponse(short) error = %v, want ErrProtocol", err)
	}
}

func TestStatusErrorMapping(t *testing.T) {
	tests := []struct {
		status Status
		want   error
	}{
		{StatusBadMagic, ErrStatusBadMagic},
		{StatusUnsupportedVersion, ErrStatusUnsupportedVersion},
		{StatusUnknownCommand, ErrStatusUnknownCommand},
		{StatusInvalidLength, ErrStatusInvalidLength},
		{StatusChecksumMismatch, ErrStatusChecksumMismatch},
		{Status(0x7F), ErrStatusUnknown},
	}

	for _, tt := range tests {
		err := parseResponse(testResponse(tt.status))
		if !errors.Is(err, tt.want) {
			t.Fatalf("parseResponse(status=%s) error = %v, want %v", tt.status, err, tt.want)
		}

		var statusErr *StatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("parseResponse(status=%s) error = %T, want *StatusError", tt.status, err)
		}
		if statusErr.Status != tt.status {
			t.Fatalf("StatusError.Status = %s, want %s", statusErr.Status, tt.status)
		}
	}
}

func TestMatrixErrorClassificationPermanent(t *testing.T) {
	ctx := context.Background()

	_, payloadErr := buildCommandFrame(commandSetFrame, make([]byte, maxPayloadSize+1))
	_, durationErr := durationMilliseconds((maxMilliseconds+1)*time.Millisecond, "test")

	tests := []struct {
		name string
		err  error
	}{
		{name: "status error", err: parseResponse(testResponse(StatusInvalidLength))},
		{name: "protocol error", err: parseResponse([]byte{magic0})},
		{name: "payload too large", err: payloadErr},
		{name: "duration validation", err: durationErr},
		{name: "context canceled", err: context.Canceled},
		{name: "context deadline", err: context.DeadlineExceeded},
		{name: "caller validation", err: errors.New("animation request animation_id is required")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !IsPermanentError(ctx, tt.err) {
				t.Fatalf("IsPermanentError(%v) = false, want true", tt.err)
			}
			if IsRetryableError(ctx, tt.err) {
				t.Fatalf("IsRetryableError(%v) = true, want false", tt.err)
			}
		})
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	timeoutErr := testNetError{timeout: true}
	if !IsPermanentError(canceledCtx, timeoutErr) {
		t.Fatalf("IsPermanentError(canceled context, timeout) = false, want true")
	}
	if IsRetryableError(canceledCtx, timeoutErr) {
		t.Fatalf("IsRetryableError(canceled context, timeout) = true, want false")
	}
}

func TestMatrixErrorClassificationRetryableTransport(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		err  error
	}{
		{name: "eof", err: fmt.Errorf("read matrix response: %w", io.EOF)},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF},
		{name: "closed socket", err: net.ErrClosed},
		{name: "closed pipe", err: io.ErrClosedPipe},
		{name: "deadline timeout", err: os.ErrDeadlineExceeded},
		{name: "connection refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}},
		{name: "connection reset", err: &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}},
		{name: "temporary network failure", err: testNetError{temporary: true}},
		{name: "io timeout", err: testNetError{timeout: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !IsRetryableError(ctx, tt.err) {
				t.Fatalf("IsRetryableError(%v) = false, want true", tt.err)
			}
			if IsPermanentError(ctx, tt.err) {
				t.Fatalf("IsPermanentError(%v) = true, want false", tt.err)
			}
		})
	}
}

func TestDurationValidationWrapsSentinel(t *testing.T) {
	if _, err := durationMilliseconds(-time.Millisecond, "test"); !errors.Is(err, ErrInvalidDuration) {
		t.Fatalf("durationMilliseconds(negative) error = %v, want ErrInvalidDuration", err)
	}
	if _, err := durationMilliseconds((maxMilliseconds+1)*time.Millisecond, "test"); !errors.Is(err, ErrInvalidDuration) {
		t.Fatalf("durationMilliseconds(too large) error = %v, want ErrInvalidDuration", err)
	}
}

type testNetError struct {
	timeout   bool
	temporary bool
}

func (e testNetError) Error() string {
	return "test network error"
}

func (e testNetError) Timeout() bool {
	return e.timeout
}

func (e testNetError) Temporary() bool {
	return e.temporary
}

func testResponse(status Status) []byte {
	response := []byte{magic0, magic1, protocolVersion, responseCommand, byte(status), 0}
	response[5] = checksum(response[:5])
	return response
}
