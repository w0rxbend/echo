package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultConnectTimeout  = 5 * time.Second
	defaultResponseTimeout = 2 * time.Second
)

type Client interface {
	Ping(ctx context.Context) error
	Clear(ctx context.Context) error
	SetBrightness(ctx context.Context, value byte) error
	Fill(ctx context.Context, c RGB) error
	SetPixel(ctx context.Context, x, y byte, c RGB) error
	SetFrame(ctx context.Context, frame PackedFrame) error
	SetPanelEnabled(ctx context.Context, enabled bool) error
	SetStaticColor(ctx context.Context, c RGB) error
	SetPreset(ctx context.Context, effectID byte, interval time.Duration, c RGB) error
	UploadCustomFrame(ctx context.Context, index, count byte, delay time.Duration, frame PackedFrame) error
	StopEffect(ctx context.Context) error
}

type ClientOptions struct {
	Address         string
	Host            string
	Port            int
	ConnectTimeout  time.Duration
	ResponseTimeout time.Duration
	// OnCommandDone runs synchronously while command serialization is held.
	// Keep it fast, nonblocking, and panic-safe.
	OnCommandDone func(CommandResult)
	// OnReconnectAttempt runs synchronously while command serialization is held.
	// Keep it fast and nonblocking; route logging or external I/O off this path.
	OnReconnectAttempt func(ReconnectAttempt)
	// OnReconnectRecovered runs synchronously while command serialization is held.
	// Keep it fast and nonblocking; route logging or external I/O off this path.
	OnReconnectRecovered func(ReconnectRecovery)
	// OnReconnectFailure runs synchronously while command serialization is held.
	// Keep it fast and nonblocking; route logging or external I/O off this path.
	OnReconnectFailure func(ReconnectFailure)
}

type CommandResult struct {
	Command  string
	Status   string
	Duration time.Duration
}

type TCPClient struct {
	addr            string
	connectTimeout  time.Duration
	responseTimeout time.Duration

	mu   sync.Mutex
	conn net.Conn

	onCommandDone        func(CommandResult)
	onReconnectAttempt   func(ReconnectAttempt)
	onReconnectRecovered func(ReconnectRecovery)
	onReconnectFailure   func(ReconnectFailure)

	reconnectRecoveries atomic.Uint64

	observabilityMu                  sync.Mutex
	observabilityCallbackPanicCounts map[string]uint64
	observabilityCallbackPanics      atomic.Uint64
}

func NewTCPClient(options ClientOptions) (*TCPClient, error) {
	addr := options.Address
	if addr == "" && options.Host != "" && options.Port > 0 {
		addr = net.JoinHostPort(options.Host, strconv.Itoa(options.Port))
	}
	if addr == "" {
		return nil, errors.New("matrix client address is required")
	}

	connectTimeout := options.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}
	responseTimeout := options.ResponseTimeout
	if responseTimeout <= 0 {
		responseTimeout = defaultResponseTimeout
	}

	return &TCPClient{
		addr:                 addr,
		connectTimeout:       connectTimeout,
		responseTimeout:      responseTimeout,
		onCommandDone:        options.OnCommandDone,
		onReconnectAttempt:   options.OnReconnectAttempt,
		onReconnectRecovered: options.OnReconnectRecovered,
		onReconnectFailure:   options.OnReconnectFailure,
	}, nil
}

func (c *TCPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *TCPClient) reconnectRecoveryCount() uint64 {
	return c.reconnectRecoveries.Load()
}

func (c *TCPClient) sendCommand(ctx context.Context, cmd command, payload []byte) error {
	if _, err := buildCommandFrame(cmd, payload); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	needsPing := cmd != commandPing
	if err := c.ensureConnectedLocked(ctx, needsPing); err != nil {
		return err
	}

	if err := c.sendRawLocked(ctx, cmd, payload); err != nil {
		if !shouldReconnect(ctx, err) {
			return err
		}
		attempt := ReconnectAttempt{
			Source:    ReconnectSourceTCPImmediate,
			Attempt:   1,
			ErrorKind: ErrorKindRetryable,
			Error:     err.Error(),
		}
		c.reportReconnectAttempt(attempt)
		if closeErr := c.closeLocked(); closeErr != nil {
			joined := errors.Join(err, closeErr)
			c.reportReconnectFailure(reconnectFailureFromError(ctx, attempt.Attempt, joined))
			return joined
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.reportReconnectFailure(reconnectFailureFromError(ctx, attempt.Attempt, ctxErr))
			return ctxErr
		}
		if reconnectErr := c.connectLocked(ctx, needsPing); reconnectErr != nil {
			joined := errors.Join(err, reconnectErr)
			failure := reconnectFailureFromError(ctx, attempt.Attempt, joined)
			if isReconnectVerificationError(reconnectErr) {
				failure = reconnectFailureWithOutcome(ctx, attempt.Attempt, joined, ReconnectFailureVerificationFailed)
			}
			c.reportReconnectFailure(failure)
			return joined
		}
		// For non-ping commands, connectLocked(..., true) sends and validates a
		// ping before returning. That means reconnect recovery is about
		// firmware-verified replacement connectivity, not the retried logical
		// command result. If the retried non-ping command then loses transport,
		// the same operation may emit reconnect recovery plus command failure;
		// the reconnect attempt itself has already terminated with recovery.
		// For Ping itself, the retried command is the firmware verification step:
		// permanent status/protocol/validation errors are verification failures,
		// not recoveries, and the suspect replacement socket is closed.
		reconnectVerified := needsPing
		if reconnectVerified {
			c.reportReconnectRecovered(ReconnectRecovery{
				Source:  ReconnectSourceTCPImmediate,
				Attempt: attempt.Attempt,
				State:   StateReady,
			})
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if retryErr := c.sendRawLocked(ctx, cmd, payload); retryErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(retryErr, context.Canceled) ||
				errors.Is(retryErr, context.DeadlineExceeded) || isRetryableTransportError(retryErr)) {
				joined := errors.Join(retryErr, ctxErr, c.closeLocked())
				if !reconnectVerified {
					c.reportReconnectFailure(reconnectFailureFromError(ctx, attempt.Attempt, joined))
				}
				return joined
			}
			if shouldReconnect(ctx, retryErr) {
				joined := errors.Join(retryErr, c.closeLocked())
				if !reconnectVerified {
					c.reportReconnectFailure(reconnectFailureFromError(ctx, attempt.Attempt, joined))
				}
				return joined
			}
			if !reconnectVerified {
				verificationErr := retryErr
				if closeErr := c.closeLocked(); closeErr != nil {
					verificationErr = errors.Join(retryErr, closeErr)
				}
				failure := reconnectFailureFromError(ctx, attempt.Attempt, verificationErr)
				if isReconnectVerificationFailure(ctx, retryErr) {
					failure = reconnectFailureWithOutcome(ctx, attempt.Attempt, verificationErr, ReconnectFailureVerificationFailed)
				}
				c.reportReconnectFailure(failure)
				return verificationErr
			}
			return retryErr
		}
		if !reconnectVerified {
			c.reportReconnectRecovered(ReconnectRecovery{
				Source:  ReconnectSourceTCPImmediate,
				Attempt: attempt.Attempt,
				State:   StateReady,
			})
		}
	}

	return nil
}

func shouldReconnect(ctx context.Context, err error) bool {
	return IsRetryableError(ctx, err)
}

func (c *TCPClient) ensureConnectedLocked(ctx context.Context, ping bool) error {
	if c.conn != nil {
		return nil
	}
	return c.connectLocked(ctx, ping)
}

func (c *TCPClient) connectLocked(ctx context.Context, ping bool) error {
	if err := c.closeLocked(); err != nil {
		return err
	}

	dialer := net.Dialer{Timeout: c.connectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("connect matrix %s: %w", c.addr, err)
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			_ = conn.Close()
			return fmt.Errorf("set tcp nodelay: %w", err)
		}
	}

	c.conn = conn
	if ping {
		if err := c.sendRawLocked(ctx, commandPing, nil); err != nil {
			_ = c.closeLocked()
			err = fmt.Errorf("ping after connect: %w", err)
			if isReconnectVerificationFailure(ctx, err) {
				return &reconnectVerificationError{err: err}
			}
			return err
		}
	}

	return nil
}

func (c *TCPClient) sendRawLocked(ctx context.Context, cmd command, payload []byte) error {
	frame, err := buildCommandFrame(cmd, payload)
	if err != nil {
		return err
	}
	if c.conn == nil {
		return net.ErrClosed
	}

	if err := c.setDeadline(ctx); err != nil {
		return err
	}

	start := time.Now()
	var commandErr error
	defer func() {
		c.reportCommandDone(CommandResult{
			Command:  cmd.String(),
			Status:   commandStatusLabel(commandErr),
			Duration: time.Since(start),
		})
	}()

	if err := writeAll(c.conn, frame); err != nil {
		commandErr = fmt.Errorf("write matrix command 0x%02x: %w", byte(cmd), err)
		return commandErr
	}

	response := make([]byte, responseSize)
	if _, err := io.ReadFull(c.conn, response); err != nil {
		commandErr = fmt.Errorf("read matrix response: %w", err)
		return commandErr
	}
	if err := parseResponse(response); err != nil {
		commandErr = err
		return err
	}

	return nil
}

func (c *TCPClient) reportCommandDone(result CommandResult) {
	if c.onCommandDone == nil {
		return
	}
	defer c.recoverObservabilityCallback(ObservabilityCallbackCommandDone)
	c.onCommandDone(result)
}

func (c *TCPClient) reportReconnectAttempt(attempt ReconnectAttempt) {
	if c.onReconnectAttempt == nil {
		return
	}
	defer c.recoverObservabilityCallback(ObservabilityCallbackReconnectAttempt)
	c.onReconnectAttempt(attempt)
}

func (c *TCPClient) reportReconnectRecovered(recovery ReconnectRecovery) {
	c.reconnectRecoveries.Add(1)
	if c.onReconnectRecovered == nil {
		return
	}
	defer c.recoverObservabilityCallback(ObservabilityCallbackReconnectRecovered)
	c.onReconnectRecovered(recovery)
}

func (c *TCPClient) reportReconnectFailure(failure ReconnectFailure) {
	if c.onReconnectFailure == nil {
		return
	}
	defer c.recoverObservabilityCallback(ObservabilityCallbackReconnectFailure)
	c.onReconnectFailure(failure)
}

func (c *TCPClient) recoverObservabilityCallback(name string) {
	if recovered := recover(); recovered != nil {
		c.recordObservabilityCallbackPanic(name)
	}
}

func (c *TCPClient) recordObservabilityCallbackPanic(name string) {
	c.observabilityCallbackPanics.Add(1)
	c.observabilityMu.Lock()
	defer c.observabilityMu.Unlock()
	if c.observabilityCallbackPanicCounts == nil {
		c.observabilityCallbackPanicCounts = make(map[string]uint64)
	}
	c.observabilityCallbackPanicCounts[name]++
}

func (c *TCPClient) ObservabilityCallbackPanics() uint64 {
	return c.observabilityCallbackPanics.Load()
}

func (c *TCPClient) ObservabilityCallbackPanicCounts() map[string]uint64 {
	c.observabilityMu.Lock()
	defer c.observabilityMu.Unlock()
	if len(c.observabilityCallbackPanicCounts) == 0 {
		return nil
	}
	counts := make(map[string]uint64, len(c.observabilityCallbackPanicCounts))
	for name, count := range c.observabilityCallbackPanicCounts {
		counts[name] = count
	}
	return counts
}

func reconnectFailureFromError(ctx context.Context, attempt int, err error) ReconnectFailure {
	outcome := ReconnectFailureFailed
	if errors.Is(err, context.Canceled) {
		outcome = ReconnectFailureCanceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		outcome = ReconnectFailureDeadlineExceeded
	}
	return reconnectFailureWithOutcome(ctx, attempt, err, outcome)
}

func reconnectFailureWithOutcome(ctx context.Context, attempt int, err error, outcome ReconnectFailureOutcome) ReconnectFailure {
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return ReconnectFailure{
		Source:    ReconnectSourceTCPImmediate,
		Attempt:   attempt,
		ErrorKind: ClassifyError(ctx, err),
		Outcome:   outcome,
		Error:     errText,
	}
}

func commandStatusLabel(err error) string {
	if err == nil {
		return StatusOK.Label()
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status.Label()
	}
	if errors.Is(err, ErrProtocol) {
		return "protocol_error"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if IsRetryableError(context.Background(), err) {
		return "transport_error"
	}
	return "error"
}

func (c *TCPClient) setDeadline(ctx context.Context) error {
	deadline := time.Now().Add(c.responseTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set matrix deadline: %w", err)
	}
	return nil
}

func (c *TCPClient) closeLocked() error {
	if c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
