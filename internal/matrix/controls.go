package matrix

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The control lane: direct matrix commands from the HTTP API.
//
// Controls are queued like animations but run in a reserved lane ahead of them,
// and the caller blocks until the device acknowledges, so each carries its own
// completion and deadline.

func (s *Scheduler) Clear(ctx context.Context) error {
	return s.EnqueueControl(ctx, ControlRequest{Kind: ControlClear})
}

func (s *Scheduler) SetBrightness(ctx context.Context, value byte) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:       ControlSetBrightness,
		Brightness: value,
	})
}

func (s *Scheduler) SetPreset(ctx context.Context, effectID byte, interval time.Duration, color RGB) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:     ControlSetPreset,
		EffectID: effectID,
		Interval: interval,
		Color:    color,
	})
}

// SetPixel sets one pixel, taking display-space coordinates.
//
// The firmware applies its own serpentine mapping to whatever x/y it receives, but
// that mapping alone is NOT the one frame uploads go through: LayoutPacker also
// compensates for this panel's mirrored odd rows (Layout.OddRowDisplayFlip) before
// computing the chain index. Forwarding display coordinates raw therefore put every
// pixel on rows 1/3/5/7 at the mirrored LED — 32 of 64 coordinates disagreed with
// every frame-based path.
//
// Converting to server space here is exactly the missing half: the firmware's own
// serpentine step then lands on the same physical LED the packer would have chosen.
// This is the same display -> server translation the Python reference client does in
// tools/matrix_client.py::display_to_server_point.
func (s *Scheduler) SetPixel(ctx context.Context, x, y byte, color RGB) error {
	layout := s.packer.Layout()
	serverX, serverY, err := layout.DisplayToServerPoint(int(x), int(y))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidControl, err)
	}
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:  ControlSetPixel,
		X:     byte(serverX),
		Y:     byte(serverY),
		Color: color,
	})
}

// SetPanelEnabled blanks or restores visible output without discarding the frame
// the firmware is holding.
func (s *Scheduler) SetPanelEnabled(ctx context.Context, enabled bool) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:    ControlSetPanel,
		Enabled: enabled,
	})
}

// UploadAnimation stores an animation in the firmware's custom slot and lets the
// device loop it locally, with no per-frame TCP round-trip. Frames arrive in
// display space and are packed to the device's physical LED order here, so callers
// never need to know the panel's wiring.
func (s *Scheduler) UploadAnimation(ctx context.Context, frames []Frame) error {
	if len(frames) == 0 {
		return fmt.Errorf("%w: custom animation requires at least one frame", ErrInvalidControl)
	}
	if len(frames) > MaxAnimationFrames {
		return fmt.Errorf("%w: custom animation supports at most %d frames: got %d", ErrInvalidControl, MaxAnimationFrames, len(frames))
	}
	packed := make([]AnimationFrame, 0, len(frames))
	for _, frame := range frames {
		packed = append(packed, AnimationFrame{
			Frame: s.packer.Pack(frame),
			Delay: frame.Delay,
		})
	}
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:      ControlUploadAnimation,
		Animation: packed,
	})
}

func (s *Scheduler) Fill(ctx context.Context, color RGB) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:  ControlFill,
		Color: color,
	})
}

func (s *Scheduler) EnqueueControl(ctx context.Context, request ControlRequest) error {
	item, err := s.ResolveControl(ctx, request)
	if err != nil {
		return err
	}
	if s.expired(item.PlayItem) {
		s.completeControlWithOutcome(item, ErrPlayItemExpired, 0)
		return ErrPlayItemExpired
	}
	handle, queueDepth, err := s.queue.enqueueScheduled(ctx, item)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.completeControlWithOutcome(item, err, queueDepth)
			return err
		}
		dropErr := errors.Join(ErrControlDropped, err)
		s.completeControlWithOutcome(item, dropErr, queueDepth)
		return dropErr
	}
	item.QueueDepthAtAdmission = queueDepth
	s.reportQueueDepth(queueDepth)

	stopContext := context.AfterFunc(ctx, func() {
		s.completePendingControl(handle, ctx.Err())
	})
	defer stopContext()

	stopDeadline := s.afterControlDeadline(handle, item.Control)
	defer stopDeadline()

	<-item.Control.done
	return item.Control.result()
}

func (s *Scheduler) ResolveControl(ctx context.Context, request ControlRequest) (ScheduledItem, error) {
	if err := ctx.Err(); err != nil {
		return ScheduledItem{}, err
	}
	if request.Kind == "" {
		return ScheduledItem{}, fmt.Errorf("%w: control kind is required", ErrInvalidControl)
	}
	spec, ok := resolveControlSpec(request.Kind)
	if !ok {
		return ScheduledItem{}, fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, request.Kind)
	}
	if spec.validateRequest != nil {
		if err := spec.validateRequest(request); err != nil {
			return ScheduledItem{}, err
		}
	}

	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.now().UTC()
	}
	id := request.ID
	if id == "" {
		id = fmt.Sprintf("%s:%d", request.Kind, createdAt.UnixNano())
	}

	control := &ControlItem{
		ID:         id,
		Kind:       request.Kind,
		Priority:   request.Priority,
		Brightness: request.Brightness,
		EffectID:   request.EffectID,
		Interval:   request.Interval,
		Color:      request.Color,
		X:          request.X,
		Y:          request.Y,
		Enabled:    request.Enabled,
		Animation:  request.Animation,
		CreatedAt:  createdAt,
		Deadline:   request.Deadline,
		ctx:        ctx,
		done:       make(chan struct{}),
	}

	return ScheduledItem{
		PlayItem: PlayItem{
			ID:       id,
			Priority: request.Priority,
			Deadline: request.Deadline,
		},
		CreatedAt: createdAt,
		Control:   control,
	}, nil
}

func (s *Scheduler) executeControl(ctx context.Context, control *ControlItem) error {
	if control == nil {
		return nil
	}
	spec, ok := resolveControlSpec(control.Kind)
	if !ok {
		return fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, control.Kind)
	}
	if spec.run == nil {
		return fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, control.Kind)
	}
	controlCtx := control.ctx
	if controlCtx == nil {
		controlCtx = context.Background()
	}
	if err := controlCtx.Err(); err != nil {
		return err
	}

	var execCtx context.Context
	var cancel context.CancelFunc
	if !control.Deadline.IsZero() {
		execCtx, cancel = context.WithDeadline(ctx, control.Deadline)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(controlCtx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	err := s.retryControlMatrix(execCtx, control, func() error {
		return spec.run(execCtx, s, control)
	})
	if err != nil && ctx.Err() == nil {
		if controlErr := controlCtx.Err(); controlErr != nil {
			return controlErr
		}
		if errors.Is(err, context.DeadlineExceeded) && !control.Deadline.IsZero() && !s.now().Before(control.Deadline) {
			return ErrPlayItemExpired
		}
	}
	if err == nil {
		s.rememberControlDisplayState(control)
		s.markDesiredBackgroundDirtyAfterControl(control)
	}
	return err
}

func (s *Scheduler) completePendingControl(handle queueHandle, err error) {
	removed, queueDepthAtRemoval, ok := s.queue.remove(handle)
	if !ok || removed.Control == nil {
		return
	}
	if queueDepthAtRemoval > 0 {
		s.reportQueueDepth(queueDepthAtRemoval - 1)
	}
	s.completeControlWithOutcome(removed, err, queueDepthAtRemoval)
}

func (s *Scheduler) afterControlDeadline(handle queueHandle, control *ControlItem) func() bool {
	if control == nil || control.Deadline.IsZero() {
		return func() bool { return true }
	}
	delay := time.Until(control.Deadline)
	if delay <= 0 {
		s.completePendingControl(handle, ErrPlayItemExpired)
		return func() bool { return false }
	}
	timer := time.AfterFunc(delay, func() {
		s.completePendingControl(handle, ErrPlayItemExpired)
	})
	return timer.Stop
}

func (control *ControlItem) complete(err error) bool {
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.completed {
		return false
	}
	control.err = err
	control.completed = true
	close(control.done)
	return true
}

func (control *ControlItem) result() error {
	control.mu.Lock()
	defer control.mu.Unlock()
	return control.err
}

func (control *ControlItem) isCompleted() bool {
	control.mu.Lock()
	defer control.mu.Unlock()
	return control.completed
}

func (control *ControlItem) ctxErr() error {
	if control.ctx == nil {
		return nil
	}
	return control.ctx.Err()
}
