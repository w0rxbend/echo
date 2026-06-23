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
	if got, want := registry.Catalog(), []CatalogEntry{
		{ID: "alert", Kind: PublicKindGenerated, Playable: true},
		{
			ID:       "matrix_rain_background",
			Kind:     PublicKindFirmwarePreset,
			Playable: false,
			EffectID: bytePtr(9),
			Interval: durationPtr(150 * time.Millisecond),
			Color:    rgbPtr(RGB{R: 3, G: 4, B: 5}),
		},
		{ID: "notification", Kind: PublicKindGenerated, Playable: true},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Catalog() = %+v, want %+v", got, want)
	}
}

func TestProjectPublicKind(t *testing.T) {
	tests := []struct {
		name string
		kind string
		want PublicKind
		ok   bool
	}{
		{name: "generated", kind: string(EntryGenerated), want: PublicKindGenerated, ok: true},
		{name: "renderable internal background", kind: "renderable", want: PublicKindGenerated, ok: true},
		{name: "firmware preset", kind: string(EntryFirmwarePreset), want: PublicKindFirmwarePreset, ok: true},
		{name: "unknown", kind: "custom", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ProjectPublicKind(tt.kind)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("ProjectPublicKind(%q) = %q, %v; want %q, %v", tt.kind, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRegistryCatalogCoversAllEntriesAndKeepsFirmwarePresetsNonPlayable(t *testing.T) {
	registry := NewRegistry()
	animation := AnimationFunc(func(context.Context, Params) ([]Frame, error) {
		return []Frame{{Delay: time.Millisecond}}, nil
	})
	presets := map[string]FirmwarePreset{
		"matrix_rain_background": {
			EffectID: 9,
			Interval: 150 * time.Millisecond,
			Color:    RGB{R: 3, G: 4, B: 5},
		},
		"slow_glow_background": {
			EffectID: 4,
			Interval: 500 * time.Millisecond,
			Color:    RGB{R: 1, G: 2, B: 3},
		},
	}

	if err := registry.RegisterGenerated("notification", "notification", animation); err != nil {
		t.Fatalf("RegisterGenerated() error = %v", err)
	}
	for id, preset := range presets {
		if err := registry.RegisterFirmwarePreset(id, preset); err != nil {
			t.Fatalf("RegisterFirmwarePreset(%q) error = %v", id, err)
		}
	}

	got := registry.Catalog()
	want := []CatalogEntry{
		{
			ID:       "matrix_rain_background",
			Kind:     PublicKindFirmwarePreset,
			Playable: false,
			EffectID: bytePtr(9),
			Interval: durationPtr(150 * time.Millisecond),
			Color:    rgbPtr(RGB{R: 3, G: 4, B: 5}),
		},
		{ID: "notification", Kind: PublicKindGenerated, Playable: true},
		{
			ID:       "slow_glow_background",
			Kind:     PublicKindFirmwarePreset,
			Playable: false,
			EffectID: bytePtr(4),
			Interval: durationPtr(500 * time.Millisecond),
			Color:    rgbPtr(RGB{R: 1, G: 2, B: 3}),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Catalog() = %+v, want %+v", got, want)
	}

	if got, want := registry.IDs(), []string{"matrix_rain_background", "notification", "slow_glow_background"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for id := range presets {
		entry, ok := registry.Entry(id)
		if !ok {
			t.Fatalf("Entry(%q) ok = false, want true", id)
		}
		if entry.Kind != EntryFirmwarePreset || entry.Animation != nil {
			t.Fatalf("Entry(%q) = %+v, want metadata-only firmware preset", id, entry)
		}
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

func bytePtr(value byte) *byte {
	return &value
}

func durationPtr(value time.Duration) *time.Duration {
	return &value
}

func rgbPtr(value RGB) *RGB {
	return &value
}
