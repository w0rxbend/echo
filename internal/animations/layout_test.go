package animations

import (
	"bytes"
	"testing"
	"time"
)

func TestDefaultPackerOriginMapsToFirstTriple(t *testing.T) {
	frame := NewFrame(time.Second)
	red := RGB{R: 255, G: 0, B: 0}
	if err := frame.SetPixel(0, 0, red); err != nil {
		t.Fatal(err)
	}

	packed := DefaultPacker().Pack(frame)
	assertPackedPixel(t, packed, 0, red)
	assertOnlyPackedPixel(t, packed, 0)
}

func TestDefaultLayoutOddRowCompensation(t *testing.T) {
	layout := DefaultLayout()

	serverX, serverY, err := layout.DisplayToServerPoint(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if serverX != 7 || serverY != 1 {
		t.Fatalf("display (0,1) server point = (%d,%d), want (7,1)", serverX, serverY)
	}

	physicalIndex, err := layout.DisplayPointToPhysicalIndex(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if physicalIndex != 8 {
		t.Fatalf("display (0,1) physical index = %d, want 8", physicalIndex)
	}

	unflipped, err := NewLayout(CanvasWidth, CanvasHeight, WiringHorizontalTopLeft, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	physicalIndex, err = unflipped.DisplayPointToPhysicalIndex(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if physicalIndex != 15 {
		t.Fatalf("unflipped display (0,1) physical index = %d, want 15", physicalIndex)
	}
}

func TestDefaultPackerFullAsymmetricFixture(t *testing.T) {
	frame := NewFrame(100 * time.Millisecond)
	for y := 0; y < CanvasHeight; y++ {
		for x := 0; x < CanvasWidth; x++ {
			color := RGB{
				R: byte(x + 1),
				G: byte(y + 11),
				B: byte(y*CanvasWidth + x + 31),
			}
			if err := frame.SetPixel(x, y, color); err != nil {
				t.Fatal(err)
			}
		}
	}

	packed := DefaultPacker().Pack(frame)
	var expected PackedFrame
	for y := 0; y < CanvasHeight; y++ {
		for x := 0; x < CanvasWidth; x++ {
			physicalIndex := y*CanvasWidth + x
			offset := physicalIndex * 3
			expected[offset] = byte(x + 1)
			expected[offset+1] = byte(y + 11)
			expected[offset+2] = byte(y*CanvasWidth + x + 31)
		}
	}

	if !bytes.Equal(packed[:], expected[:]) {
		t.Fatalf("packed asymmetric fixture mismatch\n got: %v\nwant: %v", packed, expected)
	}
}

func TestPackerRotation90CWMovesOriginToTopRight(t *testing.T) {
	// With 90° CW rotation, source (0,0) should appear at display (7,0) (top-right),
	// matching the Python client's rotate_point(0, 0, 90) = (7, 0).
	layout, err := NewLayout(CanvasWidth, CanvasHeight, WiringHorizontalTopLeft, true, 90)
	if err != nil {
		t.Fatal(err)
	}
	packer, err := NewPacker(layout)
	if err != nil {
		t.Fatal(err)
	}

	frame := NewFrame(100 * time.Millisecond)
	red := RGB{R: 255}
	if err := frame.SetPixel(0, 0, red); err != nil {
		t.Fatal(err)
	}

	// After 90° CW rotation, source (0,0) maps to display (7,0).
	// display (7,0) → even row → physical index 7.
	packed := packer.Pack(frame)
	assertPackedPixel(t, packed, 7, red)
	assertOnlyPackedPixel(t, packed, 7)
}

func TestPackerRotation180FlipsFrame(t *testing.T) {
	// With 180° rotation, source (0,0) should appear at display (7,7).
	layout, err := NewLayout(CanvasWidth, CanvasHeight, WiringHorizontalTopLeft, true, 180)
	if err != nil {
		t.Fatal(err)
	}
	packer, err := NewPacker(layout)
	if err != nil {
		t.Fatal(err)
	}

	frame := NewFrame(100 * time.Millisecond)
	red := RGB{R: 255}
	if err := frame.SetPixel(0, 0, red); err != nil {
		t.Fatal(err)
	}

	// After 180° rotation, source (0,0) → display (7,7).
	// display (7,7) with odd_row_display_flip=true: odd row, serverX = 7-7 = 0
	// physical index = 7*8 + (8-1-0) = 56 + 7 = 63.
	packed := packer.Pack(frame)
	assertPackedPixel(t, packed, 63, red)
	assertOnlyPackedPixel(t, packed, 63)
}

func assertPackedPixel(t *testing.T, packed PackedFrame, physicalIndex int, want RGB) {
	t.Helper()
	offset := physicalIndex * 3
	got := RGB{R: packed[offset], G: packed[offset+1], B: packed[offset+2]}
	if got != want {
		t.Fatalf("packed pixel %d = %+v, want %+v", physicalIndex, got, want)
	}
}

func assertOnlyPackedPixel(t *testing.T, packed PackedFrame, physicalIndex int) {
	t.Helper()
	for i := 0; i < PixelCount; i++ {
		offset := i * 3
		got := RGB{R: packed[offset], G: packed[offset+1], B: packed[offset+2]}
		if i == physicalIndex {
			if got == (RGB{}) {
				t.Fatalf("packed pixel %d is unexpectedly blank", i)
			}
			continue
		}
		if got != (RGB{}) {
			t.Fatalf("packed pixel %d = %+v, want blank", i, got)
		}
	}
}
