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
)

func TestClassifyErrorRetryableConnectivityFailures(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		err  error
	}{
		{name: "wrapped host unreachable", err: wrappedSyscallError(syscall.EHOSTUNREACH)},
		{name: "wrapped network unreachable", err: wrappedSyscallError(syscall.ENETUNREACH)},
		{name: "wrapped connection timed out", err: wrappedSyscallError(syscall.ETIMEDOUT)},
		{name: "temporary dns failure", err: fmt.Errorf("resolve matrix host: %w", &net.DNSError{Name: "matrix.local", Err: "temporary failure", IsTemporary: true})},
		{name: "wrapped eof", err: fmt.Errorf("read matrix response: %w", io.EOF)},
		{name: "wrapped connection reset", err: &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}},
		{name: "closed socket", err: net.ErrClosed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyError(ctx, tt.err); got != ErrorKindRetryable {
				t.Fatalf("ClassifyError(%v) = %s, want %s", tt.err, got, ErrorKindRetryable)
			}
		})
	}
}

func TestClassifyErrorContextCancellationPermanent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := wrappedSyscallError(syscall.EHOSTUNREACH)
	if got := ClassifyError(ctx, err); got != ErrorKindPermanent {
		t.Fatalf("ClassifyError(canceled context, %v) = %s, want %s", err, got, ErrorKindPermanent)
	}
}

func TestClassifyErrorUnknownErrorPermanent(t *testing.T) {
	err := errors.New("matrix command failed")
	if got := ClassifyError(context.Background(), err); got != ErrorKindPermanent {
		t.Fatalf("ClassifyError(%v) = %s, want %s", err, got, ErrorKindPermanent)
	}
}

func wrappedSyscallError(errno syscall.Errno) error {
	return fmt.Errorf("dial matrix: %w", &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: errno},
	})
}
