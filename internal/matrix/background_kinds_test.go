package matrix

import (
	"context"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

// BackgroundKinds is what anything projecting a kind into the public vocabulary
// iterates, so it has to stay in step with what backgroundKindFor can actually
// hand out. A kind produced here but missing from the list would be projected by
// nobody and checked by nobody.
func TestBackgroundKindForOnlyReturnsListedKinds(t *testing.T) {
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("preset", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterStaticColor("colour", RGB{R: 10, G: 20, B: 30}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterGenerated("generated", "test_generator", animations.AnimationFunc(
		func(context.Context, animations.Params) ([]animations.Frame, error) {
			return nil, nil
		},
	)); err != nil {
		t.Fatal(err)
	}

	listed := make(map[BackgroundKind]bool, len(BackgroundKinds()))
	for _, kind := range BackgroundKinds() {
		listed[kind] = true
	}

	produced := make(map[BackgroundKind]bool, len(listed))
	for _, id := range []string{"preset", "colour", "generated"} {
		kind := backgroundKindFor(BackgroundConfig{AnimationID: id}, registry)
		if !listed[kind] {
			t.Fatalf("backgroundKindFor(%q) = %q, which BackgroundKinds does not list", id, kind)
		}
		produced[kind] = true
	}

	// The other direction: a kind listed but no longer produced is dead weight
	// that would keep a stale projection entry looking justified.
	for kind := range listed {
		if !produced[kind] {
			t.Fatalf("BackgroundKinds lists %q, which backgroundKindFor never returns", kind)
		}
	}

	// The empty kind means "no background configured" and is deliberately not a
	// member: projecting it must fail, not blank out a label.
	if kind := backgroundKindFor(BackgroundConfig{}, registry); kind != "" {
		t.Fatalf("backgroundKindFor(no animation) = %q, want the empty kind", kind)
	}
	if listed[""] {
		t.Fatal("BackgroundKinds lists the empty kind")
	}
}

func TestBackgroundKindsAreDistinct(t *testing.T) {
	seen := make(map[BackgroundKind]bool)
	for _, kind := range BackgroundKinds() {
		if seen[kind] {
			t.Fatalf("BackgroundKinds lists %q twice", kind)
		}
		seen[kind] = true
	}
	if len(seen) == 0 {
		t.Fatal("BackgroundKinds is empty")
	}
}

// The slice is copied on the way out so a caller ranging over it cannot edit the
// list every exhaustiveness check reads.
func TestBackgroundKindsReturnsACopy(t *testing.T) {
	kinds := BackgroundKinds()
	kinds[0] = "mutated"
	if BackgroundKinds()[0] == "mutated" {
		t.Fatal("BackgroundKinds returned the backing slice")
	}
}
