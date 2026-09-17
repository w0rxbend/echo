package matrix

import (
	"context"
	"errors"
	"fmt"
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
	observabilityCallbackQueueDepthChange      = ObservabilityCallbackQueueDepthChange
	observabilityCallbackAnimationRendered     = ObservabilityCallbackAnimationRendered
	observabilityCallbackItemOutcome           = ObservabilityCallbackItemOutcome
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
	Client        Client
	Registry      AnimationRegistry
	Packer        animations.LayoutPacker
	QueueCapacity int
	Background    BackgroundConfig
	// InitialBrightness is applied to the panel as soon as the scheduler first
	// reaches a ready matrix, and re-applied after every verified reconnect,
	// because a panel that rebooted comes back at its firmware default. nil
	// leaves the panel's own brightness alone; 0 is a meaningful setting
	// ("off"), so it cannot double as "unset".
	InitialBrightness       *byte
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
	queueDepthMu                      sync.Mutex
	background                        BackgroundConfig
	initialBrightness                 *byte
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
	brightnessDirty                 bool
	panelEnabled                    *bool
	panelEnabledDirty               bool
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

	s := &Scheduler{
		client:                            options.Client,
		registry:                          options.Registry,
		packer:                            options.Packer,
		queue:                             queue,
		background:                        options.Background,
		initialBrightness:                 options.InitialBrightness,
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
		onQueueDepthChange:                options.OnQueueDepthChange,
		heartbeatInterval:                 heartbeatInterval,
		probeTimeout:                      probeTimeout,
		now:                               now,
		state:                             StateDisconnected,
		backgroundKind:                    backgroundKind,
		backgroundConvergenceState:        BackgroundConvergenceUnknown,
		backgroundLastRestoreErrorClass:   ErrorKindNone,
		clientReconnectRecoveries:         clientReconnectRecoveryCount(options.Client),
	}
	// The dispatcher records its observer's panics into the scheduler's own
	// counter, so it is wired up after s exists rather than inside the literal.
	s.outcomeDispatcher = newOutcomeObserverDispatcher(options.OnItemOutcome, &s.callbackPanics)
	return s, nil
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
		s.reportQueueDepth()
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
	if s.panelEnabled != nil {
		panelEnabled := *s.panelEnabled
		health.PanelEnabled = &panelEnabled
	}
	// initialBrightness is assigned once during construction and only read after,
	// so the read lock already held here is enough.
	if s.initialBrightness != nil {
		brightness := *s.initialBrightness
		health.Brightness = &brightness
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
	s.reportQueueDepth()
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
		s.reportQueueDepth()
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

// applyInitialBrightness pushes the configured brightness to the panel before
// the background animation starts rendering, so the first thing the operator
// sees is already at the brightness they asked for. It runs once per Run: the
// panel keeps the setting until it reboots, and unlike the background there is
// no convergence machinery to re-apply it after a reconnect.
func (s *Scheduler) applyInitialBrightness(ctx context.Context) error {
	if s.initialBrightness == nil {
		return nil
	}
	brightness := *s.initialBrightness
	err := s.retryMatrix(ctx, func() error {
		return s.client.SetBrightness(ctx, brightness)
	})
	if err == nil {
		s.clearBrightnessDirty()
	}
	return err
}

// takeBrightnessDirty reports whether the panel needs the configured brightness
// re-sent, clearing the flag as it does. A failed resend is not re-queued: the
// flag is raised once per reconnect, so a panel that rejects the command costs
// one attempt rather than spinning the scheduler loop.
func (s *Scheduler) takeBrightnessDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := s.brightnessDirty
	s.brightnessDirty = false
	return dirty
}

func (s *Scheduler) clearBrightnessDirty() {
	s.mu.Lock()
	s.brightnessDirty = false
	s.mu.Unlock()
}

// takePanelEnabledDirty reports whether the operator's last panel-visibility
// command needs re-sending, clearing the flag as it does. Like the brightness
// resend it is not re-queued on failure: the flag is raised once per reconnect,
// so a panel that rejects the command costs one attempt rather than spinning the
// scheduler loop.
func (s *Scheduler) takePanelEnabledDirty() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.panelEnabledDirty || s.panelEnabled == nil {
		s.panelEnabledDirty = false
		return false, false
	}
	s.panelEnabledDirty = false
	return *s.panelEnabled, true
}

func (s *Scheduler) applyPanelEnabled(ctx context.Context, enabled bool) error {
	return s.retryMatrix(ctx, func() error {
		return s.client.SetPanelEnabled(ctx, enabled)
	})
}

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
	if err := s.applyInitialBrightness(ctx); err != nil {
		if stoppedByContext(err) {
			s.setState(StateDraining)
			return nil
		}
		return err
	}

	deferBackgroundRestore := false
	for {
		// Visibility is restored before brightness and the background so the
		// frames those send land on an already-blanked panel, rather than
		// flashing the operator's blanked display back on for an instant.
		if enabled, ok := s.takePanelEnabledDirty(); ok {
			if err := s.applyPanelEnabled(ctx, enabled); err != nil && stoppedByContext(err) {
				s.setState(StateDraining)
				return nil
			}
		}
		if s.takeBrightnessDirty() {
			if err := s.applyInitialBrightness(ctx); err != nil && stoppedByContext(err) {
				s.setState(StateDraining)
				return nil
			}
		}
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
			if errors.Is(context.Cause(itemCtx), ErrItemInterrupted) {
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
		if waitErr := s.waitReady(ctx, item.Deadline); waitErr != nil {
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
			s.reportQueueDepth()
		}
		return item, true, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, s.heartbeatInterval)
	defer cancel()

	item, err := s.queue.next(waitCtx)
	if err == nil {
		s.reportQueueDepth()
		return item, true, nil
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return ScheduledItem{}, false, nil
	}
	return ScheduledItem{}, false, err
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

func (s *Scheduler) expired(item PlayItem) bool {
	return !item.Deadline.IsZero() && !s.now().Before(item.Deadline)
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
