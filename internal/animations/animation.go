package animations

import (
	"context"
	"time"
)

type RGB struct {
	R byte `json:"r" yaml:"r"`
	G byte `json:"g" yaml:"g"`
	B byte `json:"b" yaml:"b"`
}

type FirmwarePreset struct {
	EffectID byte          `json:"effect_id" yaml:"effect_id"`
	Interval time.Duration `json:"interval" yaml:"interval"`
	Color    RGB           `json:"color" yaml:"color"`
}

type Frame struct {
	Pixels [64]RGB       `json:"pixels" yaml:"pixels"`
	Delay  time.Duration `json:"delay" yaml:"delay"`
}

type FramePaletteEntry struct {
	Symbol string
	Color  RGB
}

type FrameSpec struct {
	Delay time.Duration
	Rows  []string
}

type PackedFrame [192]byte

type Params map[string]string

type InterruptMode string

const (
	InterruptNone           InterruptMode = "none"
	InterruptHigherPriority InterruptMode = "higher_priority"
	InterruptCritical       InterruptMode = "critical"
)

type RestorePolicy string

const (
	RestoreClear         RestorePolicy = "clear"
	RestoreBlank         RestorePolicy = "blank"
	RestorePreviousFrame RestorePolicy = "previous_frame"
	RestoreBackground    RestorePolicy = "background"
	RestoreLeave         RestorePolicy = "leave"
)

type LoopPolicy string

const (
	LoopNone    LoopPolicy = "none"
	LoopForever LoopPolicy = "forever"
	LoopUntil   LoopPolicy = "until_deadline"
)

type AnimationRequest struct {
	ID            string        `json:"id" yaml:"id"`
	EventID       string        `json:"event_id" yaml:"event_id"`
	AnimationID   string        `json:"animation_id" yaml:"animation_id"`
	Params        Params        `json:"params,omitempty" yaml:"params,omitempty"`
	Priority      int           `json:"priority,omitempty" yaml:"priority,omitempty"`
	MaxDuration   time.Duration `json:"max_duration,omitempty" yaml:"max_duration,omitempty"`
	InterruptMode InterruptMode `json:"interrupt_mode,omitempty" yaml:"interrupt_mode,omitempty"`
	RestorePolicy RestorePolicy `json:"restore_policy,omitempty" yaml:"restore_policy,omitempty"`
	CreatedAt     time.Time     `json:"created_at" yaml:"created_at"`
}

type Animation interface {
	Render(ctx context.Context, params Params) ([]Frame, error)
}
