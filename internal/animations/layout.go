package animations

import "fmt"

const WiringHorizontalTopLeft = "h-tl"

type Layout struct {
	Width             int
	Height            int
	Wiring            string
	OddRowDisplayFlip bool
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
	}
}

func NewLayout(width, height int, wiring string, oddRowDisplayFlip bool) (Layout, error) {
	layout := Layout{
		Width:             width,
		Height:            height,
		Wiring:            wiring,
		OddRowDisplayFlip: oddRowDisplayFlip,
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
	return nil
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
	for y := 0; y < layout.Height; y++ {
		for x := 0; x < layout.Width; x++ {
			physicalIndex, err := layout.DisplayPointToPhysicalIndex(x, y)
			if err != nil {
				panic(err)
			}
			displayIndex := y*layout.Width + x
			offset := physicalIndex * 3
			pixel := frame.Pixels[displayIndex]
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
