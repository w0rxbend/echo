package matrix

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/worxbend/echo/internal/animations"

	"github.com/worxbend/echo/internal/observability"
)

const (
	defaultPlayQueueCapacity = 128
	defaultReconnectMinDelay = 500 * time.Millisecond
	defaultReconnectMaxDelay = 10 * time.Second
	defaultHeartbeatInterval = 5 * time.Second
	defaultProbeTimeout      = 2 * time.Second

	outcomeObserverQueueCapacity = 16

	// Desired-background retry bounds are intentionally fixed for v1.
	// Retryable failures back off from 1s to 30s; permanent failures retry
	// forever with capped backoff from 30s to 5m.
	backgroundRetryableMinDelay = 1 * time.Second
	backgroundRetryableMaxDelay = 30 * time.Second
	backgroundPermanentMinDelay = 30 * time.Second
	backgroundPermanentMaxDelay = 5 * time.Minute

	observabilityCallbackReconnectDelay        = ObservabilityCallbackReconnectDelay
	observabilityCallbackReconnectRecovered    = ObservabilityCallbackReconnectRecovered
	observabilityCallbackReconnectFailure      = ObservabilityCallbackReconnectFailure
	observabilityCallbackProbeFailure          = ObservabilityCallbackProbeFailure
	observabilityCallbackMatrixConnectedChange = ObservabilityCallbackMatrixConnectedChange
	observabilityCallbackBackgroundRestore     = ObservabilityCallbackBackgroundRestore
)

var (
	ErrSchedulerStopped       = errors.New("matrix scheduler stopped")
	ErrEmptyAnimation         = errors.New("animation rendered no frames")
	ErrPlayItemExpired        = errors.New("matrix play item expired")
	ErrPlayItemQueueCleared   = errors.New("matrix play item removed by queue clear")
	ErrControlQueueCleared    = errors.New("matrix control removed by queue clear")
	ErrControlDropped         = errors.New("matrix control dropped")
	ErrMissingAnimation       = animations.ErrAnimationNotFound
	ErrNonRenderableAnimation = errors.New("animation is not renderable/playable")
	ErrInvalidControl         = errors.New("invalid matrix control request")
	ErrItemInterrupted        = errors.New("item interrupted")
)

type AnimationRegistry interface {
	Get(id string) (animations.Animation, bool)
	FirmwarePreset(id string) (animations.FirmwarePreset, bool)
	StaticColor(id string) (animations.RGB, bool)
}

type clientReconnectRecoveryCounter interface {
	reconnectRecoveryCount() uint64
}

type controlSpec struct {
	validateRequest      func(ControlRequest) error
	run                  func(context.Context, *Scheduler, *ControlItem) error
	rememberState        func(*Scheduler, *ControlItem)
	marksBackgroundDirty bool
}

type restorePolicySpec func(context.Context, *Scheduler, displayState) error
type displayStateRestoreSpec func(context.Context, *Scheduler, displayState) error

var displayStateRestoreSpecs = map[displayStateKind]displayStateRestoreSpec{
	displayStateFrame: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.SetFrame(ctx, state.Frame)
		})
	},
	displayStateFill: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.Fill(ctx, state.Color)
		})
	},
	displayStateClear: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.Clear(ctx)
		})
	},
	displayStatePreset: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.SetPreset(ctx, state.EffectID, state.Interval, state.Color)
		})
	},
	displayStateStatic: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return s.client.SetStaticColor(ctx, state.Color)
		})
	},
	displayStateAnimation: func(ctx context.Context, s *Scheduler, state displayState) error {
		return s.retryMatrix(ctx, func() error {
			return uploadAnimation(ctx, s.client, state.Animation)
		})
	},
}

// uploadAnimation pushes a firmware-resident custom animation one frame at a time.
// The device starts looping once it has received every frame of the declared count,
// so the frames must be sent in order with a stable count.
func uploadAnimation(ctx context.Context, client Client, frames []AnimationFrame) error {
	if len(frames) == 0 {
		return fmt.Errorf("%w: custom animation requires at least one frame", ErrInvalidControl)
	}
	if len(frames) > MaxAnimationFrames {
		return fmt.Errorf("%w: custom animation supports at most %d frames: got %d", ErrInvalidControl, MaxAnimationFrames, len(frames))
	}
	count := byte(len(frames))
	for index, frame := range frames {
		if err := client.UploadCustomFrame(ctx, byte(index), count, frame.Delay, frame.Frame); err != nil {
			return err
		}
	}
	return nil
}

var controlSpecs = map[ControlKind]controlSpec{
	ControlClear: {
		run: func(ctx context.Context, scheduler *Scheduler, _ *ControlItem) error {
			return scheduler.client.Clear(ctx)
		},
		rememberState: func(scheduler *Scheduler, _ *ControlItem) {
			scheduler.rememberDisplayState(displayState{Kind: displayStateClear})
		},
		marksBackgroundDirty: true,
	},
	ControlSetBrightness: {
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetBrightness(ctx, control.Brightness)
		},
	},
	ControlSetPreset: {
		validateRequest: func(request ControlRequest) error {
			// The firmware implements effects 1..22 and treats 0 as "stop effect";
			// anything higher comes back as status 0x04. Rejecting it here turns a
			// confusing 502 from the panel into a 400 at the boundary.
			if request.EffectID > animations.MaxFirmwareEffectID {
				return fmt.Errorf("%w: effect_id must be between 0 and %d: %d", ErrInvalidControl, animations.MaxFirmwareEffectID, request.EffectID)
			}
			_, err := durationMilliseconds(request.Interval, "preset interval")
			return err
		},
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetPreset(ctx, control.EffectID, control.Interval, control.Color)
		},
		rememberState: func(scheduler *Scheduler, control *ControlItem) {
			// Effect 0 stops the running effect without touching the frame buffer, so
			// the panel keeps whatever the last tick drew. Remembering it would make
			// restore: previous_frame replay a command that reproduces no image —
			// exactly the failure ControlSetPixel deliberately avoids.
			if control.EffectID == animations.StopEffectID {
				return
			}
			scheduler.rememberDisplayState(displayState{
				Kind:     displayStatePreset,
				EffectID: control.EffectID,
				Interval: control.Interval,
				// Effects that compute their own colours discard this byte, so storing
				// it would let convergence compare something the panel never rendered.
				Color: presetColorForState(control.EffectID, control.Color),
			})
		},
		marksBackgroundDirty: true,
	},
	ControlFill: {
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.Fill(ctx, control.Color)
		},
		rememberState: func(scheduler *Scheduler, control *ControlItem) {
			scheduler.rememberDisplayState(displayState{
				Kind:  displayStateFill,
				Color: control.Color,
			})
		},
		marksBackgroundDirty: true,
	},
	ControlSetPixel: {
		validateRequest: func(request ControlRequest) error {
			return validatePixelCoordinate(request.X, request.Y)
		},
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetPixel(ctx, control.X, control.Y, control.Color)
		},
		// Deliberately no rememberState: a single pixel mutates whatever frame the
		// panel already held, so the resulting display cannot be reconstructed from
		// this command alone. Leaving the remembered state untouched is honest —
		// restore: previous_frame keeps the last state we can actually reproduce.
		marksBackgroundDirty: true,
	},
	ControlSetPanel: {
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return scheduler.client.SetPanelEnabled(ctx, control.Enabled)
		},
		// Panel enable is a visibility flag: the firmware keeps the stored frame and
		// restores it on re-enable, so the desired background is still satisfied and
		// must not be marked dirty.
	},
	ControlUploadAnimation: {
		validateRequest: func(request ControlRequest) error {
			return validateAnimationFrames(request.Animation)
		},
		run: func(ctx context.Context, scheduler *Scheduler, control *ControlItem) error {
			return uploadAnimation(ctx, scheduler.client, control.Animation)
		},
		rememberState: func(scheduler *Scheduler, control *ControlItem) {
			scheduler.rememberDisplayState(displayState{
				Kind:      displayStateAnimation,
				Animation: control.Animation,
			})
		},
		marksBackgroundDirty: true,
	},
}

// presetColorForState zeroes the colour for effects that ignore it, so remembered
// state and background-match comparisons only consider bytes the panel used.
func presetColorForState(effectID byte, color RGB) RGB {
	if animations.FirmwareEffectIgnoresColor(effectID) {
		return RGB{}
	}
	return color
}

func validatePixelCoordinate(x, y byte) error {
	if int(x) >= animations.CanvasWidth || int(y) >= animations.CanvasHeight {
		return fmt.Errorf("%w: pixel (%d,%d) outside %dx%d matrix", ErrInvalidControl, x, y, animations.CanvasWidth, animations.CanvasHeight)
	}
	return nil
}

func validateAnimationFrames(frames []AnimationFrame) error {
	if len(frames) == 0 {
		return fmt.Errorf("%w: custom animation requires at least one frame", ErrInvalidControl)
	}
	if len(frames) > MaxAnimationFrames {
		return fmt.Errorf("%w: custom animation supports at most %d frames: got %d", ErrInvalidControl, MaxAnimationFrames, len(frames))
	}
	for index, frame := range frames {
		if _, err := durationMilliseconds(frame.Delay, fmt.Sprintf("animation frame %d delay", index)); err != nil {
			return err
		}
	}
	return nil
}

type playItemLoop func(context.Context, *Scheduler, PlayItem) error

var playItemLoopStrategies = map[animations.LoopPolicy]playItemLoop{
	animations.LoopForever: func(ctx context.Context, s *Scheduler, item PlayItem) error {
		for {
			if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
				return err
			}
			if !item.Deadline.IsZero() && !s.now().Before(item.Deadline) {
				return nil
			}
		}
	},
	animations.LoopUntil: func(ctx context.Context, s *Scheduler, item PlayItem) error {
		for item.Deadline.IsZero() || s.now().Before(item.Deadline) {
			if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
				return err
			}
		}
		return nil
	},
	animations.LoopNone: func(ctx context.Context, s *Scheduler, item PlayItem) error {
		return s.playFrames(ctx, item.Frames, item.Deadline)
	},
}

type matrixErrorRecoveryPolicy func(*Scheduler, context.Context, time.Time, error) error

var matrixRetryPolicies = map[ErrorKind]matrixErrorRecoveryPolicy{
	ErrorKindRetryable: func(s *Scheduler, ctx context.Context, deadline time.Time, _ error) error {
		s.markMatrixFailure(StateDisconnected)
		return s.waitReady(ctx, deadline)
	},
	ErrorKindPermanent: func(_ *Scheduler, _ context.Context, _ time.Time, err error) error {
		return err
	},
}

var probeRetryableKinds = map[ErrorKind]struct{}{
	ErrorKindRetryable: {},
}

var restorePolicySpecs = map[animations.RestorePolicy]restorePolicySpec{
	animations.RestoreLeave: func(ctx context.Context, _ *Scheduler, _ displayState) error {
		return nil
	},
	animations.RestoreClear: func(ctx context.Context, s *Scheduler, _ displayState) error {
		err := s.retryMatrix(ctx, func() error {
			return s.client.Clear(ctx)
		})
		if err == nil {
			s.rememberDisplayState(displayState{Kind: displayStateClear})
		}
		return err
	},
	animations.RestorePreviousFrame: func(ctx context.Context, s *Scheduler, previous displayState) error {
		if !previous.known() {
			return nil
		}
		err := s.restoreDisplayState(ctx, previous)
		if err == nil && s.displayStateMatchesConfiguredBackground(previous) {
			s.markDesiredBackgroundConverged()
		}
		return err
	},
	animations.RestoreBackground: func(ctx context.Context, s *Scheduler, _ displayState) error {
		return s.applyDesiredBackground(ctx, true)
	},
}

func resolveControlSpec(kind ControlKind) (controlSpec, bool) {
	spec, ok := controlSpecs[kind]
	return spec, ok
}

type BackgroundConfig struct {
	AnimationID string
	Params      animations.Params
}

type ReconnectAttempt struct {
	Source         ReconnectSource
	Attempt        int
	BaseDelay      time.Duration
	Delay          time.Duration
	DeadlineCapped bool
	ErrorKind      ErrorKind
	Error          string
}

// ReconnectRecovery is the terminal success signal for one reconnect attempt.
// It means replacement connectivity has been firmware-verified; it does not
// guarantee that a retried non-ping command has also succeeded.
type ReconnectRecovery struct {
	Source  ReconnectSource
	Attempt int
	State   State
}

// ReconnectFailure is the terminal failure signal for one reconnect attempt
// that did not reach firmware-verified replacement connectivity. For
// tcp_immediate Ping retries, permanent status/protocol/validation errors are
// reported with outcome=verification_failed and the replacement socket is
// closed before the original Ping error is returned.
type ReconnectFailure struct {
	Source    ReconnectSource
	Attempt   int
	ErrorKind ErrorKind
	Outcome   ReconnectFailureOutcome
	Error     string
}

type ProbeFailure struct {
	ErrorKind ErrorKind
	Reason    ProbeFailureReason
	Error     string
}

type AnimationRenderResult struct {
	AnimationID string
	Duration    time.Duration
}

type BackgroundRestoreEvent struct {
	AnimationID  string
	Kind         BackgroundKind
	State        BackgroundConvergenceState
	ErrorKind    ErrorKind
	Error        string
	NextRetry    *time.Time
	FailureCount int
}

type SchedulerOptions struct {
	Client                  Client
	Registry                AnimationRegistry
	Packer                  animations.LayoutPacker
	QueueCapacity           int
	Background              BackgroundConfig
	ReconnectMinDelay       time.Duration
	ReconnectMaxDelay       time.Duration
	ReconnectJitter         func(time.Duration) time.Duration
	OnReconnectDelay        func(ReconnectAttempt)
	OnReconnectRecovered    func(ReconnectRecovery)
	OnReconnectFailure      func(ReconnectFailure)
	OnProbeFailure          func(ProbeFailure)
	OnMatrixConnectedChange func(bool)
	OnAnimationRendered     func(AnimationRenderResult)
	OnBackgroundRestore     func(BackgroundRestoreEvent)
	// OnItemOutcome observes terminal scheduler item outcomes. The scheduler
	// invokes it asynchronously and recovers observer panics so reporting cannot
	// affect scheduler correctness. Delivery is best-effort under sustained
	// observer backpressure; new notifications may be dropped instead of
	// blocking scheduler operations.
	OnItemOutcome      func(OutcomeReport)
	OnQueueDepthChange func(int)
	RetryDelay         time.Duration
	HeartbeatInterval  time.Duration
	ProbeTimeout       time.Duration
	Now                func() time.Time
}

type Scheduler struct {
	client                            Client
	registry                          AnimationRegistry
	packer                            animations.LayoutPacker
	queue                             *playQueue
	background                        BackgroundConfig
	reconnectMinDelay                 time.Duration
	reconnectMaxDelay                 time.Duration
	reconnectJitter                   func(time.Duration) time.Duration
	onReconnectDelay                  func(ReconnectAttempt)
	onReconnectRecovered              func(ReconnectRecovery)
	onReconnectFailure                func(ReconnectFailure)
	onProbeFailure                    func(ProbeFailure)
	onMatrixConnectedChange           func(bool)
	onAnimationRendered               func(AnimationRenderResult)
	onBackgroundRestore               func(BackgroundRestoreEvent)
	onItemOutcomeRecordedCriticalPath func(OutcomeReport)
	outcomeDispatcher                 *outcomeObserverDispatcher
	onQueueDepthChange                func(int)
	heartbeatInterval                 time.Duration
	probeTimeout                      time.Duration
	now                               func() time.Time
	onIdle                            func()

	mu                              sync.RWMutex
	state                           State
	connected                       bool
	lastSuccess                     time.Time
	lastFailure                     time.Time
	displayState                    displayState
	backgroundKind                  BackgroundKind
	backgroundConvergenceState      BackgroundConvergenceState
	desiredBackgroundDirty          bool
	backgroundLastRestoreAttempt    time.Time
	backgroundLastRestoreSuccess    time.Time
	backgroundLastRestoreError      string
	backgroundLastRestoreErrorClass ErrorKind
	backgroundRetryFailureCount     int
	backgroundRetryLastErrorClass   ErrorKind
	backgroundNextRestoreAttempt    time.Time
	clientReconnectRecoveries       uint64
	reconnectAttempt                int
	outcomeDrops                    atomic.Uint64
	outcomeRecordingPanics          atomic.Uint64

	callbackPanics observability.CallbackPanics

	// currentItemCancel and currentItemPriority track the in-flight animation
	// item's per-item cancel function and priority. Protected by mu.
	// currentItemCancel is nil when no animation item is in flight.
	currentItemCancel   context.CancelCauseFunc
	currentItemPriority int
}

func NewScheduler(options SchedulerOptions) (*Scheduler, error) {
	return newScheduler(options, nil)
}

// NewSchedulerWithReliableAppOutcomeRecorder constructs a Scheduler with the
// app's reliable play-item metrics recorder wired into the terminal outcome
// critical path.
//
// This is intentionally separate from SchedulerOptions so ordinary scheduler
// construction cannot casually attach arbitrary blocking work to terminal
// paths. The recorder must only update fast in-memory app metrics. It runs
// synchronously before best-effort OnItemOutcome observers, so blocking here
// blocks terminal scheduler paths. Panics are recovered and counted by
// OutcomeRecordingPanics, separately from best-effort observer drops.
func NewSchedulerWithReliableAppOutcomeRecorder(options SchedulerOptions, record func(OutcomeReport)) (*Scheduler, error) {
	return newScheduler(options, record)
}

func newScheduler(options SchedulerOptions, recordReliableOutcome func(OutcomeReport)) (*Scheduler, error) {
	if options.Client == nil {
		return nil, errors.New("matrix scheduler client is required")
	}
	if options.Registry == nil {
		return nil, errors.New("matrix scheduler animation registry is required")
	}
	if err := validateBackgroundConfig(options.Background, options.Registry); err != nil {
		return nil, err
	}
	queueCapacity := options.QueueCapacity
	if queueCapacity <= 0 {
		queueCapacity = defaultPlayQueueCapacity
	}
	queue := newPlayQueue(queueCapacity)
	reconnectMinDelay := options.ReconnectMinDelay
	if reconnectMinDelay <= 0 {
		reconnectMinDelay = options.RetryDelay
	}
	if reconnectMinDelay <= 0 {
		reconnectMinDelay = defaultReconnectMinDelay
	}
	reconnectMaxDelay := options.ReconnectMaxDelay
	if reconnectMaxDelay <= 0 {
		reconnectMaxDelay = defaultReconnectMaxDelay
	}
	if reconnectMaxDelay < reconnectMinDelay {
		reconnectMaxDelay = reconnectMinDelay
	}
	reconnectJitter := options.ReconnectJitter
	if reconnectJitter == nil {
		reconnectJitter = defaultReconnectJitter
	}
	heartbeatInterval := options.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	probeTimeout := options.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultProbeTimeout
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}

	backgroundKind := backgroundKindFor(options.Background, options.Registry)

	return &Scheduler{
		client:                            options.Client,
		registry:                          options.Registry,
		packer:                            options.Packer,
		queue:                             queue,
		background:                        options.Background,
		reconnectMinDelay:                 reconnectMinDelay,
		reconnectMaxDelay:                 reconnectMaxDelay,
		reconnectJitter:                   reconnectJitter,
		onReconnectDelay:                  options.OnReconnectDelay,
		onReconnectRecovered:              options.OnReconnectRecovered,
		onReconnectFailure:                options.OnReconnectFailure,
		onProbeFailure:                    options.OnProbeFailure,
		onMatrixConnectedChange:           options.OnMatrixConnectedChange,
		onAnimationRendered:               options.OnAnimationRendered,
		onBackgroundRestore:               options.OnBackgroundRestore,
		onItemOutcomeRecordedCriticalPath: recordReliableOutcome,
		outcomeDispatcher:                 newOutcomeObserverDispatcher(options.OnItemOutcome),
		onQueueDepthChange:                options.OnQueueDepthChange,
		heartbeatInterval:                 heartbeatInterval,
		probeTimeout:                      probeTimeout,
		now:                               now,
		state:                             StateDisconnected,
		backgroundKind:                    backgroundKind,
		backgroundConvergenceState:        BackgroundConvergenceUnknown,
		backgroundLastRestoreErrorClass:   ErrorKindNone,
		clientReconnectRecoveries:         clientReconnectRecoveryCount(options.Client),
	}, nil
}

func validateBackgroundConfig(background BackgroundConfig, registry AnimationRegistry) error {
	if background.AnimationID == "" {
		return nil
	}
	if preset, ok := registry.FirmwarePreset(background.AnimationID); ok {
		if err := animations.ValidateFirmwarePreset(preset); err != nil {
			return fmt.Errorf("background animation %q: %w", background.AnimationID, err)
		}
		return nil
	}
	if _, ok := registry.StaticColor(background.AnimationID); ok {
		return nil
	}
	if _, ok := registry.Get(background.AnimationID); !ok {
		return fmt.Errorf("%w: %s", ErrMissingAnimation, background.AnimationID)
	}
	return nil
}

func backgroundKindFor(background BackgroundConfig, registry AnimationRegistry) BackgroundKind {
	if background.AnimationID == "" {
		return ""
	}
	if _, ok := registry.FirmwarePreset(background.AnimationID); ok {
		return BackgroundKindFirmwarePreset
	}
	if _, ok := registry.StaticColor(background.AnimationID); ok {
		return BackgroundKindStaticColor
	}
	return BackgroundKindRenderable
}

// SetBackground atomically replaces the scheduler's desired idle background.
// Pass an empty BackgroundConfig to disable idle convergence. The new animation
// ID is validated against the registry; an error is returned and the background
// is left unchanged if validation fails.
func (s *Scheduler) SetBackground(background BackgroundConfig) error {
	if err := validateBackgroundConfig(background, s.registry); err != nil {
		return err
	}
	newKind := backgroundKindFor(background, s.registry)
	s.mu.Lock()
	s.background = background
	s.backgroundKind = newKind
	if background.AnimationID != "" {
		s.markDesiredBackgroundDirtyLocked(true)
	} else {
		// Disabling the background: mark converged so the scheduler stops trying.
		s.markDesiredBackgroundConvergedLocked()
		s.backgroundConvergenceState = BackgroundConvergenceConverged
	}
	s.mu.Unlock()
	return nil
}

// Background returns the current desired idle background configuration.
func (s *Scheduler) Background() BackgroundConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.background
}

func (s *Scheduler) QueueLen() int {
	return s.queue.len()
}

func (s *Scheduler) QueueSnapshot() []QueueItemStatus {
	return s.queue.snapshot()
}

// Close closes the scheduler-owned best-effort outcome observer dispatcher.
//
// During normal operation Run owns the dispatcher lifetime and calls Close
// before returning. Call Close for schedulers that are constructed with an
// OnItemOutcome observer but never run. Close is idempotent and prevents new
// observer reports from being accepted, but it cannot preempt observer code
// already blocked inside the user-provided callback.
func (s *Scheduler) Close() {
	if s.outcomeDispatcher == nil {
		return
	}
	s.outcomeDispatcher.close()
}

func (s *Scheduler) ClearQueue() int {
	items := s.queue.clear()
	queueDepthBeforeClear := len(items)
	if queueDepthBeforeClear > 0 {
		s.reportQueueDepth(0)
	}
	for _, item := range items {
		s.completeQueueClearedItemWithOutcome(item, queueDepthBeforeClear)
	}
	return len(items)
}

func (s *Scheduler) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Scheduler) Health() Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	background := ProjectBackgroundConvergence(BackgroundConvergenceProjectionInput{
		State:                 s.backgroundConvergenceState,
		Dirty:                 s.desiredBackgroundDirty,
		LastRestoreError:      s.backgroundLastRestoreError,
		LastRestoreErrorClass: s.backgroundLastRestoreErrorClass,
		NextRetry:             nextRetry,
		FailureCount:          s.backgroundRetryFailureCount,
	}, s.now())
	health := Health{
		State:                           s.state,
		MatrixConnected:                 s.connected,
		BackgroundID:                    s.background.AnimationID,
		BackgroundKind:                  s.backgroundKind,
		BackgroundConvergenceState:      background.State,
		BackgroundDirty:                 background.Dirty,
		BackgroundConverged:             background.Converged,
		BackgroundLastRestoreError:      s.backgroundLastRestoreError,
		BackgroundLastRestoreErrorClass: s.backgroundLastRestoreErrorClass,
		BackgroundNextRetry:             nextRetry,
		BackgroundRetryFailureCount:     s.backgroundRetryFailureCount,
		OutcomeReportsDropped:           s.OutcomeReportsDropped(),
		OutcomeRecordingPanics:          s.OutcomeRecordingPanics(),
	}
	health.ObservabilityCallbackPanics = s.ObservabilityCallbackPanics()
	health.ObservabilityCallbackCounts = s.ObservabilityCallbackPanicCounts()
	if !s.lastSuccess.IsZero() {
		lastSuccess := s.lastSuccess
		health.LastSuccess = &lastSuccess
	}
	if !s.lastFailure.IsZero() {
		lastFailure := s.lastFailure
		health.LastFailure = &lastFailure
	}
	if !s.backgroundLastRestoreAttempt.IsZero() {
		lastAttempt := s.backgroundLastRestoreAttempt
		health.BackgroundLastRestoreAttempt = &lastAttempt
	}
	if !s.backgroundLastRestoreSuccess.IsZero() {
		lastSuccess := s.backgroundLastRestoreSuccess
		health.BackgroundLastRestoreSuccess = &lastSuccess
	}
	return health
}

func (s *Scheduler) EnqueueRequest(ctx context.Context, request animations.AnimationRequest) error {
	item, err := s.ResolveRequest(ctx, request)
	if err != nil {
		return err
	}
	if s.expired(item.PlayItem) {
		s.completeAnimationWithOutcome(item, ErrPlayItemExpired, 0)
		return ErrPlayItemExpired
	}
	_, queueDepth, err := s.queue.enqueueScheduled(ctx, item)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.completeAnimationWithOutcome(item, err, queueDepth)
			return err
		}
		s.completeAnimationWithOutcome(item, err, queueDepth)
		return err
	}
	item.QueueDepthAtAdmission = queueDepth
	s.reportQueueDepth(queueDepth)
	s.applyInterruptMode(item)
	return nil
}

// applyInterruptMode evicts lower-priority queued items and optionally cancels
// the in-flight item based on the newly enqueued item's InterruptMode.
func (s *Scheduler) applyInterruptMode(item ScheduledItem) {
	mode := item.InterruptMode
	if mode != animations.InterruptHigherPriority && mode != animations.InterruptCritical {
		return
	}
	evicted, newDepth := s.queue.evictLowerPriority(item.Priority)
	if len(evicted) > 0 {
		s.reportQueueDepth(newDepth)
		for _, evictedItem := range evicted {
			s.completeAnimationWithOutcome(evictedItem, ErrItemInterrupted, newDepth)
		}
	}
	if mode == animations.InterruptCritical {
		s.mu.RLock()
		cancel := s.currentItemCancel
		inflight := s.currentItemPriority
		s.mu.RUnlock()
		if cancel != nil && inflight < item.Priority {
			cancel(ErrItemInterrupted)
		}
	}
}

func (s *Scheduler) Clear(ctx context.Context) error {
	return s.EnqueueControl(ctx, ControlRequest{Kind: ControlClear})
}

func (s *Scheduler) SetBrightness(ctx context.Context, value byte) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:       ControlSetBrightness,
		Brightness: value,
	})
}

func (s *Scheduler) SetPreset(ctx context.Context, effectID byte, interval time.Duration, color RGB) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:     ControlSetPreset,
		EffectID: effectID,
		Interval: interval,
		Color:    color,
	})
}

// SetPixel sets one pixel, taking display-space coordinates.
//
// The firmware applies its own serpentine mapping to whatever x/y it receives, but
// that mapping alone is NOT the one frame uploads go through: LayoutPacker also
// compensates for this panel's mirrored odd rows (Layout.OddRowDisplayFlip) before
// computing the chain index. Forwarding display coordinates raw therefore put every
// pixel on rows 1/3/5/7 at the mirrored LED — 32 of 64 coordinates disagreed with
// every frame-based path.
//
// Converting to server space here is exactly the missing half: the firmware's own
// serpentine step then lands on the same physical LED the packer would have chosen.
// This is the same display -> server translation the Python reference client does in
// tools/matrix_client.py::display_to_server_point.
func (s *Scheduler) SetPixel(ctx context.Context, x, y byte, color RGB) error {
	layout := s.packer.Layout()
	serverX, serverY, err := layout.DisplayToServerPoint(int(x), int(y))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidControl, err)
	}
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:  ControlSetPixel,
		X:     byte(serverX),
		Y:     byte(serverY),
		Color: color,
	})
}

// SetPanelEnabled blanks or restores visible output without discarding the frame
// the firmware is holding.
func (s *Scheduler) SetPanelEnabled(ctx context.Context, enabled bool) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:    ControlSetPanel,
		Enabled: enabled,
	})
}

// UploadAnimation stores an animation in the firmware's custom slot and lets the
// device loop it locally, with no per-frame TCP round-trip. Frames arrive in
// display space and are packed to the device's physical LED order here, so callers
// never need to know the panel's wiring.
func (s *Scheduler) UploadAnimation(ctx context.Context, frames []Frame) error {
	if len(frames) == 0 {
		return fmt.Errorf("%w: custom animation requires at least one frame", ErrInvalidControl)
	}
	if len(frames) > MaxAnimationFrames {
		return fmt.Errorf("%w: custom animation supports at most %d frames: got %d", ErrInvalidControl, MaxAnimationFrames, len(frames))
	}
	packed := make([]AnimationFrame, 0, len(frames))
	for _, frame := range frames {
		packed = append(packed, AnimationFrame{
			Frame: s.packer.Pack(frame),
			Delay: frame.Delay,
		})
	}
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:      ControlUploadAnimation,
		Animation: packed,
	})
}

func (s *Scheduler) Fill(ctx context.Context, color RGB) error {
	return s.EnqueueControl(ctx, ControlRequest{
		Kind:  ControlFill,
		Color: color,
	})
}

func (s *Scheduler) EnqueueControl(ctx context.Context, request ControlRequest) error {
	item, err := s.ResolveControl(ctx, request)
	if err != nil {
		return err
	}
	if s.expired(item.PlayItem) {
		s.completeControlWithOutcome(item, ErrPlayItemExpired, 0)
		return ErrPlayItemExpired
	}
	handle, queueDepth, err := s.queue.enqueueScheduled(ctx, item)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.completeControlWithOutcome(item, err, queueDepth)
			return err
		}
		dropErr := errors.Join(ErrControlDropped, err)
		s.completeControlWithOutcome(item, dropErr, queueDepth)
		return dropErr
	}
	item.QueueDepthAtAdmission = queueDepth
	s.reportQueueDepth(queueDepth)

	stopContext := context.AfterFunc(ctx, func() {
		s.completePendingControl(handle, ctx.Err())
	})
	defer stopContext()

	stopDeadline := s.afterControlDeadline(handle, item.Control)
	defer stopDeadline()

	<-item.Control.done
	return item.Control.result()
}

func (s *Scheduler) ResolveControl(ctx context.Context, request ControlRequest) (ScheduledItem, error) {
	if err := ctx.Err(); err != nil {
		return ScheduledItem{}, err
	}
	if request.Kind == "" {
		return ScheduledItem{}, fmt.Errorf("%w: control kind is required", ErrInvalidControl)
	}
	spec, ok := resolveControlSpec(request.Kind)
	if !ok {
		return ScheduledItem{}, fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, request.Kind)
	}
	if spec.validateRequest != nil {
		if err := spec.validateRequest(request); err != nil {
			return ScheduledItem{}, err
		}
	}

	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.now().UTC()
	}
	id := request.ID
	if id == "" {
		id = fmt.Sprintf("%s:%d", request.Kind, createdAt.UnixNano())
	}

	control := &ControlItem{
		ID:         id,
		Kind:       request.Kind,
		Priority:   request.Priority,
		Brightness: request.Brightness,
		EffectID:   request.EffectID,
		Interval:   request.Interval,
		Color:      request.Color,
		X:          request.X,
		Y:          request.Y,
		Enabled:    request.Enabled,
		Animation:  request.Animation,
		CreatedAt:  createdAt,
		Deadline:   request.Deadline,
		ctx:        ctx,
		done:       make(chan struct{}),
	}

	return ScheduledItem{
		PlayItem: PlayItem{
			ID:       id,
			Priority: request.Priority,
			Deadline: request.Deadline,
		},
		CreatedAt: createdAt,
		Control:   control,
	}, nil
}

func (s *Scheduler) ResolveRequest(ctx context.Context, request animations.AnimationRequest) (ScheduledItem, error) {
	if request.AnimationID == "" {
		return ScheduledItem{}, errors.New("animation request animation_id is required")
	}

	animation, ok := s.registry.Get(request.AnimationID)
	if !ok {
		if _, presetOK := s.registry.FirmwarePreset(request.AnimationID); presetOK {
			return ScheduledItem{}, fmt.Errorf("%w: %s", ErrNonRenderableAnimation, request.AnimationID)
		}
		return ScheduledItem{}, fmt.Errorf("%w: %s", ErrMissingAnimation, request.AnimationID)
	}

	renderStart := time.Now()
	frames, err := animation.Render(ctx, request.Params)
	s.reportAnimationRendered(request.AnimationID, time.Since(renderStart))
	if err != nil {
		return ScheduledItem{}, fmt.Errorf("render animation %q: %w", request.AnimationID, err)
	}
	// Trimming applies to a single pass. A looping item keeps its whole cycle and is
	// bounded by a deadline instead, set below.
	if request.Loop == "" || request.Loop == animations.LoopNone {
		frames = applyMaxDuration(frames, request.MaxDuration)
	}
	if len(frames) == 0 {
		return ScheduledItem{}, fmt.Errorf("%w: %s", ErrEmptyAnimation, request.AnimationID)
	}

	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.now().UTC()
	}
	restore := request.RestorePolicy
	if restore == "" {
		restore = animations.RestoreLeave
	}
	id := request.ID
	if id == "" {
		id = fmt.Sprintf("%s:%d", request.AnimationID, createdAt.UnixNano())
	}
	interruptMode := request.InterruptMode
	if interruptMode == "" {
		interruptMode = animations.InterruptNone
	}
	loop := request.Loop
	if loop == "" {
		loop = animations.LoopNone
	}
	if !animations.IsValidLoopPolicy(loop) {
		return ScheduledItem{}, fmt.Errorf("%w: loop %q is not a valid loop policy; expected one of %s",
			ErrInvalidControl, loop, strings.Join(animations.LoopPolicyNames(), ", "))
	}
	// Both looping strategies test PlayItem.Deadline, and applyMaxDuration only
	// trims frames — it never sets one. Without a deadline LoopForever's exit test
	// can never fire and LoopUntil's guard is permanently true, so either would loop
	// forever and pin the queue. Require a duration and convert it to a deadline.
	var deadline time.Time
	if loop != animations.LoopNone {
		if request.MaxDuration <= 0 {
			return ScheduledItem{}, fmt.Errorf("%w: loop %q requires a positive duration to terminate", ErrInvalidControl, loop)
		}
		deadline = createdAt.Add(request.MaxDuration)
	}

	return ScheduledItem{
		PlayItem: PlayItem{
			ID:       id,
			EventID:  request.EventID,
			Priority: request.Priority,
			Frames:   frames,
			Loop:     loop,
			Deadline: deadline,
		},
		AnimationID:         request.AnimationID,
		RestorePolicy:       restore,
		InterruptMode:       interruptMode,
		CreatedAt:           createdAt,
		animationCompletion: &animationCompletion{},
	}, nil
}

// stoppedByContext reports whether err means the run context ended rather than
// the operation itself failing.
func stoppedByContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// itemDisposition says what Run should do once an item has finished.
type itemDisposition uint8

const (
	// itemNext: take the next item.
	itemNext itemDisposition = iota
	// itemDrain: the run context is done; stop cleanly.
	itemDrain
	// itemFail: stop and report the error.
	itemFail
)

func (s *Scheduler) Run(ctx context.Context) error {
	defer s.Close()
	defer s.completeQueuedControls(ErrSchedulerStopped)

	if err := s.waitReady(ctx, time.Time{}); err != nil {
		if stoppedByContext(err) {
			s.setState(StateDraining)
			return nil
		}
		return err
	}

	deferBackgroundRestore := false
	for {
		if deferBackgroundRestore {
			deferBackgroundRestore = false
		} else if s.shouldApplyDesiredBackground() && s.queue.len() == 0 {
			if err := s.applyDesiredBackground(ctx, false); err != nil {
				if stoppedByContext(err) {
					s.setState(StateDraining)
					return nil
				}
				s.setState(StateReady)
				deferBackgroundRestore = true
				continue
			}
			s.setState(StateReady)
			continue
		}

		if s.queue.len() == 0 {
			s.notifyIdle()
		}

		item, ok, err := s.nextItemOrHeartbeat(ctx)
		if err != nil {
			if stoppedByContext(err) {
				s.setState(StateDraining)
				return nil
			}
			return err
		}
		if !ok {
			if err := s.heartbeat(ctx); err != nil {
				if stoppedByContext(err) {
					s.setState(StateDraining)
					return nil
				}
				return err
			}
			continue
		}

		if s.expired(item.PlayItem) {
			if item.Control != nil {
				s.completeControlWithOutcome(item, ErrPlayItemExpired, 0)
			} else {
				s.completeAnimationWithOutcome(item, ErrPlayItemExpired, 0)
			}
			continue
		}

		if item.Control != nil {
			if !s.runControl(ctx, item) {
				s.setState(StateDraining)
				return nil
			}
			continue
		}

		switch disposition, err := s.playScheduledItem(ctx, item); disposition {
		case itemNext:
			continue
		case itemDrain:
			return nil
		default:
			return err
		}
	}
}

// runControl executes one queued control item and records its outcome. It
// reports whether the run should continue; false means the context ended while
// the control was in flight.
func (s *Scheduler) runControl(ctx context.Context, item ScheduledItem) bool {
	if item.Control.isCompleted() {
		return true
	}
	if err := item.Control.ctxErr(); err != nil {
		s.completeControlWithOutcome(item, err, 0)
		return true
	}

	err := s.terminalError(ctx, s.executeControl(ctx, item.Control))
	s.completeControlWithOutcome(item, err, 0)
	if ctx.Err() != nil {
		return false
	}
	s.setState(StateReady)
	return true
}

// playScheduledItem plays one animation item to completion and says what Run
// should do next.
//
// Releasing the per-item cancel func and recording the item's outcome happens
// once, in a defer, instead of at each of the five exits this loop used to
// have -- where every new exit was a chance to leak the cancel func or drop the
// outcome record.
//
// The value the item is recorded as finishing with is not always the value Run
// is told to fail with: a failed restore records the classified terminal error
// but surfaces the raw restore error to the caller.
func (s *Scheduler) playScheduledItem(ctx context.Context, item ScheduledItem) (itemDisposition, error) {
	preItemState := s.snapshotDisplayState()
	// Ordinary playback is transient. Once a playback item is selected, the
	// configured background is again the desired eventual idle state; item
	// restore policies may affect only the immediate post-playback display
	// unless they explicitly force a background restore.
	s.markDesiredBackgroundDirty()

	itemCtx, itemCancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	s.currentItemCancel = itemCancel
	s.currentItemPriority = item.Priority
	s.mu.Unlock()

	var outcomeErr error
	defer func() {
		itemCancel(nil)
		s.clearCurrentItemCancel()
		s.completeAnimationWithOutcome(item, outcomeErr, 0)
	}()

	for {
		if s.expired(item.PlayItem) {
			outcomeErr = ErrPlayItemExpired
			return itemNext, nil
		}

		s.setState(StatePlayingTransient)
		err := s.playItem(itemCtx, item.PlayItem)

		switch {
		case err == nil:
			restoreErr := s.restore(ctx, item.RestorePolicy, preItemState)
			if restoreErr == nil {
				s.setState(StateReady)
				outcomeErr = nil
				return itemNext, nil
			}
			outcomeErr = s.terminalError(ctx, restoreErr)
			if errors.Is(outcomeErr, ErrSchedulerStopped) {
				s.setState(StateDraining)
				return itemDrain, nil
			}
			s.setState(StateReady)
			if stoppedByContext(outcomeErr) || errors.Is(outcomeErr, ErrPlayItemExpired) {
				return itemNext, nil
			}
			return itemFail, restoreErr

		case errors.Is(err, ErrEmptyAnimation) || errors.Is(err, ErrPlayItemExpired):
			s.setState(StateReady)
			outcomeErr = err
			return itemNext, nil

		case stoppedByContext(err):
			if context.Cause(itemCtx) == ErrItemInterrupted {
				outcomeErr = ErrItemInterrupted
			} else {
				outcomeErr = s.terminalError(ctx, err)
			}
			if errors.Is(outcomeErr, ErrSchedulerStopped) {
				s.setState(StateDraining)
				return itemDrain, nil
			}
			s.setState(StateReady)
			return itemNext, nil

		case IsPermanentError(ctx, err):
			outcomeErr = err
			return itemFail, err
		}

		s.markMatrixFailure(StateDisconnected)
		if waitErr := s.waitReady(ctx, item.PlayItem.Deadline); waitErr != nil {
			if stoppedByContext(waitErr) || errors.Is(waitErr, ErrPlayItemExpired) {
				outcomeErr = s.terminalError(ctx, waitErr)
				if errors.Is(outcomeErr, ErrSchedulerStopped) {
					s.setState(StateDraining)
				}
				return itemNext, nil
			}
			outcomeErr = waitErr
			return itemFail, waitErr
		}
	}
}

// terminalError reports a context-shaped failure as ErrSchedulerStopped when
// the run context is what ended, and passes anything else through unchanged.
func (s *Scheduler) terminalError(runCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if stoppedByContext(err) && runCtx.Err() != nil {
		return ErrSchedulerStopped
	}
	return err
}

func (s *Scheduler) nextItemOrHeartbeat(ctx context.Context) (ScheduledItem, bool, error) {
	if s.heartbeatInterval <= 0 {
		item, err := s.queue.next(ctx)
		if err == nil {
			s.reportQueueDepth(s.queue.len())
		}
		return item, true, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, s.heartbeatInterval)
	defer cancel()

	item, err := s.queue.next(waitCtx)
	if err == nil {
		s.reportQueueDepth(s.queue.len())
		return item, true, nil
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return ScheduledItem{}, false, nil
	}
	return ScheduledItem{}, false, err
}

func (s *Scheduler) heartbeat(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err, probeTimedOut := s.pingProbe(ctx, time.Time{})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		s.reportProbeFailure(ctx, err, probeTimedOut)
		if s.classifyProbeError(ctx, err) == ErrorKindPermanent {
			return cancellationOr(ctx, err)
		}
		s.markMatrixFailure(StateDisconnected)
		return nil
	}
	s.markMatrixSuccess(StateReady)
	return nil
}

func (s *Scheduler) playItem(ctx context.Context, item PlayItem) error {
	if len(item.Frames) == 0 {
		return ErrEmptyAnimation
	}
	if item.OnStart != nil {
		if err := item.OnStart(ctx); err != nil {
			return err
		}
	}
	runner, ok := playItemLoopStrategies[item.Loop]
	if !ok {
		runner = playItemLoopStrategies[animations.LoopNone]
	}
	if err := runner(ctx, s, item); err != nil {
		return err
	}

	return s.finish(ctx, item)
}

func (s *Scheduler) retryMatrixError(ctx context.Context, readyDeadline time.Time, err error, classify func(context.Context, error) ErrorKind) error {
	recovery, ok := matrixRetryPolicies[classify(ctx, err)]
	if !ok {
		return err
	}
	return recovery(s, ctx, readyDeadline, err)
}

func (s *Scheduler) executeControl(ctx context.Context, control *ControlItem) error {
	if control == nil {
		return nil
	}
	spec, ok := resolveControlSpec(control.Kind)
	if !ok {
		return fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, control.Kind)
	}
	if spec.run == nil {
		return fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, control.Kind)
	}
	controlCtx := control.ctx
	if controlCtx == nil {
		controlCtx = context.Background()
	}
	if err := controlCtx.Err(); err != nil {
		return err
	}

	var execCtx context.Context
	var cancel context.CancelFunc
	if !control.Deadline.IsZero() {
		execCtx, cancel = context.WithDeadline(ctx, control.Deadline)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(controlCtx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	err := s.retryControlMatrix(execCtx, control, func() error {
		return spec.run(execCtx, s, control)
	})
	if err != nil && ctx.Err() == nil {
		if controlErr := controlCtx.Err(); controlErr != nil {
			return controlErr
		}
		if errors.Is(err, context.DeadlineExceeded) && !control.Deadline.IsZero() && !s.now().Before(control.Deadline) {
			return ErrPlayItemExpired
		}
	}
	if err == nil {
		s.rememberControlDisplayState(control)
		s.markDesiredBackgroundDirtyAfterControl(control)
	}
	return err
}

func (s *Scheduler) completeQueuedControls(err error) {
	items := s.queue.clear()
	if len(items) > 0 {
		s.reportQueueDepth(0)
	}
	for _, item := range items {
		if item.Control != nil {
			s.completeControlWithOutcome(item, err, len(items))
			continue
		}
		s.completeAnimationWithOutcome(item, err, len(items))
	}
}

func (s *Scheduler) completeQueueClearedItemWithOutcome(item ScheduledItem, queueDepthBeforeClear int) {
	if item.Control != nil {
		if !item.Control.complete(ErrControlQueueCleared) {
			return
		}
		s.reportOutcome(queueClearedOutcomeReport(item, queueDepthBeforeClear, s.now().UTC()))
		return
	}
	if !s.completeClearedAnimation(item) {
		return
	}
	s.reportOutcome(queueClearedOutcomeReport(item, queueDepthBeforeClear, s.now().UTC()))
}

func (s *Scheduler) completeClearedAnimation(item ScheduledItem) bool {
	if item.Control != nil {
		return false
	}
	item.ensureAnimationCompletion()
	if !item.animationCompletion.complete() {
		return false
	}
	return errors.Is(s.completeClearedPlayItem(item.PlayItem), ErrPlayItemQueueCleared)
}

func (s *Scheduler) completeClearedPlayItem(item PlayItem) error {
	// A queue-cleared play item never started, and Hook has no outcome
	// parameter. Queue-clear observability is emitted through OutcomeReport;
	// do not call OnStart or OnFinish for this terminal state.
	return ErrPlayItemQueueCleared
}

func (s *Scheduler) completeControlWithOutcome(item ScheduledItem, err error, queueDepthAtRemoval int) {
	if item.Control == nil {
		return
	}
	if !item.Control.complete(err) {
		return
	}
	s.reportOutcome(controlOutcomeReport(item, err, queueDepthAtRemoval, s.now().UTC()))
}

func (s *Scheduler) completeAnimationWithOutcome(item ScheduledItem, err error, queueDepthAtRemoval int) {
	if item.Control != nil {
		return
	}
	item.ensureAnimationCompletion()
	if !item.animationCompletion.complete() {
		return
	}
	s.reportOutcome(animationOutcomeReport(item, err, queueDepthAtRemoval, s.now().UTC()))
}

type animationCompletion struct {
	once sync.Once
}

func (completion *animationCompletion) complete() bool {
	if completion == nil {
		return true
	}
	completed := false
	completion.once.Do(func() {
		completed = true
	})
	return completed
}

func (item *ScheduledItem) ensureAnimationCompletion() {
	if item.Control != nil || item.animationCompletion != nil {
		return
	}
	item.animationCompletion = &animationCompletion{}
}

func (s *Scheduler) reportOutcome(report OutcomeReport) {
	if s.onItemOutcomeRecordedCriticalPath != nil {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					s.outcomeRecordingPanics.Add(1)
				}
			}()
			s.onItemOutcomeRecordedCriticalPath(report)
		}()
	}
	if s.outcomeDispatcher == nil {
		return
	}
	if !s.outcomeDispatcher.dispatch(report) {
		s.outcomeDrops.Add(1)
	}
}

func (s *Scheduler) OutcomeReportsDropped() uint64 {
	return s.outcomeDrops.Load()
}

func (s *Scheduler) OutcomeRecordingPanics() uint64 {
	return s.outcomeRecordingPanics.Load()
}

func (s *Scheduler) notifyIdle() {
	if s.onIdle != nil {
		s.onIdle()
	}
}

func (s *Scheduler) clearCurrentItemCancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentItemCancel = nil
	s.currentItemPriority = 0
}

type outcomeObserverDispatcher struct {
	observer func(OutcomeReport)
	reports  chan OutcomeReport
	done     chan struct{}

	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func newOutcomeObserverDispatcher(observer func(OutcomeReport)) *outcomeObserverDispatcher {
	if observer == nil {
		return nil
	}
	dispatcher := &outcomeObserverDispatcher{
		observer: observer,
		reports:  make(chan OutcomeReport, outcomeObserverQueueCapacity),
		done:     make(chan struct{}),
	}
	go dispatcher.run()
	return dispatcher
}

func (d *outcomeObserverDispatcher) dispatch(report OutcomeReport) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	select {
	case d.reports <- report:
		return true
	default:
		return false
	}
}

func (d *outcomeObserverDispatcher) close() {
	d.once.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.reports)
		d.mu.Unlock()
	})
}

func (d *outcomeObserverDispatcher) wait() {
	<-d.done
}

func (d *outcomeObserverDispatcher) doneCh() <-chan struct{} {
	return d.done
}

func (d *outcomeObserverDispatcher) run() {
	defer close(d.done)
	for report := range d.reports {
		func() {
			defer func() {
				_ = recover()
			}()
			d.observer(report)
		}()
	}
}

func (s *Scheduler) reportQueueDepth(depth int) {
	observer := s.onQueueDepthChange
	if observer == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	observer(depth)
}

func (s *Scheduler) reportAnimationRendered(animationID string, duration time.Duration) {
	observer := s.onAnimationRendered
	if observer == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	observer(AnimationRenderResult{
		AnimationID: animationID,
		Duration:    duration,
	})
}

func controlOutcomeReport(item ScheduledItem, err error, queueDepthAtRemoval int, timestamp time.Time) OutcomeReport {
	outcome, reason, errorClass := controlOutcomeFromError(err)
	control := item.Control
	report := OutcomeReport{
		Outcome:               outcome,
		ItemKind:              QueueItemControl,
		ItemID:                item.ID,
		Priority:              item.Priority,
		QueueDepthAtAdmission: item.QueueDepthAtAdmission,
		QueueDepthAtRemoval:   queueDepthAtRemoval,
		Reason:                reason,
		ErrorClass:            errorClass,
		Timestamp:             timestamp,
	}
	if control != nil {
		report.ItemID = control.ID
		report.ControlKind = control.Kind
		report.Priority = control.Priority
	}
	return report
}

func controlOutcomeFromError(err error) (ItemOutcome, string, ErrorKind) {
	if err == nil {
		return ItemOutcomeExecuted, string(ItemOutcomeExecuted), ErrorKindNone
	}
	if errors.Is(err, ErrPlayItemExpired) {
		return ItemOutcomeExpired, string(ItemOutcomeExpired), ErrorKindPermanent
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ItemOutcomeCanceled, string(ItemOutcomeCanceled), ErrorKindPermanent
	}
	if errors.Is(err, ErrControlDropped) || errors.Is(err, ErrPlayQueueFull) {
		return ItemOutcomeDropped, string(ItemOutcomeDropped), ClassifyError(context.Background(), err)
	}
	if errors.Is(err, ErrSchedulerStopped) {
		return ItemOutcomeSchedulerStopped, string(ItemOutcomeSchedulerStopped), ErrorKindPermanent
	}
	if ClassifyError(context.Background(), err) == ErrorKindPermanent {
		return ItemOutcomePermanentError, string(ItemOutcomePermanentError), ErrorKindPermanent
	}
	return ItemOutcomePermanentError, string(ItemOutcomePermanentError), ClassifyError(context.Background(), err)
}

func animationOutcomeReport(item ScheduledItem, err error, queueDepthAtRemoval int, timestamp time.Time) OutcomeReport {
	outcome, reason, errorClass := animationOutcomeFromError(err)
	return OutcomeReport{
		Outcome:               outcome,
		ItemKind:              QueueItemAnimation,
		ItemID:                item.ID,
		EventID:               item.EventID,
		AnimationID:           item.AnimationID,
		Priority:              item.Priority,
		QueueDepthAtAdmission: item.QueueDepthAtAdmission,
		QueueDepthAtRemoval:   queueDepthAtRemoval,
		Reason:                reason,
		ErrorClass:            errorClass,
		Timestamp:             timestamp,
	}
}

func animationOutcomeFromError(err error) (ItemOutcome, string, ErrorKind) {
	if err == nil {
		return ItemOutcomeExecuted, string(ItemOutcomeExecuted), ErrorKindNone
	}
	if errors.Is(err, ErrItemInterrupted) {
		return ItemOutcomeInterrupted, string(ItemOutcomeInterrupted), ErrorKindPermanent
	}
	if errors.Is(err, ErrPlayItemExpired) {
		return ItemOutcomeExpired, string(ItemOutcomeExpired), ErrorKindPermanent
	}
	if errors.Is(err, ErrPlayQueueFull) {
		return ItemOutcomeDropped, string(ItemOutcomeDropped), ClassifyError(context.Background(), err)
	}
	if errors.Is(err, ErrSchedulerStopped) {
		return ItemOutcomeSchedulerStopped, string(ItemOutcomeSchedulerStopped), ErrorKindPermanent
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ItemOutcomeCanceled, string(ItemOutcomeCanceled), ErrorKindPermanent
	}
	if ClassifyError(context.Background(), err) == ErrorKindPermanent {
		return ItemOutcomePermanentError, string(ItemOutcomePermanentError), ErrorKindPermanent
	}
	return ItemOutcomePermanentError, string(ItemOutcomePermanentError), ClassifyError(context.Background(), err)
}

func queueClearedOutcomeReport(item ScheduledItem, queueDepthBeforeClear int, timestamp time.Time) OutcomeReport {
	report := OutcomeReport{
		Outcome:               ItemOutcomeQueueCleared,
		ItemKind:              QueueItemAnimation,
		ItemID:                item.ID,
		EventID:               item.EventID,
		AnimationID:           item.AnimationID,
		Priority:              item.Priority,
		QueueDepthBeforeClear: queueDepthBeforeClear,
		QueueDepthAtAdmission: item.QueueDepthAtAdmission,
		QueueDepthAtRemoval:   queueDepthBeforeClear,
		Reason:                string(ItemOutcomeQueueCleared),
		Timestamp:             timestamp,
	}
	if item.Control != nil {
		report.ItemKind = QueueItemControl
		report.ControlKind = item.Control.Kind
		report.ItemID = item.Control.ID
		report.Priority = item.Control.Priority
	}
	return report
}

func (s *Scheduler) completePendingControl(handle queueHandle, err error) {
	removed, queueDepthAtRemoval, ok := s.queue.remove(handle)
	if !ok || removed.Control == nil {
		return
	}
	if queueDepthAtRemoval > 0 {
		s.reportQueueDepth(queueDepthAtRemoval - 1)
	}
	s.completeControlWithOutcome(removed, err, queueDepthAtRemoval)
}

func (s *Scheduler) afterControlDeadline(handle queueHandle, control *ControlItem) func() bool {
	if control == nil || control.Deadline.IsZero() {
		return func() bool { return true }
	}
	delay := time.Until(control.Deadline)
	if delay <= 0 {
		s.completePendingControl(handle, ErrPlayItemExpired)
		return func() bool { return false }
	}
	timer := time.AfterFunc(delay, func() {
		s.completePendingControl(handle, ErrPlayItemExpired)
	})
	return timer.Stop
}

func (control *ControlItem) complete(err error) bool {
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.completed {
		return false
	}
	control.err = err
	control.completed = true
	close(control.done)
	return true
}

func (control *ControlItem) result() error {
	control.mu.Lock()
	defer control.mu.Unlock()
	return control.err
}

func (control *ControlItem) isCompleted() bool {
	control.mu.Lock()
	defer control.mu.Unlock()
	return control.completed
}

func (control *ControlItem) ctxErr() error {
	if control.ctx == nil {
		return nil
	}
	return control.ctx.Err()
}

func (s *Scheduler) finish(ctx context.Context, item PlayItem) error {
	if item.OnFinish == nil {
		return nil
	}
	return item.OnFinish(ctx)
}

func (s *Scheduler) playFrames(ctx context.Context, frames []Frame, deadline time.Time) error {
	for _, frame := range frames {
		if !deadline.IsZero() && !s.now().Before(deadline) {
			return ErrPlayItemExpired
		}
		packed := s.packer.Pack(frame)
		if err := s.client.SetFrame(ctx, packed); err != nil {
			return err
		}
		s.setConnected(true)
		s.rememberDisplayState(displayState{
			Kind:  displayStateFrame,
			Frame: packed,
		})
		if err := sleepContext(ctx, frame.Delay); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) restore(ctx context.Context, policy animations.RestorePolicy, previous displayState) error {
	if policy == "" {
		policy = animations.RestoreLeave
	}
	spec, ok := restorePolicySpecs[policy]
	if !ok {
		return fmt.Errorf("unsupported restore policy %q", policy)
	}
	return spec(ctx, s, previous)
}

func (s *Scheduler) applyDesiredBackground(ctx context.Context, force bool) error {
	if !s.canApplyDesiredBackground(force) {
		return nil
	}
	s.setState(StateRestoringBackground)
	s.markBackgroundRestoreAttempt()
	var err error
	switch preset, color := s.backgroundPreset(); {
	case preset != nil:
		err = s.restoreFirmwarePreset(ctx, *preset)
	case color != nil:
		err = s.restoreStaticColorBackground(ctx, *color)
	default:
		err = s.restoreRenderableBackground(ctx)
	}
	if err == nil {
		s.markDesiredBackgroundClean()
	} else {
		s.markBackgroundRestoreFailure(ctx, err)
	}
	return err
}

// backgroundPreset resolves the configured background to whichever firmware-side
// representation backs it. Exactly one of the returns is non-nil; both nil means
// the background is a renderable animation driven frame-by-frame from here.
func (s *Scheduler) backgroundPreset() (*animations.FirmwarePreset, *animations.RGB) {
	if preset, ok := s.registry.FirmwarePreset(s.background.AnimationID); ok {
		return &preset, nil
	}
	if color, ok := s.registry.StaticColor(s.background.AnimationID); ok {
		return nil, &color
	}
	return nil, nil
}

func (s *Scheduler) restoreStaticColorBackground(ctx context.Context, color animations.RGB) error {
	rgb := RGB{R: color.R, G: color.G, B: color.B}
	err := s.retryBackgroundMatrix(ctx, func() error {
		return s.client.SetStaticColor(ctx, rgb)
	})
	if err == nil {
		s.rememberDisplayState(displayState{
			Kind:         displayStateStatic,
			Color:        rgb,
			BackgroundID: s.background.AnimationID,
		})
	}
	return err
}

func (s *Scheduler) restoreFirmwarePreset(ctx context.Context, preset animations.FirmwarePreset) error {
	err := s.retryBackgroundMatrix(ctx, func() error {
		return s.client.SetPreset(ctx, preset.EffectID, preset.Interval, preset.Color)
	})
	if err == nil {
		s.rememberDisplayState(displayState{
			Kind:         displayStatePreset,
			EffectID:     preset.EffectID,
			Interval:     preset.Interval,
			Color:        presetColorForState(preset.EffectID, RGB{R: preset.Color.R, G: preset.Color.G, B: preset.Color.B}),
			BackgroundID: s.background.AnimationID,
		})
	}
	return err
}

func (s *Scheduler) restoreRenderableBackground(ctx context.Context) error {
	animation, ok := s.registry.Get(s.background.AnimationID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMissingAnimation, s.background.AnimationID)
	}
	renderStart := time.Now()
	frames, err := animation.Render(ctx, s.background.Params)
	s.reportAnimationRendered(s.background.AnimationID, time.Since(renderStart))
	if err != nil {
		return fmt.Errorf("render background animation %q: %w", s.background.AnimationID, err)
	}
	if len(frames) == 0 {
		return fmt.Errorf("%w: %s", ErrEmptyAnimation, s.background.AnimationID)
	}

	err = s.retryBackgroundMatrix(ctx, func() error {
		return s.playFrames(ctx, frames, time.Time{})
	})
	if err == nil {
		s.rememberDisplayState(displayState{
			Kind:         displayStateFrame,
			Frame:        s.packer.Pack(frames[len(frames)-1]),
			BackgroundID: s.background.AnimationID,
		})
	}
	return err
}

func (s *Scheduler) displayStateMatchesConfiguredBackground(state displayState) bool {
	if s.background.AnimationID == "" {
		return false
	}
	if preset, ok := s.registry.FirmwarePreset(s.background.AnimationID); ok {
		return state.Kind == displayStatePreset &&
			state.EffectID == preset.EffectID &&
			state.Interval == preset.Interval &&
			state.Color == presetColorForState(preset.EffectID, RGB{R: preset.Color.R, G: preset.Color.G, B: preset.Color.B})
	}
	if color, ok := s.registry.StaticColor(s.background.AnimationID); ok {
		return state.Kind == displayStateStatic &&
			state.Color == RGB{R: color.R, G: color.G, B: color.B}
	}
	return state.Kind == displayStateFrame && state.BackgroundID == s.background.AnimationID
}

// retryBackgroundMatrix makes exactly one attempt and reports failure upward.
//
// Background convergence owns its own retry ladder (scheduleBackgroundRetryLocked),
// so on a retryable error this waits for the link to come back and then returns the
// error, letting the caller mark the background dirty and schedule a backoff retry.
// Looping until success here instead would converge inline and starve that ladder:
// the background would report converged while the panel never received the command.
func (s *Scheduler) retryBackgroundMatrix(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil {
		s.markMatrixSuccess(s.State())
		return nil
	}
	if ClassifyError(ctx, err) != ErrorKindRetryable {
		return err
	}
	s.markMatrixFailure(StateDisconnected)
	if waitErr := s.waitReady(ctx, time.Time{}); waitErr != nil {
		return waitErr
	}
	return err
}

func (s *Scheduler) retryMatrix(ctx context.Context, fn func() error) error {
	return s.retryMatrixUntilReady(ctx, fn)
}

func (s *Scheduler) retryMatrixUntilReady(ctx context.Context, fn func() error) error {
	for {
		if err := fn(); err != nil {
			if err := s.retryMatrixError(ctx, time.Time{}, err, ClassifyError); err != nil {
				return err
			}
			continue
		}
		s.markMatrixSuccess(s.State())
		return nil
	}
}

func (s *Scheduler) restoreDisplayState(ctx context.Context, state displayState) error {
	restoreDisplayState, ok := displayStateRestoreSpecs[state.Kind]
	if !ok {
		return nil
	}
	err := restoreDisplayState(ctx, s, state)
	if err == nil {
		s.rememberDisplayState(state)
	}
	return err
}

func (s *Scheduler) retryControlMatrix(ctx context.Context, control *ControlItem, fn func() error) error {
	for {
		if control != nil && !control.Deadline.IsZero() && !s.now().Before(control.Deadline) {
			return ErrPlayItemExpired
		}
		if err := fn(); err != nil {
			if control != nil && !control.Deadline.IsZero() && !s.now().Before(control.Deadline) {
				return ErrPlayItemExpired
			}
			deadline := time.Time{}
			if control != nil {
				deadline = control.Deadline
			}
			if err := s.retryMatrixError(ctx, deadline, err, ClassifyError); err != nil {
				return err
			}
			continue
		}
		s.markMatrixSuccess(s.State())
		return nil
	}
}

func (s *Scheduler) waitReady(ctx context.Context, deadline time.Time) error {
	s.setState(StateDisconnected)
	for {
		if !deadline.IsZero() && !s.now().Before(deadline) {
			s.reportReconnectFailure(ReconnectFailureDeadlineExceeded, ErrPlayItemExpired)
			return ErrPlayItemExpired
		}
		s.setState(StateConnecting)
		err, probeTimedOut := s.pingProbe(ctx, deadline)
		if err == nil {
			s.markMatrixSuccess(StateReady)
			return nil
		} else {
			if ctxErr := ctx.Err(); ctxErr != nil {
				s.reportReconnectFailure(reconnectFailureOutcomeFromError(ctxErr), ctxErr)
				return ctxErr
			}
			s.reportProbeFailure(ctx, err, probeTimedOut)
			if !deadline.IsZero() && !s.now().Before(deadline) {
				s.reportReconnectFailure(ReconnectFailureDeadlineExceeded, ErrPlayItemExpired)
				return ErrPlayItemExpired
			}
			if _, ok := probeRetryableKinds[s.classifyProbeError(ctx, err)]; !ok {
				return cancellationOr(ctx, err)
			}
		}
		s.markMatrixFailure(StateDisconnected)

		delay, err := s.nextReconnectDelay(deadline, err)
		if err != nil {
			if errors.Is(err, ErrPlayItemExpired) {
				s.reportReconnectFailure(ReconnectFailureDeadlineExceeded, err)
			}
			return err
		}
		if err := sleepContext(ctx, delay); err != nil {
			s.reportReconnectFailure(reconnectFailureOutcomeFromError(err), err)
			return err
		}
	}
}

func (s *Scheduler) pingProbe(ctx context.Context, deadline time.Time) (error, bool) {
	probeCtx, cancel := s.probeContext(ctx, deadline)
	defer cancel()
	err := s.client.Ping(probeCtx)
	probeTimedOut := err != nil &&
		errors.Is(probeCtx.Err(), context.DeadlineExceeded) &&
		(ctx == nil || ctx.Err() == nil)
	return err, probeTimedOut
}

func (s *Scheduler) probeContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	timeout := s.probeTimeout
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *Scheduler) nextReconnectDelay(deadline time.Time, retryErr error) (time.Duration, error) {
	attempt := s.incrementReconnectAttempt()
	base := exponentialReconnectDelay(attempt, s.reconnectMinDelay, s.reconnectMaxDelay)
	delay := s.reconnectJitter(base)
	if delay <= 0 {
		delay = base
	}
	if delay > s.reconnectMaxDelay {
		delay = s.reconnectMaxDelay
	}

	deadlineCapped := false
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, ErrPlayItemExpired
		}
		if remaining < delay {
			delay = remaining
			deadlineCapped = true
		}
	}

	if s.onReconnectDelay != nil {
		errText := ""
		if retryErr != nil {
			errText = retryErr.Error()
		}
		s.callbackPanics.Run(observabilityCallbackReconnectDelay, func() {
			s.onReconnectDelay(ReconnectAttempt{
				Source:         ReconnectSourceSchedulerBackoff,
				Attempt:        attempt,
				BaseDelay:      base,
				Delay:          delay,
				DeadlineCapped: deadlineCapped,
				ErrorKind:      ErrorKindRetryable,
				Error:          errText,
			})
		})
	}
	return delay, nil
}

func (s *Scheduler) incrementReconnectAttempt() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconnectAttempt++
	return s.reconnectAttempt
}

func (s *Scheduler) currentReconnectAttempt() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reconnectAttempt
}

func exponentialReconnectDelay(attempt int, minDelay, maxDelay time.Duration) time.Duration {
	if attempt <= 1 {
		return minDelay
	}
	delay := minDelay
	for i := 1; i < attempt; i++ {
		if delay >= maxDelay {
			return maxDelay
		}
		next := delay * 2
		if next < delay || next > maxDelay {
			return maxDelay
		}
		delay = next
	}
	return delay
}

func defaultReconnectJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	window := base / 5
	if window <= 0 {
		return base
	}
	return base - time.Duration(rand.Int63n(int64(window)+1))
}

// cancellationOr reports ctx's own error in place of err once ctx is done.
//
// Both classifiers treat every error as permanent the moment the context is
// cancelled, so an ordinary "connection refused" from a probe that raced
// cancellation is indistinguishable from a genuine permanent failure. Checking
// the context before the probe is not enough: cancellation can land between
// that check and the classification, and then a retryable transport error
// escapes Run as a real failure -- which is how Shutdown came to report a
// connection error for a device the caller had just deliberately stopped.
//
// Anywhere a "permanent" verdict would end the run, ask the context first.
func cancellationOr(ctx context.Context, err error) error {
	if ctx == nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func (s *Scheduler) classifyProbeError(ctx context.Context, err error) ErrorKind {
	if err == nil {
		return ErrorKindNone
	}
	if ctx != nil && ctx.Err() != nil {
		return ErrorKindPermanent
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindRetryable
	}
	return ClassifyError(ctx, err)
}

func (s *Scheduler) reportProbeFailure(ctx context.Context, err error, probeTimedOut bool) {
	if s.onProbeFailure == nil {
		return
	}
	reason := ProbeFailurePermanent
	errorKind := s.classifyProbeError(ctx, err)
	if probeTimedOut {
		reason = ProbeFailureProbeTimeout
		errorKind = ErrorKindRetryable
	} else if errorKind == ErrorKindRetryable {
		reason = ProbeFailureTransport
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	failure := ProbeFailure{
		ErrorKind: errorKind,
		Reason:    reason,
		Error:     errText,
	}
	s.callbackPanics.Run(observabilityCallbackProbeFailure, func() {
		s.onProbeFailure(failure)
	})
}

func reconnectFailureOutcomeFromError(err error) ReconnectFailureOutcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPlayItemExpired) {
		return ReconnectFailureDeadlineExceeded
	}
	return ReconnectFailureCanceled
}

func (s *Scheduler) reportReconnectFailure(outcome ReconnectFailureOutcome, err error) {
	if s.onReconnectFailure == nil {
		return
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	failure := ReconnectFailure{
		Source:    ReconnectSourceSchedulerBackoff,
		Attempt:   s.currentReconnectAttempt(),
		ErrorKind: ClassifyError(context.Background(), err),
		Outcome:   outcome,
		Error:     errText,
	}
	s.callbackPanics.Run(observabilityCallbackReconnectFailure, func() {
		s.onReconnectFailure(failure)
	})
}

func (s *Scheduler) expired(item PlayItem) bool {
	return !item.Deadline.IsZero() && !s.now().Before(item.Deadline)
}

func (s *Scheduler) snapshotDisplayState() displayState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.displayState
}

func (s *Scheduler) rememberControlDisplayState(control *ControlItem) {
	if control == nil {
		return
	}
	spec, ok := resolveControlSpec(control.Kind)
	if !ok || spec.rememberState == nil {
		return
	}
	spec.rememberState(s, control)
}

func (s *Scheduler) rememberDisplayState(state displayState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displayState = state
}

func (s *Scheduler) shouldApplyDesiredBackground() bool {
	return s.canApplyDesiredBackground(false)
}

func (s *Scheduler) canApplyDesiredBackground(force bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.background.AnimationID == "" || !s.connected {
		return false
	}
	if !force && !s.desiredBackgroundDirty && s.backgroundConvergenceState != BackgroundConvergenceUnknown {
		return false
	}
	return s.backgroundRetryDueLocked(s.now())
}

func (s *Scheduler) backgroundRetryDueLocked(now time.Time) bool {
	return s.backgroundNextRestoreAttempt.IsZero() || !now.Before(s.backgroundNextRestoreAttempt)
}

func (s *Scheduler) markDesiredBackgroundDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundDirtyLocked(false)
}

func (s *Scheduler) markDesiredBackgroundDirtyAfterControl(control *ControlItem) {
	if control == nil {
		return
	}
	spec, ok := resolveControlSpec(control.Kind)
	if !ok || !spec.marksBackgroundDirty {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundDirtyLocked(true)
}

func (s *Scheduler) markDesiredBackgroundDirtyLocked(resetRetry bool) {
	if s.background.AnimationID == "" {
		return
	}
	s.desiredBackgroundDirty = true
	if resetRetry {
		s.resetBackgroundRetryLocked()
	}
	s.backgroundConvergenceState = s.backgroundDirtyStateLocked(s.now())
}

func (s *Scheduler) backgroundDirtyStateLocked(now time.Time) BackgroundConvergenceState {
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	return ProjectBackgroundConvergence(BackgroundConvergenceProjectionInput{
		State:                 s.backgroundConvergenceState,
		Dirty:                 s.desiredBackgroundDirty,
		LastRestoreError:      s.backgroundLastRestoreError,
		LastRestoreErrorClass: s.backgroundLastRestoreErrorClass,
		NextRetry:             nextRetry,
		FailureCount:          s.backgroundRetryFailureCount,
	}, now).State
}

func (s *Scheduler) markDesiredBackgroundClean() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundConvergedLocked()
	s.backgroundLastRestoreSuccess = s.now().UTC()
}

func (s *Scheduler) markDesiredBackgroundConverged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDesiredBackgroundConvergedLocked()
}

func (s *Scheduler) markDesiredBackgroundConvergedLocked() {
	s.desiredBackgroundDirty = false
	s.backgroundConvergenceState = BackgroundConvergenceConverged
	s.backgroundLastRestoreError = ""
	s.backgroundLastRestoreErrorClass = ErrorKindNone
	s.resetBackgroundRetryLocked()
}

func (s *Scheduler) markBackgroundRestoreAttempt() {
	s.mu.Lock()
	s.desiredBackgroundDirty = true
	s.backgroundConvergenceState = BackgroundConvergenceAttempting
	s.backgroundLastRestoreAttempt = s.now().UTC()
	failureCount := s.backgroundRetryFailureCount
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	event := BackgroundRestoreEvent{
		AnimationID:  s.background.AnimationID,
		Kind:         s.backgroundKind,
		State:        s.backgroundConvergenceState,
		ErrorKind:    ErrorKindNone,
		NextRetry:    nextRetry,
		FailureCount: failureCount,
	}
	s.mu.Unlock()
	s.reportBackgroundRestore(event)
}

func (s *Scheduler) markBackgroundRestoreFailure(ctx context.Context, err error) {
	errorClass := classifyBackgroundRestoreError(ctx, err)
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	s.mu.Lock()
	now := s.now().UTC()
	s.desiredBackgroundDirty = true
	s.backgroundConvergenceState = BackgroundConvergenceRetrying
	s.backgroundLastRestoreError = errText
	s.backgroundLastRestoreErrorClass = errorClass
	s.lastFailure = now
	s.scheduleBackgroundRetryLocked(now, errorClass)
	failureCount := s.backgroundRetryFailureCount
	var nextRetry *time.Time
	if !s.backgroundNextRestoreAttempt.IsZero() {
		nextRetryValue := s.backgroundNextRestoreAttempt
		nextRetry = &nextRetryValue
	}
	event := BackgroundRestoreEvent{
		AnimationID:  s.background.AnimationID,
		Kind:         s.backgroundKind,
		State:        s.backgroundConvergenceState,
		ErrorKind:    errorClass,
		Error:        errText,
		NextRetry:    nextRetry,
		FailureCount: failureCount,
	}
	s.mu.Unlock()
	s.reportBackgroundRestore(event)
}

func (s *Scheduler) resetBackgroundRetryLocked() {
	s.backgroundRetryFailureCount = 0
	s.backgroundRetryLastErrorClass = ErrorKindNone
	s.backgroundNextRestoreAttempt = time.Time{}
}

func (s *Scheduler) scheduleBackgroundRetryLocked(now time.Time, errorClass ErrorKind) {
	if errorClass != ErrorKindRetryable && errorClass != ErrorKindPermanent {
		errorClass = ErrorKindPermanent
	}
	if errorClass != s.backgroundRetryLastErrorClass {
		s.backgroundRetryFailureCount = 0
		s.backgroundRetryLastErrorClass = errorClass
	}
	s.backgroundRetryFailureCount++
	minDelay, maxDelay := backgroundRetryBounds(errorClass)
	delay := exponentialReconnectDelay(s.backgroundRetryFailureCount, minDelay, maxDelay)
	s.backgroundNextRestoreAttempt = now.Add(delay)
}

func backgroundRetryBounds(errorClass ErrorKind) (time.Duration, time.Duration) {
	if errorClass == ErrorKindRetryable {
		return backgroundRetryableMinDelay, backgroundRetryableMaxDelay
	}
	return backgroundPermanentMinDelay, backgroundPermanentMaxDelay
}

func (s *Scheduler) reportBackgroundRestore(event BackgroundRestoreEvent) {
	if s.onBackgroundRestore == nil {
		return
	}
	s.callbackPanics.Run(observabilityCallbackBackgroundRestore, func() {
		s.onBackgroundRestore(event)
	})
}

func classifyBackgroundRestoreError(ctx context.Context, err error) ErrorKind {
	if err == nil {
		return ErrorKindNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindPermanent
	}
	if errors.Is(err, ErrMissingAnimation) || errors.Is(err, ErrEmptyAnimation) || isPermanentMatrixError(err) {
		return ErrorKindPermanent
	}
	return ErrorKindRetryable
}

func clientReconnectRecoveryCount(client Client) uint64 {
	counter, ok := client.(clientReconnectRecoveryCounter)
	if !ok {
		return 0
	}
	return counter.reconnectRecoveryCount()
}

func (s *Scheduler) setState(state State) {
	var changedToDisconnected bool
	s.mu.Lock()
	previousConnected := s.connected
	s.state = state
	if state == StateDisconnected || state == StateDraining {
		s.connected = false
	}
	if previousConnected && !s.connected && s.background.AnimationID != "" {
		s.markDesiredBackgroundDirtyLocked(false)
	}
	changedToDisconnected = previousConnected && !s.connected
	s.mu.Unlock()
	if changedToDisconnected {
		s.notifyMatrixConnectedChange(false)
	}
}

func (s *Scheduler) setConnected(connected bool) {
	var changed bool
	recoveryCount := clientReconnectRecoveryCount(s.client)
	s.mu.Lock()
	previousConnected := s.connected
	s.connected = connected
	if connected {
		recovered := !previousConnected || recoveryCount != s.clientReconnectRecoveries
		if recoveryCount != s.clientReconnectRecoveries {
			s.clientReconnectRecoveries = recoveryCount
		}
		if recovered {
			s.markDesiredBackgroundDirtyLocked(true)
		}
		s.reconnectAttempt = 0
		s.lastSuccess = s.now().UTC()
	} else {
		s.lastFailure = s.now().UTC()
		if previousConnected && s.background.AnimationID != "" {
			s.markDesiredBackgroundDirtyLocked(false)
		}
	}
	changed = previousConnected != connected
	s.mu.Unlock()
	if changed {
		s.notifyMatrixConnectedChange(connected)
	}
}

func (s *Scheduler) markMatrixSuccess(state State) {
	var recovery *ReconnectRecovery
	var connectedChanged bool
	recoveryCount := clientReconnectRecoveryCount(s.client)
	s.mu.Lock()
	previousConnected := s.connected
	attempt := s.reconnectAttempt
	s.state = state
	s.connected = true
	clientRecovered := recoveryCount != s.clientReconnectRecoveries
	if clientRecovered {
		s.clientReconnectRecoveries = recoveryCount
	}
	if s.background.AnimationID != "" {
		if !previousConnected || clientRecovered {
			s.markDesiredBackgroundDirtyLocked(true)
		}
	}
	s.reconnectAttempt = 0
	s.lastSuccess = s.now().UTC()
	if !previousConnected && attempt > 0 {
		recovery = &ReconnectRecovery{Source: ReconnectSourceSchedulerBackoff, Attempt: attempt, State: state}
	}
	connectedChanged = !previousConnected
	s.mu.Unlock()
	if connectedChanged {
		s.notifyMatrixConnectedChange(true)
	}
	if recovery != nil && s.onReconnectRecovered != nil {
		s.callbackPanics.Run(observabilityCallbackReconnectRecovered, func() {
			s.onReconnectRecovered(*recovery)
		})
	}
}

func (s *Scheduler) markMatrixFailure(state State) {
	var connectedChanged bool
	s.mu.Lock()
	previousConnected := s.connected
	s.state = state
	s.connected = false
	if previousConnected && s.background.AnimationID != "" {
		s.markDesiredBackgroundDirtyLocked(false)
	}
	s.lastFailure = s.now().UTC()
	connectedChanged = previousConnected
	s.mu.Unlock()
	if connectedChanged {
		s.notifyMatrixConnectedChange(false)
	}
}

func (s *Scheduler) notifyMatrixConnectedChange(connected bool) {
	if s.onMatrixConnectedChange != nil {
		s.callbackPanics.Run(observabilityCallbackMatrixConnectedChange, func() {
			s.onMatrixConnectedChange(connected)
		})
	}
}

func applyMaxDuration(frames []animations.Frame, maxDuration time.Duration) []animations.Frame {
	copied := make([]animations.Frame, 0, len(frames))
	if maxDuration <= 0 {
		return append(copied, frames...)
	}

	var elapsed time.Duration
	for _, frame := range frames {
		if elapsed >= maxDuration {
			break
		}
		next := frame
		if next.Delay > maxDuration-elapsed {
			next.Delay = maxDuration - elapsed
		}
		copied = append(copied, next)
		elapsed += next.Delay
	}

	return copied
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ObservabilityCallbackPanics is the total number of panics recovered from
// best-effort observability callbacks.
func (s *Scheduler) ObservabilityCallbackPanics() uint64 {
	return s.callbackPanics.Total()
}

// ObservabilityCallbackPanicCounts breaks that total down by callback name.
func (s *Scheduler) ObservabilityCallbackPanicCounts() map[string]uint64 {
	return s.callbackPanics.Counts()
}
