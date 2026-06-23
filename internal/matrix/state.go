package matrix

import (
	"context"
	"sync"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

type RGB = animations.RGB
type Frame = animations.Frame
type PackedFrame = animations.PackedFrame
type InterruptMode = animations.InterruptMode
type RestorePolicy = animations.RestorePolicy
type LoopPolicy = animations.LoopPolicy

type State string

const (
	StateDisconnected        State = "disconnected"
	StateConnecting          State = "connecting"
	StateReady               State = "ready"
	StatePlayingTransient    State = "playing_transient"
	StateRestoringBackground State = "restoring_background"
	StateDraining            State = "draining"
)

type ReconnectSource string

const (
	// ReconnectSourceSchedulerBackoff identifies delayed scheduler reconnect
	// loops after retryable matrix failures.
	ReconnectSourceSchedulerBackoff ReconnectSource = "scheduler_backoff"
	// ReconnectSourceTCPImmediate identifies the TCP client's immediate
	// replacement-dial path after a retryable socket error on an in-flight
	// command.
	ReconnectSourceTCPImmediate ReconnectSource = "tcp_immediate"
)

type BackgroundKind string

const (
	BackgroundKindFirmwarePreset BackgroundKind = "firmware_preset"
	BackgroundKindRenderable     BackgroundKind = "renderable"
)

type BackgroundConvergenceState string

const (
	BackgroundConvergenceUnknown    BackgroundConvergenceState = "unknown"
	BackgroundConvergenceDirty      BackgroundConvergenceState = "dirty"
	BackgroundConvergenceAttempting BackgroundConvergenceState = "attempting"
	BackgroundConvergenceConverged  BackgroundConvergenceState = "converged"
	BackgroundConvergenceFailed     BackgroundConvergenceState = "failed"
	BackgroundConvergenceRetrying   BackgroundConvergenceState = "retrying"
)

// backgroundConvergenceV1States is the v1 public bounded convergence state
// vocabulary. Do not emit or document any additional states in scheduler
// health fields, /readyz.background, or matrix_proxy_background_state.
var backgroundConvergenceV1States = []BackgroundConvergenceState{
	BackgroundConvergenceUnknown,
	BackgroundConvergenceDirty,
	BackgroundConvergenceAttempting,
	BackgroundConvergenceConverged,
	BackgroundConvergenceFailed,
	BackgroundConvergenceRetrying,
}

// BackgroundConvergenceV1States returns the bounded state vocabulary used by the
// shared v1 background convergence contract.
func BackgroundConvergenceV1States() []BackgroundConvergenceState {
	states := make([]BackgroundConvergenceState, len(backgroundConvergenceV1States))
	copy(states, backgroundConvergenceV1States)
	return states
}

type BackgroundConvergenceProjectionInput struct {
	State                 BackgroundConvergenceState
	Dirty                 bool
	LastRestoreError      string
	LastRestoreErrorClass ErrorKind
	NextRetry             *time.Time
	FailureCount          int
}

type BackgroundConvergenceProjection struct {
	State     BackgroundConvergenceState
	Dirty     bool
	Converged bool
}

// ProjectBackgroundConvergence maps internal scheduler background state into the
// v1 public bounded vocabulary: unknown, dirty, attempting, converged,
// failed, retrying. The output is shared across scheduler health, /readyz
// background payloads, and matrix_proxy_background_state.
//
// Transition invariants:
// - retrying: dirty background with a future next_retry deadline.
// - failed: dirty background due for retry and no future suppression deadline.
// - attempting: restore command currently in progress.
func ProjectBackgroundConvergence(input BackgroundConvergenceProjectionInput, now time.Time) BackgroundConvergenceProjection {
	if input.State == "" {
		input.State = BackgroundConvergenceUnknown
	}
	if input.State == BackgroundConvergenceAttempting {
		return BackgroundConvergenceProjection{State: input.State, Dirty: input.Dirty}
	}
	if input.State == BackgroundConvergenceConverged && !input.Dirty {
		return BackgroundConvergenceProjection{State: input.State, Converged: true}
	}
	if !input.Dirty {
		return BackgroundConvergenceProjection{State: input.State}
	}
	if input.NextRetry != nil && now.Before(*input.NextRetry) {
		return BackgroundConvergenceProjection{State: BackgroundConvergenceRetrying, Dirty: true}
	}
	if input.FailureCount > 0 || input.LastRestoreError != "" || input.LastRestoreErrorClass == ErrorKindRetryable || input.LastRestoreErrorClass == ErrorKindPermanent {
		return BackgroundConvergenceProjection{State: BackgroundConvergenceFailed, Dirty: true}
	}
	return BackgroundConvergenceProjection{State: BackgroundConvergenceDirty, Dirty: true}
}

const (
	ObservabilityCallbackCommandDone           = "command_done"
	ObservabilityCallbackReconnectAttempt      = "reconnect_attempt"
	ObservabilityCallbackReconnectDelay        = "reconnect_delay"
	ObservabilityCallbackReconnectRecovered    = "reconnect_recovered"
	ObservabilityCallbackReconnectFailure      = "reconnect_failure"
	ObservabilityCallbackProbeFailure          = "probe_failure"
	ObservabilityCallbackMatrixConnectedChange = "matrix_connected_change"
	ObservabilityCallbackBackgroundRestore     = "background_restore"
)

type ReconnectFailureOutcome string

const (
	// ReconnectFailureCanceled means a reconnect attempt was terminated by
	// context cancellation before firmware-verified replacement connectivity.
	ReconnectFailureCanceled ReconnectFailureOutcome = "canceled"
	// ReconnectFailureDeadlineExceeded means a reconnect attempt exceeded its
	// context or item deadline before firmware verification completed.
	ReconnectFailureDeadlineExceeded ReconnectFailureOutcome = "deadline_exceeded"
	// ReconnectFailureFailed means the reconnect attempt did not establish
	// firmware-verified replacement connectivity because of a bounded failure
	// such as transport loss or dial failure.
	ReconnectFailureFailed ReconnectFailureOutcome = "failed"
	// ReconnectFailureVerificationFailed means a replacement TCP connection was
	// opened, but firmware verification on that connection returned a permanent
	// status, protocol, or validation error. The connection must not be kept.
	ReconnectFailureVerificationFailed ReconnectFailureOutcome = "verification_failed"
)

type ProbeFailureReason string

const (
	ProbeFailureProbeTimeout ProbeFailureReason = "probe_timeout"
	ProbeFailureTransport    ProbeFailureReason = "transport"
	ProbeFailurePermanent    ProbeFailureReason = "permanent"
)

type Health struct {
	State                           State                      `json:"state"`
	MatrixConnected                 bool                       `json:"matrix_connected"`
	BackgroundID                    string                     `json:"background_id,omitempty"`
	BackgroundKind                  BackgroundKind             `json:"background_kind,omitempty"`
	BackgroundConvergenceState      BackgroundConvergenceState `json:"background_convergence_state"`
	BackgroundDirty                 bool                       `json:"background_dirty"`
	BackgroundConverged             bool                       `json:"background_converged"`
	BackgroundLastRestoreAttempt    *time.Time                 `json:"background_last_restore_attempt,omitempty"`
	BackgroundLastRestoreSuccess    *time.Time                 `json:"background_last_restore_success,omitempty"`
	BackgroundLastRestoreError      string                     `json:"background_last_restore_error,omitempty"`
	BackgroundLastRestoreErrorClass ErrorKind                  `json:"background_last_restore_error_class,omitempty"`
	BackgroundNextRetry             *time.Time                 `json:"background_next_retry,omitempty"`
	BackgroundRetryFailureCount     int                        `json:"background_retry_failure_count"`
	OutcomeReportsDropped           uint64                     `json:"outcome_reports_dropped"`
	OutcomeRecordingPanics          uint64                     `json:"outcome_recording_panics"`
	ObservabilityCallbackPanics     uint64                     `json:"observability_callback_panics"`
	ObservabilityCallbackCounts     map[string]uint64          `json:"observability_callback_panic_counts,omitempty"`
	LastSuccess                     *time.Time                 `json:"last_success,omitempty"`
	LastFailure                     *time.Time                 `json:"last_failure,omitempty"`
}

type QueueItemKind string

const (
	QueueItemAnimation QueueItemKind = "animation"
	QueueItemControl   QueueItemKind = "control"
)

type ItemOutcome string

const (
	ItemOutcomeExecuted         ItemOutcome = "executed"
	ItemOutcomeExpired          ItemOutcome = "expired"
	ItemOutcomeCanceled         ItemOutcome = "canceled"
	ItemOutcomeDropped          ItemOutcome = "dropped"
	ItemOutcomeQueueCleared     ItemOutcome = "queue_cleared"
	ItemOutcomeSchedulerStopped ItemOutcome = "scheduler_stopped"
	ItemOutcomePermanentError   ItemOutcome = "permanent_error"
)

// OutcomeReport is a terminal scheduler lifecycle report for one admitted or
// resolved item. The scheduler emits at most one report per item, after the
// terminal outcome is known. Reporting is observational only: an observer must
// never be required for queue mutation, control completion, playback,
// shutdown, or scheduler state correctness.
type OutcomeReport struct {
	Outcome               ItemOutcome   `json:"outcome"`
	ItemKind              QueueItemKind `json:"item_kind"`
	ItemID                string        `json:"item_id,omitempty"`
	EventID               string        `json:"event_id,omitempty"`
	AnimationID           string        `json:"animation_id,omitempty"`
	ControlKind           ControlKind   `json:"control_kind,omitempty"`
	Priority              int           `json:"priority"`
	QueueDepthBeforeClear int           `json:"queue_depth_before_clear,omitempty"`
	QueueDepthAtAdmission int           `json:"queue_depth_at_admission,omitempty"`
	QueueDepthAtRemoval   int           `json:"queue_depth_at_removal,omitempty"`
	Reason                string        `json:"reason,omitempty"`
	ErrorClass            ErrorKind     `json:"error_class,omitempty"`
	Timestamp             time.Time     `json:"timestamp"`
}

type QueueItemStatus struct {
	Kind          QueueItemKind            `json:"kind"`
	ID            string                   `json:"id"`
	EventID       string                   `json:"event_id,omitempty"`
	AnimationID   string                   `json:"animation_id,omitempty"`
	Priority      int                      `json:"priority"`
	RestorePolicy animations.RestorePolicy `json:"restore_policy,omitempty"`
	CreatedAt     time.Time                `json:"created_at,omitempty"`
	Deadline      time.Time                `json:"deadline,omitempty"`
	Control       *QueueControlStatus      `json:"control,omitempty"`
}

type QueueControlStatus struct {
	ID         string        `json:"id,omitempty"`
	Kind       ControlKind   `json:"kind"`
	Priority   int           `json:"priority"`
	Brightness byte          `json:"brightness,omitempty"`
	EffectID   byte          `json:"effect_id,omitempty"`
	Interval   time.Duration `json:"interval,omitempty"`
	Color      RGB           `json:"color,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitempty"`
	Deadline   time.Time     `json:"deadline,omitempty"`
}

type ControlKind string

const (
	ControlClear         ControlKind = "clear"
	ControlSetBrightness ControlKind = "brightness"
	ControlSetPreset     ControlKind = "preset"
	ControlFill          ControlKind = "fill"
)

type PlayItem struct {
	ID       string     `json:"id"`
	EventID  string     `json:"event_id,omitempty"`
	Priority int        `json:"priority"`
	Frames   []Frame    `json:"-"`
	Loop     LoopPolicy `json:"loop"`
	Deadline time.Time  `json:"deadline,omitempty"`
	OnStart  Hook       `json:"-"`
	OnFinish Hook       `json:"-"`
}

type Hook func(context.Context) error

type ControlRequest struct {
	ID         string        `json:"id,omitempty"`
	Kind       ControlKind   `json:"kind"`
	Priority   int           `json:"priority"`
	Brightness byte          `json:"brightness,omitempty"`
	EffectID   byte          `json:"effect_id,omitempty"`
	Interval   time.Duration `json:"interval,omitempty"`
	Color      RGB           `json:"color,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitempty"`
	Deadline   time.Time     `json:"deadline,omitempty"`
}

type ControlItem struct {
	ID         string        `json:"id,omitempty"`
	Kind       ControlKind   `json:"kind"`
	Priority   int           `json:"priority"`
	Brightness byte          `json:"brightness,omitempty"`
	EffectID   byte          `json:"effect_id,omitempty"`
	Interval   time.Duration `json:"interval,omitempty"`
	Color      RGB           `json:"color,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitempty"`
	Deadline   time.Time     `json:"deadline,omitempty"`

	ctx context.Context

	mu        sync.Mutex
	done      chan struct{}
	err       error
	completed bool
}

type displayStateKind string

const (
	displayStateUnknown displayStateKind = ""
	displayStateFrame   displayStateKind = "frame"
	displayStateFill    displayStateKind = "fill"
	displayStateClear   displayStateKind = "clear"
	displayStatePreset  displayStateKind = "preset"
)

type displayState struct {
	Kind         displayStateKind
	Frame        PackedFrame
	Color        RGB
	EffectID     byte
	Interval     time.Duration
	BackgroundID string
}

func (s displayState) known() bool {
	return s.Kind != displayStateUnknown
}
