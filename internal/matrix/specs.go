package matrix

import (
	"context"
	"fmt"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

// Dispatch tables for the scheduler.
//
// Each table maps one enum to the behaviour it selects, so adding a control
// kind, loop policy or restore policy is an entry here rather than another
// branch in the run loop.

type controlSpec struct {
	validateRequest      func(ControlRequest) error
	run                  func(context.Context, *Scheduler, *ControlItem) error
	rememberState        func(*Scheduler, *ControlItem)
	marksBackgroundDirty bool
}

type restorePolicySpec func(context.Context, *Scheduler, displayState) error

type displayStateRestoreSpec func(context.Context, *Scheduler, displayState) error

var displayStateRestoreSpecs = map[displayStateKind]displayStateRestoreSpec{
	displayStateFrame: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.SetFrame(ctx, state.Frame)
		})
	},
	displayStateFill: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.Fill(ctx, state.Color)
		})
	},
	displayStateClear: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.Clear(ctx)
		})
	},
	displayStatePreset: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.SetPreset(ctx, state.EffectID, state.Interval, state.Color)
		})
	},
	displayStateStatic: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.SetStaticColor(ctx, state.Color)
		})
	},
	displayStateAnimation: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return uploadAnimation(ctx, s.client, state.Animation)
		})
	},
}

// uploadAnimation pushes a firmware-resident custom animation one frame at a time.
// The device starts looping once it has received every frame of the declared count,
// so the frames must be sent in order with a stable count.
func uploadAnimation(ctx context.Context, client Client, frames []AnimationFrame) error {
	if len(frames) == 0 {
		return fmt.Errorf("%w: custom animation requires at least one frame", ErrInvalidControl)
	}
	if len(frames) > MaxAnimationFrames {
		return fmt.Errorf("%w: custom animation supports at most %d frames: got %d", ErrInvalidControl, MaxAnimationFrames, len(frames))
	}
	count := byte(len(frames))
	for index, frame := range frames {
		if err := client.UploadCustomFrame(ctx, byte(index), count, frame.Delay, frame.Frame); err != nil {
			return err
		}
	}
	return nil
}

var controlSpecs = map[ControlKind]controlSpec{
	ControlClear: {
		run: func(ctx context.Context, scheduler *Scheduler, _ *ControlItem) error {
			return scheduler.client.Clear(ctx)
		},
		rememberState: func(scheduler *Scheduler, _ *ControlItem) {
			scheduler.rememberDisplayState(displayState{Kind: displayStateClear})
		},
		marksBackgroundDirty: true,
	},
	ControlSetBrightness: {
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetBrightness(ctx, control.Brightness)
		},
	},
	ControlSetPreset: {
		validateRequest: func(request ControlRequest) error {
			// The firmware implements effects 1..22 and treats 0 as "stop effect";
			// anything higher comes back as status 0x04. Rejecting it here turns a
			// confusing 502 from the panel into a 400 at the boundary.
			if request.EffectID > animations.MaxFirmwareEffectID {
				return fmt.Errorf("%w: effect_id must be between 0 and %d: %d", ErrInvalidControl, animations.MaxFirmwareEffectID, request.EffectID)
			}
			_, err := durationMilliseconds(request.Interval, "preset interval")
			return err
		},
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetPreset(ctx, control.EffectID, control.Interval, control.Color)
		},
		rememberState: func(scheduler *Scheduler, control *ControlItem) {
			// Effect 0 stops the running effect without touching the frame buffer, so
			// the panel keeps whatever the last tick drew. Remembering it would make
			// restore: previous_frame replay a command that reproduces no image —
			// exactly the failure ControlSetPixel deliberately avoids.
			if control.EffectID == animations.StopEffectID {
				return
			}
			scheduler.rememberDisplayState(displayState{
				Kind:     displayStatePreset,
				EffectID: control.EffectID,
				Interval: control.Interval,
				// Effects that compute their own colours discard this byte, so storing
				// it would let convergence compare something the panel never rendered.
				Color: presetColorForState(control.EffectID, control.Color),
			})
		},
		marksBackgroundDirty: true,
	},
	ControlFill: {
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.Fill(ctx, control.Color)
		},
		rememberState: func(scheduler *Scheduler, control *ControlItem) {
			scheduler.rememberDisplayState(displayState{
				Kind:  displayStateFill,
				Color: control.Color,
			})
		},
		marksBackgroundDirty: true,
	},
	ControlSetPixel: {
		validateRequest: func(request ControlRequest) error {
			return validatePixelCoordinate(request.X, request.Y)
		},
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetPixel(ctx, control.X, control.Y, control.Color)
		},
		// Deliberately no rememberState: a single pixel mutates whatever frame the
		// panel already held, so the resulting display cannot be reconstructed from
		// this command alone. Leaving the remembered state untouched is honest —
		// restore: previous_frame keeps the last state we can actually reproduce.
		marksBackgroundDirty: true,
	},
	ControlSetPanel: {
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetPanelEnabled(ctx, control.Enabled)
		},
		// Panel enable is a visibility flag: the firmware keeps the stored frame and
		// restores it on re-enable, so the desired background is still satisfied and
		// must not be marked dirty.
	},
	ControlUploadAnimation: {
		validateRequest: func(request ControlRequest) error {
			return validateAnimationFrames(request.Animation)
		},
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return uploadAnimation(ctx, scheduler.client, control.Animation)
		},
		rememberState: func(scheduler *Scheduler, control *ControlItem) {
			scheduler.rememberDisplayState(displayState{
				Kind:      displayStateAnimation,
				Animation: control.Animation,
			})
		},
		marksBackgroundDirty: true,
	},
}

// presetColorForState zeroes the colour for effects that ignore it, so remembered
// state and background-match comparisons only consider bytes the panel used.
func presetColorForState(effectID byte, color RGB) RGB {
	if animations.FirmwareEffectIgnoresColor(effectID) {
		return RGB{}
	}
	return color
}

func validatePixelCoordinate(x, y byte) error {
	if int(x) >= animations.CanvasWidth || int(y) >= animations.CanvasHeight {
		return fmt.Errorf("%w: pixel (%d,%d) outside %dx%d matrix", ErrInvalidControl, x, y, animations.CanvasWidth, animations.CanvasHeight)
	}
	return nil
}

func validateAnimationFrames(frames []AnimationFrame) error {
	if len(frames) == 0 {
		return fmt.Errorf("%w: custom animation requires at least one frame", ErrInvalidControl)
	}
	if len(frames) > MaxAnimationFrames {
		return fmt.Errorf("%w: custom animation supports at most %d frames: got %d", ErrInvalidControl, MaxAnimationFrames, len(frames))
	}
	for index, frame := range frames {
		if _, err := durationMilliseconds(frame.Delay, fmt.Sprintf("animation frame %d delay", index)); err != nil {
			return err
		}
	}
	return nil
}

type playItemLoop func(context.Context, *Scheduler, PlayItem) error

var playItemLoopStrategies = map[animations.LoopPolicy]playItemLoop{
	animations.LoopForever: func(ctx context.Context, s *Scheduler, item PlayItem) error {
		for {
			if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
				return err
			}
			if !item.Deadline.IsZero() && !s.now().Before(item.Deadline) {
				return nil
			}
		}
	},
	animations.LoopUntil: func(ctx context.Context, s *Scheduler, item PlayItem) error {
		for item.Deadline.IsZero() || s.now().Before(item.Deadline) {
			if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
				return err
			}
		}
		return nil
	},
	animations.LoopNone: func(ctx context.Context, s *Scheduler, item PlayItem) error {
		return s.playFrames(ctx, item.Frames, item.Deadline)
	},
}

type matrixErrorRecoveryPolicy func(*Scheduler, context.Context, time.Time, error) error

var matrixRetryPolicies = map[ErrorKind]matrixErrorRecoveryPolicy{
	ErrorKindRetryable: func(s *Scheduler, ctx context.Context, deadline time.Time, _ error) error {
		s.markMatrixFailure(StateDisconnected)
		return s.waitReady(ctx, deadline)
	},
	ErrorKindPermanent: func(_ *Scheduler, _ context.Context, _ time.Time, err error) error {
		return err
	},
}

var probeRetryableKinds = map[ErrorKind]struct{}{
	ErrorKindRetryable: {},
}

var restorePolicySpecs = map[animations.RestorePolicy]restorePolicySpec{
	animations.RestoreLeave: func(ctx context.Context, _ *Scheduler, _ displayState) error {
		return nil
	},
	animations.RestoreClear: func(ctx context.Context, s *Scheduler, _ displayState) error {
		err := s.retryMatrix(ctx, func() error {
			return s.client.Clear(ctx)
		})
		if err == nil {
			s.rememberDisplayState(displayState{Kind: displayStateClear})
		}
		return err
	},
	animations.RestorePreviousFrame: func(ctx context.Context, s *Scheduler, previous displayState) error {
		if !previous.known() {
			return nil
		}
		err := s.restoreDisplayState(ctx, previous)
		if err == nil && s.displayStateMatchesConfiguredBackground(previous) {
			s.markDesiredBackgroundConverged()
		}
		return err
	},
	animations.RestoreBackground: func(ctx context.Context, s *Scheduler, _ displayState) error {
		return s.applyDesiredBackground(ctx, true)
	},
}

func resolveControlSpec(kind ControlKind) (controlSpec, bool) {
	spec, ok := controlSpecs[kind]
	return spec, ok
}
