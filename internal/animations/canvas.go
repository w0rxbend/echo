package animations

import (
	"fmt"
	"time"
)

const (
	CanvasWidth  = 8
	CanvasHeight = 8
	PixelCount   = CanvasWidth * CanvasHeight
)

type Canvas8x8 struct {
	pixels [PixelCount]RGB
	delay  time.Duration
}

func NewCanvas8x8(delay time.Duration) Canvas8x8 {
	return Canvas8x8{delay: delay}
}

func NewFrame(delay time.Duration) Frame {
	return Frame{Delay: delay}
}

func (c *Canvas8x8) Set(x, y int, color RGB) error {
	idx, err := pixelOffset(x, y)
	if err != nil {
		return err
	}
	c.pixels[idx] = color
	return nil
}

func (c *Canvas8x8) SetRGB(x, y int, r, g, b byte) error {
	return c.Set(x, y, RGB{R: r, G: g, B: b})
}

func (c *Canvas8x8) Pixel(x, y int) (RGB, error) {
	idx, err := pixelOffset(x, y)
	if err != nil {
		return RGB{}, err
	}
	return c.pixels[idx], nil
}

func (c *Canvas8x8) Fill(color RGB) {
	for i := range c.pixels {
		c.pixels[i] = color
	}
}

func (c *Canvas8x8) Clear() {
	c.Fill(RGB{})
}

func (c *Canvas8x8) SetDelay(delay time.Duration) {
	c.delay = delay
}

func (c Canvas8x8) Delay() time.Duration {
	return c.delay
}

func (c Canvas8x8) Frame() Frame {
	return Frame{
		Pixels: c.pixels,
		Delay:  c.delay,
	}
}

func (f *Frame) SetPixel(x, y int, color RGB) error {
	idx, err := pixelOffset(x, y)
	if err != nil {
		return err
	}
	f.Pixels[idx] = color
	return nil
}

func (f *Frame) Fill(color RGB) {
	for i := range f.Pixels {
		f.Pixels[i] = color
	}
}

func pixelOffset(x, y int) (int, error) {
	if x < 0 || x >= CanvasWidth || y < 0 || y >= CanvasHeight {
		return 0, fmt.Errorf("display pixel (%d,%d) outside %dx%d canvas", x, y, CanvasWidth, CanvasHeight)
	}
	return y*CanvasWidth + x, nil
}
