package animations

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestRegistryRenderableHelpersSeparateGeneratedAnimationsFromFirmwarePresets(t *testing.T) {
	registry := NewRegistry()
	animation := AnimationFunc(func(context.Context, Params) ([]Frame, error) {
		return []Frame{{Delay: time.Millisecond}}, nil
	})
	preset := FirmwarePreset{
		EffectID: 9,
		Interval: 150 * time.Millisecond,
		Color:    RGB{R: 3, G: 4, B: 5},
	}

	if err := registry.RegisterFirmwarePreset("matrix_rain_background", preset); err != nil {
		t.Fatalf("RegisterFirmwarePreset() error = %v", err)
	}
	if err := registry.RegisterGenerated("alert", "notification", animation); err != nil {
		t.Fatalf("RegisterGenerated() error = %v", err)
	}
	if err := registry.RegisterGenerated("notification", "notification", animation); err != nil {
		t.Fatalf("RegisterGenerated() error = %v", err)
	}

	if !registry.IsRenderable("alert") {
		t.Fatal("generated animation IsRenderable() = false, want true")
	}
	if registry.IsRenderable("matrix_rain_background") {
		t.Fatal("firmware preset IsRenderable() = true, want false")
	}
	if registry.IsRenderable("missing") {
		t.Fatal("missing animation IsRenderable() = true, want false")
	}

	if got, want := registry.RenderableIDs(), []string{"alert", "notification"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RenderableIDs() = %v, want %v", got, want)
	}
	if got, want := registry.IDs(), []string{"alert", "matrix_rain_background", "notification"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
}

func TestRegistryEntryAndFirmwarePresetReturnClonedMetadata(t *testing.T) {
	registry := NewRegistry()
	preset := FirmwarePreset{
		EffectID: 7,
		Interval: 250 * time.Millisecond,
		Color:    RGB{R: 1, G: 2, B: 3},
	}
	if err := registry.RegisterFirmwarePreset("rain", preset); err != nil {
		t.Fatalf("RegisterFirmwarePreset() error = %v", err)
	}

	entry, ok := registry.Entry("rain")
	if !ok {
		t.Fatal("Entry() ok = false, want true")
	}
	if entry.FirmwarePreset == nil {
		t.Fatal("Entry().FirmwarePreset = nil, want preset metadata")
	}
	entry.FirmwarePreset.EffectID = 99
	entry.FirmwarePreset.Interval = time.Second
	entry.FirmwarePreset.Color = RGB{R: 9, G: 9, B: 9}

	got, ok := registry.FirmwarePreset("rain")
	if !ok {
		t.Fatal("FirmwarePreset() ok = false, want true")
	}
	if got != preset {
		t.Fatalf("FirmwarePreset() after Entry mutation = %+v, want %+v", got, preset)
	}

	secondEntry, ok := registry.Entry("rain")
	if !ok {
		t.Fatal("second Entry() ok = false, want true")
	}
	if secondEntry.FirmwarePreset == nil {
		t.Fatal("second Entry().FirmwarePreset = nil, want preset metadata")
	}
	if *secondEntry.FirmwarePreset != preset {
		t.Fatalf("second Entry().FirmwarePreset = %+v, want %+v", *secondEntry.FirmwarePreset, preset)
	}
}
