package animations

import (
	"context"
	"fmt"
	"unicode/utf8"
)

type frameAnimation struct {
	frames []Frame
}

// NewFrameAnimation builds a generated animation from 8x8 display-space rows.
// Symbols are never implicit: callers must include every usable symbol in the
// palette, including "." if it should mean a blank black pixel.
func NewFrameAnimation(specs []FrameSpec, palette []FramePaletteEntry) (Animation, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("frame animation requires at least one frame")
	}
	colors, err := framePaletteColors(palette)
	if err != nil {
		return nil, err
	}

	frames := make([]Frame, 0, len(specs))
	for frameIndex, spec := range specs {
		if spec.Delay <= 0 {
			return nil, fmt.Errorf("frame %d delay must be positive", frameIndex)
		}
		if len(spec.Rows) != CanvasHeight {
			return nil, fmt.Errorf("frame %d must have exactly %d rows, got %d", frameIndex, CanvasHeight, len(spec.Rows))
		}

		frame := NewFrame(spec.Delay)
		for y, row := range spec.Rows {
			if !utf8.ValidString(row) {
				return nil, fmt.Errorf("frame %d row %d must be valid UTF-8", frameIndex, y)
			}
			symbols := []rune(row)
			if len(symbols) != CanvasWidth {
				return nil, fmt.Errorf("frame %d row %d must have exactly %d symbols, got %d", frameIndex, y, CanvasWidth, len(symbols))
			}
			for x, symbol := range symbols {
				color, ok := colors[symbol]
				if !ok {
					return nil, fmt.Errorf("frame %d row %d column %d uses unknown palette symbol %q", frameIndex, y, x, string(symbol))
				}
				if err := frame.SetPixel(x, y, color); err != nil {
					return nil, err
				}
			}
		}
		frames = append(frames, frame)
	}

	return frameAnimation{frames: frames}, nil
}

func framePaletteColors(palette []FramePaletteEntry) (map[rune]RGB, error) {
	if len(palette) == 0 {
		return nil, fmt.Errorf("frame animation palette is required")
	}
	colors := make(map[rune]RGB, len(palette))
	for i, entry := range palette {
		symbol, err := framePaletteSymbol(entry.Symbol)
		if err != nil {
			return nil, fmt.Errorf("palette entry %d: %w", i, err)
		}
		if _, exists := colors[symbol]; exists {
			return nil, fmt.Errorf("duplicate palette symbol %q", entry.Symbol)
		}
		colors[symbol] = entry.Color
	}
	return colors, nil
}

func framePaletteSymbol(symbol string) (rune, error) {
	if symbol == "" {
		return 0, fmt.Errorf("palette symbol is required")
	}
	if !utf8.ValidString(symbol) {
		return 0, fmt.Errorf("palette symbol %q must be valid UTF-8", symbol)
	}
	value, size := utf8.DecodeRuneInString(symbol)
	if value == utf8.RuneError && size == 0 {
		return 0, fmt.Errorf("palette symbol is required")
	}
	if size != len(symbol) {
		return 0, fmt.Errorf("palette symbol %q must be exactly one character", symbol)
	}
	return value, nil
}

func (a frameAnimation) Render(ctx context.Context, _ Params) ([]Frame, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	frames := make([]Frame, len(a.frames))
	copy(frames, a.frames)
	return frames, nil
}

var _ Animation = frameAnimation{}
