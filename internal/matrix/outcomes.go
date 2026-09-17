package matrix

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/worxbend/echo/internal/observability"
)

// Terminal outcome reporting.
//
// Every item that leaves the queue is recorded exactly once, whether it played,
// expired, was interrupted, or was cleared. Observers are best-effort and run
// off the scheduler loop through a bounded dispatcher, so a slow or panicking
// observer cannot stall playback.

type outcomeObserverDispatcher struct {
	observer func(OutcomeReport)
	panics   *observability.CallbackPanics
	reports  chan OutcomeReport
	done     chan struct{}

	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func newOutcomeObserverDispatcher(observer func(OutcomeReport), panics *observability.CallbackPanics) *outcomeObserverDispatcher {
	if observer == nil {
		return nil
	}
	dispatcher := &outcomeObserverDispatcher{
		observer: observer,
		panics:   panics,
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
		d.deliver(report)
	}
}

// deliver runs the observer, counting a panic rather than letting it kill the
// dispatcher goroutine -- which would leave every later report undelivered with
// nothing to show for it. The counter is optional so a dispatcher built without
// one still recovers.
func (d *outcomeObserverDispatcher) deliver(report OutcomeReport) {
	if d.panics == nil {
		defer func() {
			_ = recover()
		}()
		d.observer(report)
		return
	}
	d.panics.Run(observabilityCallbackItemOutcome, func() {
		d.observer(report)
	})
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

// ObservabilityCallbackPanics is the total number of panics recovered from
// best-effort observability callbacks.
func (s *Scheduler) ObservabilityCallbackPanics() uint64 {
	return s.callbackPanics.Total()
}

// ObservabilityCallbackPanicCounts breaks that total down by callback name.
func (s *Scheduler) ObservabilityCallbackPanicCounts() map[string]uint64 {
	return s.callbackPanics.Counts()
}

// reportQueueDepth publishes the queue's current depth to the observer.
//
// It reads the depth itself rather than accepting one from the caller, and
// that is the whole point. Every caller observes a depth under the queue's own
// lock and reports it only after releasing that lock, so the goroutine
// admitting an item and the goroutine popping it can reach the observer in
// either order. When the enqueuer's Set(1) lands after the dequeuer's Set(0),
// the gauge keeps a depth the queue no longer has -- and nothing ever corrects
// it, because a gauge is only written when the depth changes. That is how CI
// caught matrix_proxy_play_queue_depth reporting 1 for an empty queue whose
// only item had already run to completion.
//
// Reading here makes each publication carry the depth as of its own turn, so
// the last one to run reports the most recent state rather than the oldest
// reading. The observer runs with queueDepthMu held, which is what keeps a
// slow observer from being overtaken by a later, faster one; it is a metrics
// write that never re-enters the queue.
func (s *Scheduler) reportQueueDepth() {
	observer := s.onQueueDepthChange
	if observer == nil {
		return
	}
	s.queueDepthMu.Lock()
	defer s.queueDepthMu.Unlock()
	// Registered after the unlock so recover runs first and the mutex is
	// released either way.
	defer s.callbackPanics.RecoverFrom(observabilityCallbackQueueDepthChange)
	observer(s.queue.len())
}

func (s *Scheduler) reportAnimationRendered(animationID string, duration time.Duration) {
	observer := s.onAnimationRendered
	if observer == nil {
		return
	}
	defer s.callbackPanics.RecoverFrom(observabilityCallbackAnimationRendered)
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

func (s *Scheduler) completeQueuedControls(err error) {
	items := s.queue.clear()
	if len(items) > 0 {
		s.reportQueueDepth()
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
