// Package events provides the in-process v1 event bus.
//
// The v1 contract is intentionally simple and blocking: Publish fans out to
// subscribers sequentially in subscription order while holding the bus read
// lock, preserving publish order for each subscriber. Subscriber channels are
// bounded, and the only supported overflow behavior is block. A full subscriber
// therefore backpressures the publisher until that subscriber receives or the
// publish context expires.
//
// Publish errors are not atomic non-delivery signals. Because fan-out is
// sequential, an event can be delivered to earlier subscribers before Publish
// returns an error while blocked behind a later subscriber. Unsubscribe and
// Close take the bus write lock, so they do not release an already blocked
// publisher and can wait for that Publish to finish. They can also wait for
// in-flight OnDepthChange callbacks before emitting the terminal zero-depth
// observation, so depth callbacks are synchronous lifecycle blockers and must
// stay fast. Publisher backpressure metrics remain total-only; subscriber
// attribution requires a fresh event bus design pass.
package events

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrBusClosed       = errors.New("event bus is closed")
	ErrInvalidCapacity = errors.New("event bus capacity must be positive")
)

type Bus struct {
	capacity int

	mu          sync.RWMutex
	closed      bool
	subscribers map[chan Event]*subscriber
	order       []chan Event

	onPublishBackpressureWait    func(time.Duration)
	onPublishBackpressureTimeout func()
}

type SubscriptionOptions struct {
	// OnDepthChange reports this subscriber's local buffered backlog depth after
	// bus state changes. It is subscriber-local instrumentation, may be called
	// synchronously after publish, subscribe, unsubscribe, or close paths, and
	// must be fast. Terminal lifecycle paths wait for any in-flight callback
	// before publishing their terminal zero-depth observation. Calls are
	// best-effort: callback panics are recovered. Once unsubscribe returns, the
	// subscription is inactive and no later bus-owned OnDepthChange calls are
	// made for it. Callers that consume from the channel should record
	// receive-side depth.
	OnDepthChange func(int)
}

type BusOptions struct {
	// OnPublishBackpressureWait reports total time a Publish call spent blocked
	// behind full subscriber channels. It is invoked outside bus locks and
	// callback panics are recovered.
	OnPublishBackpressureWait func(time.Duration)
	// OnPublishBackpressureTimeout reports Publish calls that return because the
	// publish context expired while waiting behind subscriber backpressure. It is
	// invoked outside bus locks and callback panics are recovered.
	OnPublishBackpressureTimeout func()
}

type subscriber struct {
	mu               sync.Mutex
	cond             *sync.Cond
	active           bool
	observing        int
	terminalObserved bool
	onDepthChange    func(int)
}

type depthObservation struct {
	subscriber *subscriber
	depth      int
}

type publishBackpressureObservation struct {
	waited   time.Duration
	timedOut bool
}

func NewBus(capacity int) (*Bus, error) {
	return NewBusWithOptions(capacity, BusOptions{})
}

func NewBusWithOptions(capacity int, options BusOptions) (*Bus, error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}

	return &Bus{
		capacity:                     capacity,
		subscribers:                  make(map[chan Event]*subscriber),
		onPublishBackpressureWait:    options.OnPublishBackpressureWait,
		onPublishBackpressureTimeout: options.OnPublishBackpressureTimeout,
	}, nil
}

func MustNewBus(capacity int) *Bus {
	bus, err := NewBus(capacity)
	if err != nil {
		panic(err)
	}
	return bus
}

func (b *Bus) Subscribe(ctx context.Context) (<-chan Event, func()) {
	return b.SubscribeWithOptions(ctx, SubscriptionOptions{})
}

// SubscribeWithOptions subscribes to published events with optional
// subscriber-local instrumentation. Publish fans out to subscribers in
// subscription order under the bus read lock. The returned unsubscribe function
// removes the subscriber through the bus write lock, so it can wait for any
// Publish currently blocked behind a full subscriber channel.
func (b *Bus) SubscribeWithOptions(ctx context.Context, options SubscriptionOptions) (<-chan Event, func()) {
	ch := make(chan Event, b.capacity)
	sub := newSubscriber(options.OnDepthChange)

	b.mu.Lock()
	if b.closed {
		close(ch)
		b.mu.Unlock()
		observeTerminalDepthChanges([]depthObservation{{subscriber: sub, depth: 0}})
		return ch, func() {}
	}
	b.subscribers[ch] = sub
	b.order = append(b.order, ch)
	b.mu.Unlock()
	observeDepthChanges([]depthObservation{{subscriber: sub, depth: 0}})

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			var observations []depthObservation
			b.mu.Lock()
			if _, ok := b.subscribers[ch]; ok {
				delete(b.subscribers, ch)
				b.removeSubscriberOrderLocked(ch)
				drainAndClose(ch)
				observations = append(observations, depthObservation{subscriber: sub, depth: 0})
			}
			b.mu.Unlock()
			observeTerminalDepthChanges(observations)
		})
	}

	if ctx != nil {
		go func() {
			<-ctx.Done()
			unsubscribe()
		}()
	}

	return ch, unsubscribe
}

// Publish performs sequential blocking fan-out under the bus read lock. While
// the only supported queue.overflow_policy is "block", a full subscriber
// channel backpressures the publisher until that subscriber receives or the
// publish context expires. Because delivery is sequential, Publish can deliver
// an event to earlier subscribers and then return a context error while blocked
// behind a later full subscriber; it does not roll back those earlier
// deliveries.
func (b *Bus) Publish(ctx context.Context, event Event) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	event = cloneEvent(event)

	b.mu.RLock()

	if b.closed {
		b.mu.RUnlock()
		return ErrBusClosed
	}

	var observations []depthObservation
	var backpressure publishBackpressureObservation
	for _, ch := range b.order {
		sub := b.subscribers[ch]
		eventCopy := cloneEvent(event)
		select {
		case ch <- eventCopy:
			observations = append(observations, depthObservation{subscriber: sub, depth: len(ch)})
			continue
		case <-ctx.Done():
			b.mu.RUnlock()
			observeDepthChanges(observations)
			b.observePublishBackpressure(backpressure)
			return ctx.Err()
		default:
		}

		started := time.Now()
		select {
		case ch <- eventCopy:
			backpressure.waited += time.Since(started)
			observations = append(observations, depthObservation{subscriber: sub, depth: len(ch)})
		case <-ctx.Done():
			backpressure.waited += time.Since(started)
			backpressure.timedOut = true
			b.mu.RUnlock()
			observeDepthChanges(observations)
			b.observePublishBackpressure(backpressure)
			return ctx.Err()
		}
	}
	b.mu.RUnlock()
	observeDepthChanges(observations)
	b.observePublishBackpressure(backpressure)

	return nil
}

// Close removes all subscribers through the bus write lock, so it can wait for
// any Publish currently blocked behind a full subscriber channel. Close emits
// terminal zero-depth observations and can wait for in-flight OnDepthChange
// callbacks before returning.
func (b *Bus) Close() error {
	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()
		return ErrBusClosed
	}

	b.closed = true
	var observations []depthObservation
	for _, ch := range b.order {
		sub := b.subscribers[ch]
		delete(b.subscribers, ch)
		drainAndClose(ch)
		observations = append(observations, depthObservation{subscriber: sub, depth: 0})
	}
	b.order = nil
	b.mu.Unlock()
	observeTerminalDepthChanges(observations)

	return nil
}

func (b *Bus) Capacity() int {
	if b == nil {
		return 0
	}
	return b.capacity
}

func cloneEvent(event Event) Event {
	if len(event.Attributes) == 0 {
		return event
	}

	attrs := make(map[string]string, len(event.Attributes))
	for key, value := range event.Attributes {
		attrs[key] = value
	}
	event.Attributes = attrs

	return event
}

func drainAndClose(ch chan Event) {
	for {
		select {
		case <-ch:
		default:
			close(ch)
			return
		}
	}
}

func (b *Bus) removeSubscriberOrderLocked(ch chan Event) {
	for i, candidate := range b.order {
		if candidate == ch {
			copy(b.order[i:], b.order[i+1:])
			b.order[len(b.order)-1] = nil
			b.order = b.order[:len(b.order)-1]
			return
		}
	}
}

func newSubscriber(onDepthChange func(int)) *subscriber {
	sub := &subscriber{
		active:        true,
		onDepthChange: onDepthChange,
	}
	sub.cond = sync.NewCond(&sub.mu)
	return sub
}

func (s *subscriber) observeDepth(depth int) {
	if s.onDepthChange != nil {
		s.onDepthChange(depth)
	}
}

func observeDepthChanges(observations []depthObservation) {
	for _, observation := range observations {
		observation.subscriber.observeDepthSafely(observation.depth)
	}
}

func observeTerminalDepthChanges(observations []depthObservation) {
	for _, observation := range observations {
		observation.subscriber.observeTerminalDepthSafely(observation.depth)
	}
}

func (b *Bus) observePublishBackpressure(observation publishBackpressureObservation) {
	if observation.waited > 0 {
		observeDurationSafely(b.onPublishBackpressureWait, observation.waited)
	}
	if observation.timedOut {
		observeFuncSafely(b.onPublishBackpressureTimeout)
	}
}

func observeDurationSafely(callback func(time.Duration), value time.Duration) {
	if callback == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	callback(value)
}

func observeFuncSafely(callback func()) {
	if callback == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	callback()
}

func (s *subscriber) observeDepthSafely(depth int) {
	if !s.beginDepthObservation() {
		return
	}
	defer s.endDepthObservation()
	defer func() {
		_ = recover()
	}()
	s.observeDepth(depth)
}

func (s *subscriber) observeTerminalDepthSafely(depth int) {
	if !s.beginTerminalDepthObservation() {
		return
	}
	defer func() {
		_ = recover()
	}()
	s.observeDepth(depth)
}

func (s *subscriber) beginDepthObservation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return false
	}
	s.observing++
	return true
}

func (s *subscriber) endDepthObservation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observing--
	if s.observing == 0 {
		s.cond.Broadcast()
	}
}

func (s *subscriber) beginTerminalDepthObservation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalObserved {
		return false
	}
	s.active = false
	s.terminalObserved = true
	for s.observing > 0 {
		s.cond.Wait()
	}
	return true
}
