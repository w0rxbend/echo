package matrix

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/worxbend/echo/internal/animations"
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
)

type AnimationRegistry interface {
	Get(id string) (animations.Animation, bool)
	FirmwarePreset(id string) (animations.FirmwarePreset, bool)
}

type clientReconnectRecoveryCounter interface {
	reconnectRecoveryCount() uint64
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
	AnimationID string
	Kind        BackgroundKind
	State       BackgroundConvergenceState
	ErrorKind   ErrorKind
	Error       string
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

	observabilityMu                   sync.Mutex
	observabilityCallbackPanicCounts  map[string]uint64
	observabilityCallbackPanicCounter atomic.Uint64
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
		observabilityCallbackPanicCounts:  make(map[string]uint64),
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
	return BackgroundKindRenderable
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
	return nil
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
	switch request.Kind {
	case ControlClear, ControlSetBrightness, ControlFill:
	case ControlSetPreset:
		if _, err := durationMilliseconds(request.Interval, "preset interval"); err != nil {
			return ScheduledItem{}, err
		}
	case "":
		return ScheduledItem{}, fmt.Errorf("%w: control kind is required", ErrInvalidControl)
	default:
		return ScheduledItem{}, fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, request.Kind)
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
	frames = applyMaxDuration(frames, request.MaxDuration)
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

	return ScheduledItem{
		PlayItem: PlayItem{
			ID:       id,
			EventID:  request.EventID,
			Priority: request.Priority,
			Frames:   frames,
			Loop:     animations.LoopNone,
		},
		AnimationID:         request.AnimationID,
		RestorePolicy:       restore,
		CreatedAt:           createdAt,
		animationCompletion: &animationCompletion{},
	}, nil
}

func (s *Scheduler) Run(ctx context.Context) error {
	defer s.Close()
	defer s.completeQueuedControls(ErrSchedulerStopped)

	if err := s.waitReady(ctx, time.Time{}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.setState(StateDraining)
			return nil
		}
		return err
	}
	deferBackgroundRestore := false
runLoop:
	for {
		if deferBackgroundRestore {
			deferBackgroundRestore = false
		} else if s.shouldApplyDesiredBackground() && s.queue.len() == 0 {
			if err := s.applyDesiredBackground(ctx, false); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				s.setState(StateDraining)
				return nil
			}
			return err
		}
		if !ok {
			if err := s.heartbeat(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
			if item.Control.isCompleted() {
				continue
			}
			if err := item.Control.ctxErr(); err != nil {
				s.completeControlWithOutcome(item, err, 0)
				continue
			}
			err = s.controlTerminalError(ctx, s.executeControl(ctx, item.Control))
			s.completeControlWithOutcome(item, err, 0)
			if ctx.Err() != nil {
				s.setState(StateDraining)
				return nil
			}
			s.setState(StateReady)
			continue
		}

		preItemState := s.snapshotDisplayState()
		// Ordinary playback is transient. Once a playback item is selected,
		// the configured background is again the desired eventual idle state;
		// item restore policies may affect only the immediate post-playback
		// display unless they explicitly force a background restore.
		s.markDesiredBackgroundDirty()
		var terminalErr error
		for {
			if s.expired(item.PlayItem) {
				terminalErr = ErrPlayItemExpired
				break
			}

			s.setState(StatePlayingTransient)
			err = s.playItem(ctx, item.PlayItem)
			if err == nil {
				if restoreErr := s.restore(ctx, item.RestorePolicy, preItemState); restoreErr != nil {
					terminalErr = s.animationTerminalError(ctx, restoreErr)
					if errors.Is(terminalErr, ErrSchedulerStopped) {
						s.setState(StateDraining)
						s.completeAnimationWithOutcome(item, terminalErr, 0)
						return nil
					}
					s.setState(StateReady)
					s.completeAnimationWithOutcome(item, terminalErr, 0)
					if errors.Is(terminalErr, context.Canceled) || errors.Is(terminalErr, context.DeadlineExceeded) || errors.Is(terminalErr, ErrPlayItemExpired) {
						continue runLoop
					}
					return restoreErr
				}
				s.setState(StateReady)
				terminalErr = nil
				break
			}
			if errors.Is(err, ErrEmptyAnimation) || errors.Is(err, ErrPlayItemExpired) {
				s.setState(StateReady)
				terminalErr = err
				break
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				terminalErr = s.animationTerminalError(ctx, err)
				if errors.Is(terminalErr, ErrSchedulerStopped) {
					s.setState(StateDraining)
					s.completeAnimationWithOutcome(item, terminalErr, 0)
					return nil
				}
				s.setState(StateReady)
				break
			}
			if IsPermanentError(ctx, err) {
				s.completeAnimationWithOutcome(item, err, 0)
				return err
			}
			s.markMatrixFailure(StateDisconnected)
			if err := s.waitReady(ctx, item.PlayItem.Deadline); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPlayItemExpired) {
					terminalErr = s.animationTerminalError(ctx, err)
					if errors.Is(terminalErr, ErrSchedulerStopped) {
						s.setState(StateDraining)
					}
					break
				}
				s.completeAnimationWithOutcome(item, err, 0)
				return err
			}
		}
		s.completeAnimationWithOutcome(item, terminalErr, 0)
	}
}

func (s *Scheduler) animationTerminalError(runCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if runCtx.Err() != nil {
			return ErrSchedulerStopped
		}
		return err
	}
	return err
}

func (s *Scheduler) controlTerminalError(runCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if runCtx.Err() != nil {
			return ErrSchedulerStopped
		}
		return err
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
			return err
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

	switch item.Loop {
	case animations.LoopForever:
		for {
			if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
				return err
			}
			if !item.Deadline.IsZero() && !s.now().Before(item.Deadline) {
				return s.finish(ctx, item)
			}
		}
	case animations.LoopUntil:
		for item.Deadline.IsZero() || s.now().Before(item.Deadline) {
			if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
				return err
			}
		}
	default:
		if err := s.playFrames(ctx, item.Frames, item.Deadline); err != nil {
			return err
		}
	}

	return s.finish(ctx, item)
}

func (s *Scheduler) executeControl(ctx context.Context, control *ControlItem) error {
	if control == nil {
		return nil
	}
	controlCtx := control.ctx
	if controlCtx == nil {
		controlCtx = context.Background()
	}
	if err := controlCtx.Err(); err != nil {
		return err
	}

	var run func(context.Context) error
	switch control.Kind {
	case ControlClear:
		run = func(ctx context.Context) error {
			return s.client.Clear(ctx)
		}
	case ControlSetBrightness:
		run = func(ctx context.Context) error {
			return s.client.SetBrightness(ctx, control.Brightness)
		}
	case ControlSetPreset:
		run = func(ctx context.Context) error {
			return s.client.SetPreset(ctx, control.EffectID, control.Interval, control.Color)
		}
	case ControlFill:
		run = func(ctx context.Context) error {
			return s.client.Fill(ctx, control.Color)
		}
	default:
		return fmt.Errorf("%w: unsupported control kind %q", ErrInvalidControl, control.Kind)
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
		return run(execCtx)
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

func (s *Scheduler) completeClearedQueueItem(item ScheduledItem) {
	s.completeQueueClearedItemWithOutcome(item, 0)
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

func (s *Scheduler) ObservabilityCallbackPanics() uint64 {
	return s.observabilityCallbackPanicCounter.Load()
}

func (s *Scheduler) ObservabilityCallbackPanicCounts() map[string]uint64 {
	s.observabilityMu.Lock()
	defer s.observabilityMu.Unlock()
	if len(s.observabilityCallbackPanicCounts) == 0 {
		return nil
	}
	counts := make(map[string]uint64, len(s.observabilityCallbackPanicCounts))
	for name, count := range s.observabilityCallbackPanicCounts {
		counts[name] = count
	}
	return counts
}

func (s *Scheduler) runObservabilityCallback(name string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			s.recordObservabilityCallbackPanic(name)
		}
	}()
	fn()
}

func (s *Scheduler) recordObservabilityCallbackPanic(name string) {
	s.observabilityCallbackPanicCounter.Add(1)
	s.observabilityMu.Lock()
	defer s.observabilityMu.Unlock()
	if s.observabilityCallbackPanicCounts == nil {
		s.observabilityCallbackPanicCounts = make(map[string]uint64)
	}
	s.observabilityCallbackPanicCounts[name]++
}

func (s *Scheduler) notifyIdle() {
	if s.onIdle != nil {
		s.onIdle()
	}
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
	switch policy {
	case "", animations.RestoreLeave:
		return nil
	case animations.RestoreClear:
		err := s.retryMatrix(ctx, func() error {
			return s.client.Clear(ctx)
		})
		if err == nil {
			s.rememberDisplayState(displayState{Kind: displayStateClear})
		}
		return err
	case animations.RestoreBlank:
		blank := RGB{}
		err := s.retryMatrix(ctx, func() error {
			return s.client.Fill(ctx, blank)
		})
		if err == nil {
			s.rememberDisplayState(displayState{
				Kind:  displayStateFill,
				Color: blank,
			})
		}
		return err
	case animations.RestorePreviousFrame:
		if !previous.known() {
			return nil
		}
		err := s.restoreDisplayState(ctx, previous)
		if err == nil && s.displayStateMatchesConfiguredBackground(previous) {
			s.markDesiredBackgroundConverged()
		}
		return err
	case animations.RestoreBackground:
		// restore: background is the only playback restore policy that applies
		// the scheduler-owned desired background immediately instead of waiting
		// for the next idle convergence pass.
		return s.applyDesiredBackground(ctx, true)
	default:
		return fmt.Errorf("unsupported restore policy %q", policy)
	}
}

func (s *Scheduler) applyDesiredBackground(ctx context.Context, force bool) error {
	if !s.canApplyDesiredBackground(force) {
		return nil
	}
	s.setState(StateRestoringBackground)
	s.markBackgroundRestoreAttempt()
	var err error
	if preset, ok := s.registry.FirmwarePreset(s.background.AnimationID); ok {
		err = s.restoreFirmwarePreset(ctx, preset)
	} else {
		err = s.restoreRenderableBackground(ctx)
	}
	if err == nil {
		s.markDesiredBackgroundClean()
	} else {
		s.markBackgroundRestoreFailure(ctx, err)
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
			Color:        preset.Color,
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
			state.Color == preset.Color
	}
	return state.Kind == displayStateFrame && state.BackgroundID == s.background.AnimationID
}

func (s *Scheduler) retryBackgroundMatrix(ctx context.Context, fn func() error) error {
	if err := fn(); err != nil {
		switch ClassifyError(ctx, err) {
		case ErrorKindPermanent:
			return err
		case ErrorKindRetryable:
			s.markMatrixFailure(StateDisconnected)
			if waitErr := s.waitReady(ctx, time.Time{}); waitErr != nil {
				return waitErr
			}
			return err
		default:
			return err
		}
	}
	s.markMatrixSuccess(s.State())
	return nil
}

func (s *Scheduler) restoreDisplayState(ctx context.Context, state displayState) error {
	var err error
	switch state.Kind {
	case displayStateFrame:
		err = s.retryMatrix(ctx, func() error {
			return s.client.SetFrame(ctx, state.Frame)
		})
	case displayStateFill:
		err = s.retryMatrix(ctx, func() error {
			return s.client.Fill(ctx, state.Color)
		})
	case displayStateClear:
		err = s.retryMatrix(ctx, func() error {
			return s.client.Clear(ctx)
		})
	case displayStatePreset:
		err = s.retryMatrix(ctx, func() error {
			return s.client.SetPreset(ctx, state.EffectID, state.Interval, state.Color)
		})
	default:
		return nil
	}
	if err == nil {
		s.rememberDisplayState(state)
	}
	return err
}

func (s *Scheduler) retryMatrix(ctx context.Context, fn func() error) error {
	for {
		if err := fn(); err != nil {
			switch ClassifyError(ctx, err) {
			case ErrorKindPermanent:
				return err
			case ErrorKindRetryable:
				s.markMatrixFailure(StateDisconnected)
				if waitErr := s.waitReady(ctx, time.Time{}); waitErr != nil {
					return waitErr
				}
				continue
			default:
				return err
			}
		}
		s.markMatrixSuccess(s.State())
		return nil
	}
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
			switch ClassifyError(ctx, err) {
			case ErrorKindPermanent:
				return err
			case ErrorKindRetryable:
				s.markMatrixFailure(StateDisconnected)
				deadline := time.Time{}
				if control != nil {
					deadline = control.Deadline
				}
				if waitErr := s.waitReady(ctx, deadline); waitErr != nil {
					return waitErr
				}
				continue
			default:
				return err
			}
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
			switch s.classifyProbeError(ctx, err) {
			case ErrorKindPermanent:
				return err
			case ErrorKindRetryable:
			default:
				return err
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
		s.runObservabilityCallback(observabilityCallbackReconnectDelay, func() {
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
	s.runObservabilityCallback(observabilityCallbackProbeFailure, func() {
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
	s.runObservabilityCallback(observabilityCallbackReconnectFailure, func() {
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
	switch control.Kind {
	case ControlClear:
		s.rememberDisplayState(displayState{Kind: displayStateClear})
	case ControlSetPreset:
		s.rememberDisplayState(displayState{
			Kind:     displayStatePreset,
			EffectID: control.EffectID,
			Interval: control.Interval,
			Color:    control.Color,
		})
	case ControlFill:
		s.rememberDisplayState(displayState{
			Kind:  displayStateFill,
			Color: control.Color,
		})
	}
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
	switch control.Kind {
	case ControlClear, ControlSetPreset, ControlFill:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.markDesiredBackgroundDirtyLocked(true)
	}
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
	event := BackgroundRestoreEvent{
		AnimationID: s.background.AnimationID,
		Kind:        s.backgroundKind,
		State:       s.backgroundConvergenceState,
		ErrorKind:   ErrorKindNone,
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
	event := BackgroundRestoreEvent{
		AnimationID: s.background.AnimationID,
		Kind:        s.backgroundKind,
		State:       s.backgroundConvergenceState,
		ErrorKind:   errorClass,
		Error:       errText,
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
	s.runObservabilityCallback(observabilityCallbackBackgroundRestore, func() {
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
		s.runObservabilityCallback(observabilityCallbackReconnectRecovered, func() {
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
		s.runObservabilityCallback(observabilityCallbackMatrixConnectedChange, func() {
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
