package animations

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFrameAnimationRendersDisplaySpaceCopies(t *testing.T) {
	blank := RGB{}
	red := RGB{R: 255}
	green := RGB{G: 128}
	blue := RGB{B: 64}
	palette := []FramePaletteEntry{
		{Symbol: ".", Color: blank},
		{Symbol: "R", Color: red},
		{Symbol: "G", Color: green},
		{Symbol: "B", Color: blue},
	}
	specs := []FrameSpec{{
		Delay: 75 * time.Millisecond,
		Rows: []string{
			"R.......",
			".......G",
			"........",
			"........",
			"........",
			"........",
			"..B.....",
			"........",
		},
	}}

	animation, err := NewFrameAnimation(specs, palette)
	if err != nil {
		t.Fatalf("NewFrameAnimation() error = %v", err)
	}

	specs[0].Rows[0] = "GGGGGGGG"
	palette[1].Color = RGB{R: 1}

	frames, err := animation.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("Render() frame count = %d, want 1", len(frames))
	}
	frame := frames[0]
	if frame.Delay != 75*time.Millisecond {
		t.Fatalf("frame delay = %s, want 75ms", frame.Delay)
	}
	assertDisplayPixel(t, frame, 0, 0, red)
	assertDisplayPixel(t, frame, 7, 1, green)
	assertDisplayPixel(t, frame, 2, 6, blue)
	assertDisplayPixel(t, frame, 7, 0, blank)
	assertDisplayPixel(t, frame, 0, 1, blank)

	frames[0].Pixels[0] = blue
	again, err := animation.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Render() error = %v", err)
	}
	assertDisplayPixel(t, again[0], 0, 0, red)
}

func TestFrameAnimationHonorsCanceledContext(t *testing.T) {
	animation, err := NewFrameAnimation([]FrameSpec{validFrameSpec(time.Millisecond)}, validFramePalette())
	if err != nil {
		t.Fatalf("NewFrameAnimation() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = animation.Render(ctx, nil)
	if err == nil {
		t.Fatal("Render() canceled context error = nil, want error")
	}
}

func TestFrameAnimationValidation(t *testing.T) {
	tests := []struct {
		name    string
		specs   []FrameSpec
		palette []FramePaletteEntry
		want    string
	}{
		{
			name:    "empty frame set",
			palette: validFramePalette(),
			want:    "at least one frame",
		},
		{
			name:    "missing palette",
			specs:   []FrameSpec{validFrameSpec(time.Millisecond)},
			palette: nil,
			want:    "palette is required",
		},
		{
			name:    "empty palette symbol",
			specs:   []FrameSpec{validFrameSpec(time.Millisecond)},
			palette: []FramePaletteEntry{{Symbol: "", Color: RGB{}}},
			want:    "symbol is required",
		},
		{
			name:    "ambiguous palette symbol",
			specs:   []FrameSpec{validFrameSpec(time.Millisecond)},
			palette: []FramePaletteEntry{{Symbol: "..", Color: RGB{}}},
			want:    "exactly one character",
		},
		{
			name:    "duplicate palette symbol",
			specs:   []FrameSpec{validFrameSpec(time.Millisecond)},
			palette: []FramePaletteEntry{{Symbol: ".", Color: RGB{}}, {Symbol: ".", Color: RGB{R: 1}}},
			want:    "duplicate palette symbol",
		},
		{
			name:    "zero delay",
			specs:   []FrameSpec{validFrameSpec(0)},
			palette: validFramePalette(),
			want:    "delay must be positive",
		},
		{
			name:    "negative delay",
			specs:   []FrameSpec{validFrameSpec(-time.Millisecond)},
			palette: validFramePalette(),
			want:    "delay must be positive",
		},
		{
			name: "wrong row count",
			specs: []FrameSpec{{
				Delay: time.Millisecond,
				Rows:  []string{"........"},
			}},
			palette: validFramePalette(),
			want:    "exactly 8 rows",
		},
		{
			name: "wrong row width",
			specs: []FrameSpec{{
				Delay: time.Millisecond,
				Rows: []string{
					".......",
					"........",
					"........",
					"........",
					"........",
					"........",
					"........",
					"........",
				},
			}},
			palette: validFramePalette(),
			want:    "exactly 8 symbols",
		},
		{
			name: "unknown symbol",
			specs: []FrameSpec{{
				Delay: time.Millisecond,
				Rows: []string{
					"X.......",
					"........",
					"........",
					"........",
					"........",
					"........",
					"........",
					"........",
				},
			}},
			palette: validFramePalette(),
			want:    "unknown palette symbol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFrameAnimation(tt.specs, tt.palette)
			if err == nil {
				t.Fatal("NewFrameAnimation() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewFrameAnimation() error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func validFrameSpec(delay time.Duration) FrameSpec {
	return FrameSpec{
		Delay: delay,
		Rows: []string{
			"........",
			"........",
			"........",
			"........",
			"........",
			"........",
			"........",
			"........",
		},
	}
}

func validFramePalette() []FramePaletteEntry {
	return []FramePaletteEntry{{Symbol: ".", Color: RGB{}}}
}

func assertDisplayPixel(t *testing.T, frame Frame, x, y int, want RGB) {
	t.Helper()
	got := frame.Pixels[y*CanvasWidth+x]
	if got != want {
		t.Fatalf("display pixel (%d,%d) = %+v, want %+v", x, y, got, want)
	}
}
