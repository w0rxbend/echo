package animations

import "fmt"

const WiringHorizontalTopLeft = "h-tl"

// ValidRotations are the supported display rotation values in degrees.
// A non-zero rotation compensates for the physical mounting orientation of
// the matrix so that authored animations appear upright on the panel.
// Positive values rotate content clockwise; negative values rotate CCW.
var ValidRotations = []int{-90, 0, 90, 180}

type Layout struct {
	Width             int
	Height            int
	Wiring            string
	OddRowDisplayFlip bool
	// Rotation is the clockwise rotation in degrees applied to every frame
	// before physical LED mapping. Use 0 for the default upright mounting.
	// Valid values: -90, 0, 90, 180.
	Rotation int
}

type LayoutPacker struct {
	layout Layout
}

func DefaultLayout() Layout {
	return Layout{
		Width:             CanvasWidth,
		Height:            CanvasHeight,
		Wiring:            WiringHorizontalTopLeft,
		OddRowDisplayFlip: true,
		Rotation:          0,
	}
}

func NewLayout(width, height int, wiring string, oddRowDisplayFlip bool, rotation int) (Layout, error) {
	layout := Layout{
		Width:             width,
		Height:            height,
		Wiring:            wiring,
		OddRowDisplayFlip: oddRowDisplayFlip,
		Rotation:          rotation,
	}
	if err := layout.Validate(); err != nil {
		return Layout{}, err
	}
	return layout, nil
}

func (l Layout) Validate() error {
	if l.Width != CanvasWidth || l.Height != CanvasHeight {
		return fmt.Errorf("layout must be %dx%d, got %dx%d", CanvasWidth, CanvasHeight, l.Width, l.Height)
	}
	if l.Wiring != WiringHorizontalTopLeft {
		return fmt.Errorf("unsupported matrix wiring %q", l.Wiring)
	}
	if !validRotation(l.Rotation) {
		return fmt.Errorf("layout rotation must be one of -90, 0, 90, 180: got %d", l.Rotation)
	}
	return nil
}

func validRotation(r int) bool {
	for _, v := range ValidRotations {
		if v == r {
			return true
		}
	}
	return false
}

// rotatePoint maps a source frame coordinate (x, y) to the display coordinate
// it should appear at after clockwise rotation by degrees. This matches the
// Python client's rotate_point function in tools/matrix_client.py.
func rotatePoint(x, y, rotation, width, height int) (int, int) {
	switch rotation {
	case 90:
		return width - 1 - y, x
	case -90:
		return y, height - 1 - x
	case 180:
		return width - 1 - x, height - 1 - y
	default:
		return x, y
	}
}

func (l Layout) DisplayToServerPoint(x, y int) (int, int, error) {
	if _, err := pixelOffset(x, y); err != nil {
		return 0, 0, err
	}
	serverX := x
	if l.OddRowDisplayFlip && y%2 == 1 {
		serverX = (l.Width - 1) - x
	}
	return serverX, y, nil
}

func (l Layout) ServerPointToPhysicalIndex(serverX, y int) (int, error) {
	if err := l.Validate(); err != nil {
		return 0, err
	}
	if serverX < 0 || serverX >= l.Width || y < 0 || y >= l.Height {
		return 0, fmt.Errorf("server pixel (%d,%d) outside %dx%d layout", serverX, y, l.Width, l.Height)
	}
	if y%2 == 0 {
		return y*l.Width + serverX, nil
	}
	return y*l.Width + ((l.Width - 1) - serverX), nil
}

func (l Layout) DisplayPointToPhysicalIndex(x, y int) (int, error) {
	serverX, serverY, err := l.DisplayToServerPoint(x, y)
	if err != nil {
		return 0, err
	}
	return l.ServerPointToPhysicalIndex(serverX, serverY)
}

func DefaultPacker() LayoutPacker {
	return LayoutPacker{layout: DefaultLayout()}
}

func NewPacker(layout Layout) (LayoutPacker, error) {
	if err := layout.Validate(); err != nil {
		return LayoutPacker{}, err
	}
	return LayoutPacker{layout: layout}, nil
}

func (p LayoutPacker) Layout() Layout {
	return p.layoutOrDefault()
}

func (p LayoutPacker) Pack(frame Frame) PackedFrame {
	layout := p.layoutOrDefault()
	var packed PackedFrame
	for srcY := 0; srcY < layout.Height; srcY++ {
		for srcX := 0; srcX < layout.Width; srcX++ {
			// Rotate the source frame coordinate to its display position.
			dispX, dispY := rotatePoint(srcX, srcY, layout.Rotation, layout.Width, layout.Height)
			physicalIndex, err := layout.DisplayPointToPhysicalIndex(dispX, dispY)
			if err != nil {
				panic(err)
			}
			srcIndex := srcY*layout.Width + srcX
			offset := physicalIndex * 3
			pixel := frame.Pixels[srcIndex]
			packed[offset] = pixel.R
			packed[offset+1] = pixel.G
			packed[offset+2] = pixel.B
		}
	}
	return packed
}

func (p LayoutPacker) PackCanvas(canvas Canvas8x8) PackedFrame {
	return p.Pack(canvas.Frame())
}

func (p LayoutPacker) layoutOrDefault() Layout {
	if p.layout.Width == 0 && p.layout.Height == 0 && p.layout.Wiring == "" {
		return DefaultLayout()
	}
	return p.layout
}
