package animations

import (
	"context"
	"testing"
	"time"
)

func TestDefaultRegistryIncludesNotification(t *testing.T) {
	registry, err := NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}

	animation, ok := registry.Get(NotificationAnimationID)
	if !ok {
		t.Fatalf("default registry missing %q", NotificationAnimationID)
	}
	if animation == nil {
		t.Fatalf("default registry returned nil animation")
	}
	entry, ok := registry.Entry(NotificationAnimationID)
	if !ok {
		t.Fatalf("default registry missing entry %q", NotificationAnimationID)
	}
	if entry.Kind != EntryGenerated || entry.GeneratorID != NotificationGeneratorID {
		t.Fatalf("default registry entry = %+v, want generated notification", entry)
	}
}

func TestGeneratedAnimationConstructorsValidateGeneratorIDs(t *testing.T) {
	animation, err := NewGeneratedAnimation(NotificationGeneratorID)
	if err != nil {
		t.Fatalf("NewGeneratedAnimation(%q) error = %v", NotificationGeneratorID, err)
	}
	if animation == nil {
		t.Fatal("NewGeneratedAnimation returned nil animation")
	}

	if _, err := NewGeneratedAnimation("unknown"); err == nil {
		t.Fatal("NewGeneratedAnimation unknown generator error = nil, want error")
	}
}

func TestRegistryRegistersFirmwarePresetMetadata(t *testing.T) {
	registry := NewRegistry()
	preset := FirmwarePreset{
		EffectID: 7,
		Interval: 250 * time.Millisecond,
		Color:    RGB{R: 1, G: 2, B: 3},
	}
	if err := registry.RegisterFirmwarePreset("rain", preset); err != nil {
		t.Fatalf("RegisterFirmwarePreset() error = %v", err)
	}
	if !registry.Has("rain") {
		t.Fatal("registry does not report firmware preset ID")
	}
	if _, ok := registry.Get("rain"); ok {
		t.Fatal("firmware preset should not be returned as renderable animation")
	}
	got, ok := registry.FirmwarePreset("rain")
	if !ok {
		t.Fatal("FirmwarePreset() ok = false, want true")
	}
	if got != preset {
		t.Fatalf("FirmwarePreset() = %+v, want %+v", got, preset)
	}
}

func TestNotificationAnimationRendersTwoSecondAsymmetricFrames(t *testing.T) {
	animation := NewNotificationAnimation()
	frames, err := animation.Render(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) == 0 {
		t.Fatal("notification animation rendered no frames")
	}

	var total time.Duration
	for _, frame := range frames {
		total += frame.Delay
	}
	if total != 2*time.Second {
		t.Fatalf("notification animation duration = %s, want 2s", total)
	}

	first := frames[0]
	if first.Pixels[6] == (RGB{}) {
		t.Fatalf("expected asymmetric top-right accent at display (6,0)")
	}
	if first.Pixels[2*CanvasWidth+7] == (RGB{}) {
		t.Fatalf("expected asymmetric far-right accent at display (7,2)")
	}
	if first.Pixels[6*CanvasWidth+2] == first.Pixels[6*CanvasWidth+3] {
		t.Fatalf("expected bottom accent pixels to differ")
	}
	if first.Pixels[0] != (RGB{}) {
		t.Fatalf("expected display (0,0) to stay blank, got %+v", first.Pixels[0])
	}
}

func TestNotificationAnimationHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewNotificationAnimation().Render(ctx, nil)
	if err == nil {
		t.Fatal("expected canceled context error")
	}
}
