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
	RestorePreviousFrame RestorePolicy = "previous_frame"
	RestoreBackground    RestorePolicy = "background"
	RestoreLeave         RestorePolicy = "leave"
)

// validRestorePolicies is the canonical set. The HTTP boundary, the rules loader
// and the scheduler all validate against this one map; an unrecognised policy that
// reaches Scheduler.restore is returned as an error that exits Run, so every entry
// point must reject it first.
var validRestorePolicies = map[RestorePolicy]struct{}{
	RestoreClear:         {},
	RestorePreviousFrame: {},
	RestoreBackground:    {},
	RestoreLeave:         {},
}

// IsValidRestorePolicy reports whether the policy is one the scheduler implements.
// The empty policy is accepted: the scheduler treats it as RestoreLeave.
func IsValidRestorePolicy(policy RestorePolicy) bool {
	if policy == "" {
		return true
	}
	_, ok := validRestorePolicies[policy]
	return ok
}

var validLoopPolicies = map[LoopPolicy]struct{}{
	LoopNone:    {},
	LoopForever: {},
	LoopUntil:   {},
}

// IsValidLoopPolicy reports whether the policy is one the scheduler implements.
// The empty policy is accepted and means LoopNone.
func IsValidLoopPolicy(policy LoopPolicy) bool {
	if policy == "" {
		return true
	}
	_, ok := validLoopPolicies[policy]
	return ok
}

// LoopPolicyNames returns the accepted loop names in a stable order, for error
// messages and help text.
func LoopPolicyNames() []string {
	return []string{string(LoopNone), string(LoopForever), string(LoopUntil)}
}

// RestorePolicyNames returns the accepted policy names in a stable order, for
// error messages and help text.
func RestorePolicyNames() []string {
	return []string{
		string(RestoreLeave),
		string(RestoreBackground),
		string(RestoreClear),
		string(RestorePreviousFrame),
	}
}

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
	// Loop controls whether the rendered frames repeat. LoopUntil and LoopForever
	// both require a deadline to terminate, which MaxDuration supplies; without one
	// LoopForever never ends and holds the play queue.
	Loop      LoopPolicy `json:"loop,omitempty" yaml:"loop,omitempty"`
	CreatedAt time.Time  `json:"created_at" yaml:"created_at"`
}

type Animation interface {
	Render(ctx context.Context, params Params) ([]Frame, error)
}
