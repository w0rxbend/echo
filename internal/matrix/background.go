package matrix

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

// Idle-background convergence.
//
// The configured background is the display's desired resting state. Playback
// and direct controls take the panel away from it; everything here is about
// getting back, including the retry clock for when the device will not take the
// restore.
//
// The state these methods read and write lives under Scheduler.mu alongside the
// connection state, because Health() reports both in one consistent snapshot.

func validateBackgroundConfig(background BackgroundConfig, registry AnimationRegistry) error {
	if background.AnimationID == "" {
		return nil
	}
	if preset, ok := registry.FirmwarePreset(background.AnimationID); ok {
		if err := animations.ValidateFirmwarePreset(preset); err != nil {
			return fmt.Errorf("background animation %q: %w", background.AnimationID, err)
		}
		return nil
	}
	if _, ok := registry.StaticColor(background.AnimationID); ok {
		return nil
	}
	if _, ok := registry.Get(background.AnimationID); !ok {
		return fmt.Errorf("%w: %s", ErrMissingAnimation, background.AnimationID)
	}
	return nil
}

func backgroundKindFor(background BackgroundConfig, registry AnimationRegistry) BackgroundKind {
	if background.AnimationID == "" {
		return ""
	}
	if _, ok := registry.FirmwarePreset(background.AnimationID); ok {
		return BackgroundKindFirmwarePreset
	}
	if _, ok := registry.StaticColor(background.AnimationID); ok {
		return BackgroundKindStaticColor
	}
	return BackgroundKindRenderable
}

// SetBackground atomically replaces the scheduler's desired idle background.
// Pass an empty BackgroundConfig to disable idle convergence. The new animation
// ID is validated against the registry; an error is returned and the background
// is left unchanged if validation fails.
func (s *Scheduler) SetBackground(background BackgroundConfig) error {
	if err := validateBackgroundConfig(background, s.registry); err != nil {
		return err
	}
	newKind := backgroundKindFor(background, s.registry)
	s.mu.Lock()
	s.background = background
	s.backgroundKind = newKind
	if background.AnimationID != "" {
		s.markDesiredBackgroundDirtyLocked(true)
	} else {
		// Disabling the background: mark converged so the scheduler stops trying.
		s.markDesiredBackgroundConvergedLocked()
		s.backgroundConvergenceState = BackgroundConvergenceConverged
	}
	s.mu.Unlock()
	return nil
}

// Background returns the current desired idle background configuration.
func (s *Scheduler) Background() BackgroundConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.background
}

func (s *Scheduler) restore(ctx context.Context, policy animations.RestorePolicy, previous displayState) error {
	if policy == "" {
		policy = animations.RestoreLeave
	}
	spec, ok := restorePolicySpecs[policy]
	if !ok {
		return fmt.Errorf("unsupported restore policy %q", policy)
	}
	return spec(ctx, s, previous)
}

func (s *Scheduler) applyDesiredBackground(ctx context.Context, force bool) error {
	if !s.canApplyDesiredBackground(force) {
		return nil
	}
	s.setState(StateRestoringBackground)
	s.markBackgroundRestoreAttempt()
	var err error
	switch preset, color := s.backgroundPreset(); {
	case preset != nil:
		err = s.restoreFirmwarePreset(ctx, *preset)
	case color != nil:
		err = s.restoreStaticColorBackground(ctx, *color)
	default:
		err = s.restoreRenderableBackground(ctx)
	}
	if err == nil {
		s.markDesiredBackgroundClean()
	} else {
		s.markBackgroundRestoreFailure(ctx, err)
	}
	return err
}

// backgroundPreset resolves the configured background to whichever firmware-side
// representation backs it. Exactly one of the returns is non-nil; both nil means
// the background is a renderable animation driven frame-by-frame from here.
func (s *Scheduler) backgroundPreset() (*animations.FirmwarePreset, *animations.RGB) {
	if preset, ok := s.registry.FirmwarePreset(s.background.AnimationID); ok {
		return &preset, nil
	}
	if color, ok := s.registry.StaticColor(s.background.AnimationID); ok {
		return nil, &color
	}
	return nil, nil
}

func (s *Scheduler) restoreStaticColorBackground(ctx context.Context, color animations.RGB) error {
	rgb := RGB{R: color.R, G: color.G, B: color.B}
	err := s.retryBackgroundMatrix(ctx, func() error {
		return s.client.SetStaticColor(ctx, rgb)
	})
	if err == nil {
		s.rememberDisplayState(displayState{
			Kind:         displayStateStatic,
			Color:        rgb,
			BackgroundID: s.background.AnimationID,
		})
	}
	return err
}

func (s *Scheduler) restoreFirmwarePreset(ctx context.Context, preset animations.FirmwarePreset) error {
	err := s.retryBackgroundMatrix(ctx, func() error {
		return s.client.SetPreset(ctx, preset.EffectID, preset.Interval, preset.Color)
	})
	if err == nil {
		s.rememberDisplayState(displayState{
			Kind:         displayStatePreset,
			EffectID:     preset.EffectID,
			Interval:     preset.Interval,
			Color:        presetColorForState(preset.EffectID, RGB{R: preset.Color.R, G: preset.Color.G, B: preset.Color.B}),
			BackgroundID: s.background.AnimationID,
		})
	}
	return err
}

func (s *Scheduler) restoreRenderableBackground(ctx context.Context) error {
	animation, ok := s.registry.Get(s.background.AnimationID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMissingAnimation, s.background.AnimationID)
	}
	renderStart := time.Now()
	frames, err := animation.Render(ctx, s.background.Params)
	s.reportAnimationRendered(s.background.AnimationID, time.Since(renderStart))
	if err != nil {
		return fmt.Errorf("render background animation %q: %w", s.background.AnimationID, err)
	}
	if len(frames) == 0 {
		return fmt.Errorf("%w: %s", ErrEmptyAnimation, s.background.AnimationID)
	}

	err = s.retryBackgroundMatrix(ctx, func() error {
		return s.playFrames(ctx, frames, time.Time{})
	})
	if err == nil {
		s.rememberDisplayState(displayState{
			Kind:         displayStateFrame,
			Frame:        s.packer.Pack(frames[len(frames)-1]),
			BackgroundID: s.background.AnimationID,
		})
	}
	return err
}

func (s *Scheduler) displayStateMatchesConfiguredBackground(state displayState) bool {
	if s.background.AnimationID == "" {
		return false
	}
	if preset, ok := s.registry.FirmwarePreset(s.background.AnimationID); ok {
		return state.Kind == displayStatePreset &&
			state.EffectID == preset.EffectID &&
			state.Interval == preset.Interval &&
			state.Color == presetColorForState(preset.EffectID, RGB{R: preset.Color.R, G: preset.Color.G, B: preset.Color.B})
	}
	if color, ok := s.registry.StaticColor(s.background.AnimationID); ok {
		return state.Kind == displayStateStatic &&
			state.Color == RGB{R: color.R, G: color.G, B: color.B}
	}
	return state.Kind == displayStateFrame && state.BackgroundID == s.background.AnimationID
}

// retryBackgroundMatrix makes exactly one attempt and reports failure upward.
//
// Background convergence owns its own retry ladder (scheduleBackgroundRetryLocked),
// so on a retryable error this waits for the link to come back and then returns the
// error, letting the caller mark the background dirty and schedule a backoff retry.
// Looping until success here instead would converge inline and starve that ladder:
// the background would report converged while the panel never received the command.
func (s *Scheduler) retryBackgroundMatrix(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil {
		s.markMatrixSuccess(s.State())
		return nil
	}
	if ClassifyError(ctx, err) != ErrorKindRetryable {
		return err
	}
	s.markMatrixFailure(StateDisconnected)
	if waitErr := s.waitReady(ctx, time.Time{}); waitErr != nil {
		return waitErr
	}
	return err
}

func (s *Scheduler) restoreDisplayState(ctx context.Context, state displayState) error {
	restoreDisplayState, ok := displayStateRestoreSpecs[state.Kind]
	if !ok {
		return nil
	}
	err := restoreDisplayState(ctx, s, state)
	if err == nil {
		s.rememberDisplayState(state)
	}
	return err
}

func (s *Scheduler) snapshotDisplayState() displayState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.displayState
}

func (s *Scheduler) rememberControlDisplayState(control *ControlItem) {
	if control == nil {
		return
	}
	spec, ok := resolveControlSpec(control.Kind)
	if !ok || spec.rememberState == nil {
		return
	}
	spec.rememberState(s, control)
}

func (s *Scheduler) rememberDisplayState(state displayState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displayState = state
}

func (s *Scheduler) shouldApplyDesiredBackground() bool {
	return s.canApplyDesiredBackground(false)
}

func (s *Scheduler) canApplyDesiredBackground(force bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.background.AnimationID == "" || !s.connected {
		return false
	}
	if !force && !s.desiredBackgroundDirty && s.backgroundConvergenceState != BackgroundConvergenceUnknown {
		return false
	}
	return s.backgroundRetryDueLocked(s.now())
}

func (s *Scheduler) backgroundRetryDueLocked(now time.Time) bool {
	return s.backgroundNextRestoreAttempt.IsZero() || !now.Before(s.backgroundNextRestoreAttempt)
}

func (s *Scheduler) markDesiredBackgroundDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundDirtyLocked(false)
}

func (s *Scheduler) markDesiredBackgroundDirtyAfterControl(control *ControlItem) {
	if control == nil {
		return
	}
	spec, ok := resolveControlSpec(control.Kind)
	if !ok || !spec.marksBackgroundDirty {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundDirtyLocked(true)
}

func (s *Scheduler) markDesiredBackgroundDirtyLocked(resetRetry bool) {
	if s.background.AnimationID == "" {
		return
	}
	s.desiredBackgroundDirty = true
	if resetRetry {
		s.resetBackgroundRetryLocked()
	}
	s.backgroundConvergenceState = s.backgroundDirtyStateLocked(s.now())
}

func (s *Scheduler) backgroundDirtyStateLocked(now time.Time) BackgroundConvergenceState {
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	return ProjectBackgroundConvergence(BackgroundConvergenceProjectionInput{
		State:                 s.backgroundConvergenceState,
		Dirty:                 s.desiredBackgroundDirty,
		LastRestoreError:      s.backgroundLastRestoreError,
		LastRestoreErrorClass: s.backgroundLastRestoreErrorClass,
		NextRetry:             nextRetry,
		FailureCount:          s.backgroundRetryFailureCount,
	}, now).State
}

func (s *Scheduler) markDesiredBackgroundClean() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundConvergedLocked()
	s.backgroundLastRestoreSuccess = s.now().UTC()
}

func (s *Scheduler) markDesiredBackgroundConverged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundConvergedLocked()
}

func (s *Scheduler) markDesiredBackgroundConvergedLocked() {
	s.desiredBackgroundDirty = false
	s.backgroundConvergenceState = BackgroundConvergenceConverged
	s.backgroundLastRestoreError = ""
	s.backgroundLastRestoreErrorClass = ErrorKindNone
	s.resetBackgroundRetryLocked()
}

func (s *Scheduler) markBackgroundRestoreAttempt() {
	s.mu.Lock()
	s.desiredBackgroundDirty = true
	s.backgroundConvergenceState = BackgroundConvergenceAttempting
	s.backgroundLastRestoreAttempt = s.now().UTC()
	failureCount := s.backgroundRetryFailureCount
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	event := BackgroundRestoreEvent{
		AnimationID:  s.background.AnimationID,
		Kind:         s.backgroundKind,
		State:        s.backgroundConvergenceState,
		ErrorKind:    ErrorKindNone,
		NextRetry:    nextRetry,
		FailureCount: failureCount,
	}
	s.mu.Unlock()
	s.reportBackgroundRestore(event)
}

func (s *Scheduler) markBackgroundRestoreFailure(ctx context.Context, err error) {
	errorClass := classifyBackgroundRestoreError(ctx, err)
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	s.mu.Lock()
	now := s.now().UTC()
	s.desiredBackgroundDirty = true
	s.backgroundConvergenceState = BackgroundConvergenceRetrying
	s.backgroundLastRestoreError = errText
	s.backgroundLastRestoreErrorClass = errorClass
	s.lastFailure = now
	s.scheduleBackgroundRetryLocked(now, errorClass)
	failureCount := s.backgroundRetryFailureCount
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	event := BackgroundRestoreEvent{
		AnimationID:  s.background.AnimationID,
		Kind:         s.backgroundKind,
		State:        s.backgroundConvergenceState,
		ErrorKind:    errorClass,
		Error:        errText,
		NextRetry:    nextRetry,
		FailureCount: failureCount,
	}
	s.mu.Unlock()
	s.reportBackgroundRestore(event)
}

func (s *Scheduler) resetBackgroundRetryLocked() {
	s.backgroundRetryFailureCount = 0
	s.backgroundRetryLastErrorClass = ErrorKindNone
	s.backgroundNextRestoreAttempt = time.Time{}
}

func (s *Scheduler) scheduleBackgroundRetryLocked(now time.Time, errorClass ErrorKind) {
	if errorClass != ErrorKindRetryable && errorClass != ErrorKindPermanent {
		errorClass = ErrorKindPermanent
	}
	if errorClass != s.backgroundRetryLastErrorClass {
		s.backgroundRetryFailureCount = 0
		s.backgroundRetryLastErrorClass = errorClass
	}
	s.backgroundRetryFailureCount++
	minDelay, maxDelay := backgroundRetryBounds(errorClass)
	delay := exponentialReconnectDelay(s.backgroundRetryFailureCount, minDelay, maxDelay)
	s.backgroundNextRestoreAttempt = now.Add(delay)
}

func backgroundRetryBounds(errorClass ErrorKind) (time.Duration, time.Duration) {
	if errorClass == ErrorKindRetryable {
		return backgroundRetryableMinDelay, backgroundRetryableMaxDelay
	}
	return backgroundPermanentMinDelay, backgroundPermanentMaxDelay
}

func (s *Scheduler) reportBackgroundRestore(event BackgroundRestoreEvent) {
	if s.onBackgroundRestore == nil {
		return
	}
	s.callbackPanics.Run(observabilityCallbackBackgroundRestore, func() {
		s.onBackgroundRestore(event)
	})
}

func classifyBackgroundRestoreError(ctx context.Context, err error) ErrorKind {
	if err == nil {
		return ErrorKindNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindPermanent
	}
	if errors.Is(err, ErrMissingAnimation) || errors.Is(err, ErrEmptyAnimation) || isPermanentMatrixError(err) {
		return ErrorKindPermanent
	}
	return ErrorKindRetryable
}
