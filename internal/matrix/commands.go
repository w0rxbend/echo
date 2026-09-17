package matrix

import (
	"context"
	"fmt"
	"time"
)

const (
	framePayloadSize       = 192
	customFramePayloadSize = 196
	maxMilliseconds        = 65535

	// MaxAnimationFrames mirrors AppConfig::kMaxCustomFrames in the firmware.
	// The device holds this many frames in its custom-animation slot; uploading
	// more would be rejected with Status::kInvalidLength.
	MaxAnimationFrames = 8

	// MinAnimationFrameDelay mirrors AppConfig::kMinEffectFrameDelayMs in the
	// firmware. EffectEngine::clampDelayMs silently raises any shorter delay to
	// this floor (and rewrites a zero delay to kDefaultPresetIntervalMs), so the
	// device answers Status::kOk and then plays the animation slower than asked.
	// Reject it here instead of reporting success for a speed the panel will not
	// actually run.
	//
	// The floor belongs to the firmware-resident custom-animation slot only.
	// Scheduler.playFrames drives frames from the host and sleeps for exactly the
	// delay each frame declares, so config-authored animations are not clamped and
	// keep their existing timings.
	MinAnimationFrameDelay = 20 * time.Millisecond
)

func (c *TCPClient) Ping(ctx context.Context) error {
	return c.sendCommand(ctx, commandPing, nil)
}

func (c *TCPClient) Clear(ctx context.Context) error {
	return c.sendCommand(ctx, commandClear, nil)
}

func (c *TCPClient) SetBrightness(ctx context.Context, value byte) error {
	return c.sendCommand(ctx, commandSetBrightness, []byte{value})
}

func (c *TCPClient) Fill(ctx context.Context, color RGB) error {
	return c.sendCommand(ctx, commandFill, rgbPayload(color))
}

func (c *TCPClient) SetPixel(ctx context.Context, x, y byte, color RGB) error {
	return c.sendCommand(ctx, commandSetPixel, []byte{x, y, color.R, color.G, color.B})
}

func (c *TCPClient) SetFrame(ctx context.Context, frame PackedFrame) error {
	payload := make([]byte, framePayloadSize)
	copy(payload, frame[:])
	return c.sendCommand(ctx, commandSetFrame, payload)
}

func (c *TCPClient) SetPanelEnabled(ctx context.Context, enabled bool) error {
	if enabled {
		return c.sendCommand(ctx, commandSetPanelEnabled, []byte{1})
	}
	return c.sendCommand(ctx, commandSetPanelEnabled, []byte{0})
}

func (c *TCPClient) SetStaticColor(ctx context.Context, color RGB) error {
	return c.sendCommand(ctx, commandSetStaticColor, rgbPayload(color))
}

func (c *TCPClient) SetPreset(ctx context.Context, effectID byte, interval time.Duration, color RGB) error {
	intervalMS, err := durationMilliseconds(interval, "preset interval")
	if err != nil {
		return err
	}
	payload := []byte{
		effectID,
		byte(intervalMS),
		byte(intervalMS >> 8),
		color.R,
		color.G,
		color.B,
	}
	return c.sendCommand(ctx, commandSetPresetEffect, payload)
}

func (c *TCPClient) UploadCustomFrame(ctx context.Context, index, count byte, delay time.Duration, frame PackedFrame) error {
	delayMS, err := durationMilliseconds(delay, "custom frame delay")
	if err != nil {
		return err
	}
	payload := make([]byte, customFramePayloadSize)
	payload[0] = index
	payload[1] = count
	payload[2] = byte(delayMS)
	payload[3] = byte(delayMS >> 8)
	copy(payload[4:], frame[:])
	return c.sendCommand(ctx, commandUploadCustomFrame, payload)
}

func (c *TCPClient) StopEffect(ctx context.Context) error {
	return c.sendCommand(ctx, commandStopEffect, nil)
}

func rgbPayload(color RGB) []byte {
	return []byte{color.R, color.G, color.B}
}

func durationMilliseconds(duration time.Duration, name string) (uint16, error) {
	if duration < 0 {
		return 0, fmt.Errorf("%w: %s cannot be negative: %s", ErrInvalidDuration, name, duration)
	}
	ms := duration.Milliseconds()
	if ms > maxMilliseconds {
		return 0, fmt.Errorf("%w: %s must be <= %dms: %s", ErrInvalidDuration, name, maxMilliseconds, duration)
	}
	return uint16(ms), nil
}
