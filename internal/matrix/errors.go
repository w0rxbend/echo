package matrix

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

var (
	ErrPayloadTooLarge = errors.New("matrix payload too large")
	ErrInvalidDuration = errors.New("matrix invalid duration")
	ErrProtocol        = errors.New("matrix protocol error")

	ErrStatusBadMagic           = errors.New("matrix status bad magic")
	ErrStatusUnsupportedVersion = errors.New("matrix status unsupported version")
	ErrStatusUnknownCommand     = errors.New("matrix status unknown command")
	ErrStatusInvalidLength      = errors.New("matrix status invalid length")
	ErrStatusChecksumMismatch   = errors.New("matrix status checksum mismatch")
	ErrStatusUnknown            = errors.New("matrix status unknown")
)

type ErrorKind string

const (
	ErrorKindNone      ErrorKind = "none"
	ErrorKindRetryable ErrorKind = "retryable"
	ErrorKindPermanent ErrorKind = "permanent"
)

func ClassifyError(ctx context.Context, err error) ErrorKind {
	if err == nil {
		return ErrorKindNone
	}
	if ctx != nil && ctx.Err() != nil {
		return ErrorKindPermanent
	}
	if isPermanentMatrixError(err) {
		return ErrorKindPermanent
	}
	if isRetryableTransportError(err) {
		return ErrorKindRetryable
	}
	return ErrorKindPermanent
}

func IsRetryableError(ctx context.Context, err error) bool {
	return ClassifyError(ctx, err) == ErrorKindRetryable
}

func IsPermanentError(ctx context.Context, err error) bool {
	return ClassifyError(ctx, err) == ErrorKindPermanent
}

type reconnectVerificationError struct {
	err error
}

func (e *reconnectVerificationError) Error() string {
	return "matrix reconnect verification failed: " + e.err.Error()
}

func (e *reconnectVerificationError) Unwrap() error {
	return e.err
}

func isReconnectVerificationError(err error) bool {
	var verificationErr *reconnectVerificationError
	return errors.As(err, &verificationErr)
}

func isReconnectVerificationFailure(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return !IsRetryableError(ctx, err)
}

func isPermanentMatrixError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, ErrProtocol) ||
		errors.Is(err, ErrPayloadTooLarge) ||
		errors.Is(err, ErrInvalidDuration) ||
		errors.Is(err, ErrInvalidControl) {
		return true
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return true
	}
	return false
}

func isRetryableTransportError(err error) bool {
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrShortWrite) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	return false
}
