package animations

import (
	"fmt"
	"strings"
)

// ParseHexRGB parses a #RRGGBB colour. This is the canonical palette-colour parser
// for the project: config-authored animations and the animation-upload API both use
// it, so both accept exactly the same colour vocabulary.
func ParseHexRGB(text string) (RGB, error) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) != 7 || trimmed[0] != '#' {
		return RGB{}, fmt.Errorf("color must be #RRGGBB: %q", text)
	}
	var values [3]byte
	for i := 0; i < 3; i++ {
		offset := 1 + i*2
		value, ok := parseHexByte(trimmed[offset : offset+2])
		if !ok {
			return RGB{}, fmt.Errorf("color must be #RRGGBB: %q", text)
		}
		values[i] = value
	}
	return RGB{R: values[0], G: values[1], B: values[2]}, nil
}

func parseHexByte(text string) (byte, bool) {
	high, ok := hexNibble(text[0])
	if !ok {
		return 0, false
	}
	low, ok := hexNibble(text[1])
	if !ok {
		return 0, false
	}
	return high<<4 | low, true
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}
