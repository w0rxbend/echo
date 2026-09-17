package matrix

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// Connection health: probing, backoff, and the connected/disconnected state
// the readiness endpoint reports.
//
// Everything here answers one question -- is the device reachable right now,
// and if not, when should we try again.

func (s *Scheduler) waitReady(ctx context.Context, deadline time.Time) error {
	s.setState(StateDisconnected)
	for {
		if !deadline.IsZero() && !s.now().Before(deadline) {
			s.reportReconnectFailure(ReconnectFailureDeadlineExceeded, ErrPlayItemExpired)
			return ErrPlayItemExpired
		}
		s.setState(StateConnecting)
		err, probeTimedOut := s.pingProbe(ctx, deadline)
		if err == nil {
			s.markMatrixSuccess(StateReady)
			return nil
		}
		{
			if ctxErr := ctx.Err(); ctxErr != nil {
				s.reportReconnectFailure(reconnectFailureOutcomeFromError(ctxErr), ctxErr)
				return ctxErr
			}
			s.reportProbeFailure(ctx, err, probeTimedOut)
			if !deadline.IsZero() && !s.now().Before(deadline) {
				s.reportReconnectFailure(ReconnectFailureDeadlineExceeded, ErrPlayItemExpired)
				return ErrPlayItemExpired
			}
			if _, ok := probeRetryableKinds[s.classifyProbeError(ctx, err)]; !ok {
				return cancellationOr(ctx, err)
			}
		}
		s.markMatrixFailure(StateDisconnected)

		delay, err := s.nextReconnectDelay(deadline, err)
		if err != nil {
			if errors.Is(err, ErrPlayItemExpired) {
				s.reportReconnectFailure(ReconnectFailureDeadlineExceeded, err)
			}
			return err
		}
		if err := sleepContext(ctx, delay); err != nil {
			s.reportReconnectFailure(reconnectFailureOutcomeFromError(err), err)
			return err
		}
	}
}

func (s *Scheduler) heartbeat(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err, probeTimedOut := s.pingProbe(ctx, time.Time{})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		s.reportProbeFailure(ctx, err, probeTimedOut)
		if s.classifyProbeError(ctx, err) == ErrorKindPermanent {
			return cancellationOr(ctx, err)
		}
		s.markMatrixFailure(StateDisconnected)
		return nil
	}
	s.markMatrixSuccess(StateReady)
	return nil
}

func (s *Scheduler) pingProbe(ctx context.Context, deadline time.Time) (error, bool) {
	probeCtx, cancel := s.probeContext(ctx, deadline)
	defer cancel()
	err := s.client.Ping(probeCtx)
	probeTimedOut := err != nil &&
		errors.Is(probeCtx.Err(), context.DeadlineExceeded) &&
		(ctx == nil || ctx.Err() == nil)
	return err, probeTimedOut
}

func (s *Scheduler) probeContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	timeout := s.probeTimeout
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *Scheduler) nextReconnectDelay(deadline time.Time, retryErr error) (time.Duration, error) {
	attempt := s.incrementReconnectAttempt()
	base := exponentialReconnectDelay(attempt, s.reconnectMinDelay, s.reconnectMaxDelay)
	delay := s.reconnectJitter(base)
	if delay <= 0 {
		delay = base
	}
	if delay > s.reconnectMaxDelay {
		delay = s.reconnectMaxDelay
	}

	deadlineCapped := false
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, ErrPlayItemExpired
		}
		if remaining < delay {
			delay = remaining
			deadlineCapped = true
		}
	}

	if s.onReconnectDelay != nil {
		errText := ""
		if retryErr != nil {
			errText = retryErr.Error()
		}
		s.callbackPanics.Run(observabilityCallbackReconnectDelay, func() {
			s.onReconnectDelay(ReconnectAttempt{
				Source:         ReconnectSourceSchedulerBackoff,
				Attempt:        attempt,
				BaseDelay:      base,
				Delay:          delay,
				DeadlineCapped: deadlineCapped,
				ErrorKind:      ErrorKindRetryable,
				Error:          errText,
			})
		})
	}
	return delay, nil
}

func (s *Scheduler) incrementReconnectAttempt() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconnectAttempt++
	return s.reconnectAttempt
}

func (s *Scheduler) currentReconnectAttempt() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reconnectAttempt
}

func exponentialReconnectDelay(attempt int, minDelay, maxDelay time.Duration) time.Duration {
	if attempt <= 1 {
		return minDelay
	}
	delay := minDelay
	for i := 1; i < attempt; i++ {
		if delay >= maxDelay {
			return maxDelay
		}
		next := delay * 2
		if next < delay || next > maxDelay {
			return maxDelay
		}
		delay = next
	}
	return delay
}

func defaultReconnectJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	window := base / 5
	if window <= 0 {
		return base
	}
	return base - time.Duration(rand.Int63n(int64(window)+1))
}

// cancellationOr reports ctx's own error in place of err once ctx is done.
//
// Both classifiers treat every error as permanent the moment the context is
// cancelled, so an ordinary "connection refused" from a probe that raced
// cancellation is indistinguishable from a genuine permanent failure. Checking
// the context before the probe is not enough: cancellation can land between
// that check and the classification, and then a retryable transport error
// escapes Run as a real failure -- which is how Shutdown came to report a
// connection error for a device the caller had just deliberately stopped.
//
// Anywhere a "permanent" verdict would end the run, ask the context first.
func cancellationOr(ctx context.Context, err error) error {
	if ctx == nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func (s *Scheduler) classifyProbeError(ctx context.Context, err error) ErrorKind {
	if err == nil {
		return ErrorKindNone
	}
	if ctx != nil && ctx.Err() != nil {
		return ErrorKindPermanent
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindRetryable
	}
	return ClassifyError(ctx, err)
}

func (s *Scheduler) reportProbeFailure(ctx context.Context, err error, probeTimedOut bool) {
	if s.onProbeFailure == nil {
		return
	}
	reason := ProbeFailurePermanent
	errorKind := s.classifyProbeError(ctx, err)
	if probeTimedOut {
		reason = ProbeFailureProbeTimeout
		errorKind = ErrorKindRetryable
	} else if errorKind == ErrorKindRetryable {
		reason = ProbeFailureTransport
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	failure := ProbeFailure{
		ErrorKind: errorKind,
		Reason:    reason,
		Error:     errText,
	}
	s.callbackPanics.Run(observabilityCallbackProbeFailure, func() {
		s.onProbeFailure(failure)
	})
}

func reconnectFailureOutcomeFromError(err error) ReconnectFailureOutcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPlayItemExpired) {
		return ReconnectFailureDeadlineExceeded
	}
	return ReconnectFailureCanceled
}

func (s *Scheduler) reportReconnectFailure(outcome ReconnectFailureOutcome, err error) {
	if s.onReconnectFailure == nil {
		return
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	failure := ReconnectFailure{
		Source:    ReconnectSourceSchedulerBackoff,
		Attempt:   s.currentReconnectAttempt(),
		ErrorKind: ClassifyError(context.Background(), err),
		Outcome:   outcome,
		Error:     errText,
	}
	s.callbackPanics.Run(observabilityCallbackReconnectFailure, func() {
		s.onReconnectFailure(failure)
	})
}

func clientReconnectRecoveryCount(client Client) uint64 {
	counter, ok := client.(clientReconnectRecoveryCounter)
	if !ok {
		return 0
	}
	return counter.reconnectRecoveryCount()
}

type clientReconnectRecoveryCounter interface {
	reconnectRecoveryCount() uint64
}

func (s *Scheduler) setState(state State) {
	var changedToDisconnected bool
	s.mu.Lock()
	previousConnected := s.connected
	s.state = state
	if state == StateDisconnected || state == StateDraining {
		s.connected = false
	}
	if previousConnected && !s.connected && s.background.AnimationID != "" {
		s.markDesiredBackgroundDirtyLocked(false)
	}
	changedToDisconnected = previousConnected && !s.connected
	s.mu.Unlock()
	if changedToDisconnected {
		s.notifyMatrixConnectedChange(false)
	}
}

func (s *Scheduler) setConnected(connected bool) {
	var changed bool
	recoveryCount := clientReconnectRecoveryCount(s.client)
	s.mu.Lock()
	previousConnected := s.connected
	s.connected = connected
	if connected {
		recovered := !previousConnected || recoveryCount != s.clientReconnectRecoveries
		if recoveryCount != s.clientReconnectRecoveries {
			s.clientReconnectRecoveries = recoveryCount
		}
		if recovered {
			s.markDesiredBackgroundDirtyLocked(true)
		}
		s.reconnectAttempt = 0
		s.lastSuccess = s.now().UTC()
	} else {
		s.lastFailure = s.now().UTC()
		if previousConnected && s.background.AnimationID != "" {
			s.markDesiredBackgroundDirtyLocked(false)
		}
	}
	changed = previousConnected != connected
	s.mu.Unlock()
	if changed {
		s.notifyMatrixConnectedChange(connected)
	}
}

func (s *Scheduler) markMatrixSuccess(state State) {
	var recovery *ReconnectRecovery
	var connectedChanged bool
	recoveryCount := clientReconnectRecoveryCount(s.client)
	s.mu.Lock()
	previousConnected := s.connected
	attempt := s.reconnectAttempt
	s.state = state
	s.connected = true
	clientRecovered := recoveryCount != s.clientReconnectRecoveries
	if clientRecovered {
		s.clientReconnectRecoveries = recoveryCount
	}
	if s.background.AnimationID != "" {
		if !previousConnected || clientRecovered {
			s.markDesiredBackgroundDirtyLocked(true)
		}
	}
	// A panel that rebooted came back at its firmware default brightness, not the
	// one the operator configured. The background has its own convergence, so
	// without this the display silently drifts brighter or dimmer after every
	// reconnect and stays that way until the service restarts.
	if s.initialBrightness != nil && (!previousConnected || clientRecovered) {
		s.brightnessDirty = true
	}
	s.reconnectAttempt = 0
	s.lastSuccess = s.now().UTC()
	if !previousConnected && attempt > 0 {
		recovery = &ReconnectRecovery{Source: ReconnectSourceSchedulerBackoff, Attempt: attempt, State: state}
	}
	connectedChanged = !previousConnected
	s.mu.Unlock()
	if connectedChanged {
		s.notifyMatrixConnectedChange(true)
	}
	if recovery != nil && s.onReconnectRecovered != nil {
		s.callbackPanics.Run(observabilityCallbackReconnectRecovered, func() {
			s.onReconnectRecovered(*recovery)
		})
	}
}

func (s *Scheduler) markMatrixFailure(state State) {
	var connectedChanged bool
	s.mu.Lock()
	previousConnected := s.connected
	s.state = state
	s.connected = false
	if previousConnected && s.background.AnimationID != "" {
		s.markDesiredBackgroundDirtyLocked(false)
	}
	s.lastFailure = s.now().UTC()
	connectedChanged = previousConnected
	s.mu.Unlock()
	if connectedChanged {
		s.notifyMatrixConnectedChange(false)
	}
}

func (s *Scheduler) notifyMatrixConnectedChange(connected bool) {
	if s.onMatrixConnectedChange != nil {
		s.callbackPanics.Run(observabilityCallbackMatrixConnectedChange, func() {
			s.onMatrixConnectedChange(connected)
		})
	}
}

func (s *Scheduler) retryMatrix(ctx context.Context, fn func() error) error {
	return s.retryMatrixUntilReady(ctx, fn)
}

func (s *Scheduler) retryMatrixUntilReady(ctx context.Context, fn func() error) error {
	for {
		if err := fn(); err != nil {
			if err := s.retryMatrixError(ctx, time.Time{}, err, ClassifyError); err != nil {
				return err
			}
			continue
		}
		s.markMatrixSuccess(s.State())
		return nil
	}
}

func (s *Scheduler) retryMatrixError(ctx context.Context, readyDeadline time.Time, err error, classify func(context.Context, error) ErrorKind) error {
	recovery, ok := matrixRetryPolicies[classify(ctx, err)]
	if !ok {
		return err
	}
	return recovery(s, ctx, readyDeadline, err)
}

func (s *Scheduler) retryControlMatrix(ctx context.Context, control *ControlItem, fn func() error) error {
	for {
		if control != nil && !control.Deadline.IsZero() && !s.now().Before(control.Deadline) {
			return ErrPlayItemExpired
		}
		if err := fn(); err != nil {
			if control != nil && !control.Deadline.IsZero() && !s.now().Before(control.Deadline) {
				return ErrPlayItemExpired
			}
			deadline := time.Time{}
			if control != nil {
				deadline = control.Deadline
			}
			if err := s.retryMatrixError(ctx, deadline, err, ClassifyError); err != nil {
				return err
			}
			continue
		}
		s.markMatrixSuccess(s.State())
		return nil
	}
}
