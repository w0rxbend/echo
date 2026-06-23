package animations

import (
	"context"
	"fmt"
	"time"
)

const NotificationAnimationID = "notification"

const NotificationGeneratorID = "notification"

type notificationAnimation struct{}

func NewNotificationAnimation() Animation {
	return notificationAnimation{}
}

func NewDefaultRegistry() (*Registry, error) {
	registry := NewRegistry()
	if err := RegisterBuiltins(registry); err != nil {
		return nil, err
	}
	return registry, nil
}

func RegisterBuiltins(registry *Registry) error {
	if registry == nil {
		return fmt.Errorf("registry is required")
	}
	animation, err := NewGeneratedAnimation(NotificationGeneratorID)
	if err != nil {
		return err
	}
	return registry.RegisterGenerated(NotificationAnimationID, NotificationGeneratorID, animation)
}

func NewGeneratedAnimation(generatorID string) (Animation, error) {
	switch generatorID {
	case NotificationGeneratorID:
		return NewNotificationAnimation(), nil
	default:
		return nil, fmt.Errorf("unknown animation generator %q", generatorID)
	}
}

func BuiltinGeneratorIDs() []string {
	return []string{NotificationGeneratorID}
}

func (notificationAnimation) Render(ctx context.Context, _ Params) ([]Frame, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	levels := []byte{48, 88, 140, 220, 220, 140, 88, 48}
	frames := make([]Frame, 0, len(levels))
	for _, level := range levels {
		canvas := NewCanvas8x8(250 * time.Millisecond)
		drawNotificationBadge(&canvas, level)
		frames = append(frames, canvas.Frame())
	}
	return frames, nil
}

func drawNotificationBadge(canvas *Canvas8x8, level byte) {
	primary := RGB{R: 0, G: level, B: level}
	accent := RGB{R: level, G: level / 2, B: 0}
	white := RGB{R: level, G: level, B: level}

	for x := 1; x <= 5; x++ {
		mustSet(canvas, x, 1, primary)
		mustSet(canvas, x, 5, primary)
	}
	for y := 2; y <= 4; y++ {
		mustSet(canvas, 1, y, primary)
		mustSet(canvas, 5, y, primary)
	}

	// Asymmetric accents make orientation errors visible on real hardware.
	mustSet(canvas, 6, 0, accent)
	mustSet(canvas, 6, 1, accent)
	mustSet(canvas, 7, 2, accent)
	mustSet(canvas, 2, 6, white)
	mustSet(canvas, 3, 6, accent)
}

func mustSet(canvas *Canvas8x8, x, y int, color RGB) {
	if err := canvas.Set(x, y, color); err != nil {
		panic(err)
	}
}
