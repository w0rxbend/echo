package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSubscriptionDepthTracksPublishAndReceiveBacklogOnlyNoInflightCount(t *testing.T) {
	bus := MustNewBus(3)
	depths := newDepthRecorder(t)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: depths.record,
	})
	defer unsubscribe()

	depths.assertAll(0)

	publishEvent(t, bus, "one")
	depths.assertAll(0, 1)
	publishEvent(t, bus, "two")
	depths.assertAll(0, 1, 2)

	assertEventID(t, receiveAndRecordBacklogDepth(ch, depths.record), "one")
	// Depth is subscriber-channel backlog only; the event being processed is not
	// counted as in flight by the bus or by this receive-side observation.
	depths.assertAll(0, 1, 2, 1)

	assertEventID(t, receiveAndRecordBacklogDepth(ch, depths.record), "two")
	depths.assertAll(0, 1, 2, 1, 0)
}

func TestSubscriptionDepthResetsOnUnsubscribe(t *testing.T) {
	bus := MustNewBus(3)
	depths := newDepthRecorder(t)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: depths.record,
	})

	publishEvent(t, bus, "one")
	publishEvent(t, bus, "two")
	depths.assertAll(0, 1, 2)

	unsubscribe()
	depths.assertAll(0, 1, 2, 0)
	assertSubscriptionClosedAfterDrain(t, ch)
	unsubscribe()
	depths.assertAll(0, 1, 2, 0)

	publishEvent(t, bus, "after-unsubscribe")
	depths.assertAll(0, 1, 2, 0)
}

func TestDepthCallbackDoesNotPublishLateNonzeroAfterUnsubscribeReturns(t *testing.T) {
	bus := MustNewBus(2)
	depths := newDepthRecorder(t)
	nonzeroStarted := make(chan struct{})
	releaseNonzero := make(chan struct{})
	var startedOnce sync.Once
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: func(depth int) {
			if depth > 0 {
				startedOnce.Do(func() {
					close(nonzeroStarted)
				})
				<-releaseNonzero
			}
			depths.record(depth)
		},
	})

	depths.assertAll(0)

	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(context.Background(), Event{ID: "one", Source: SourceExternal, Type: "test"})
	}()

	select {
	case <-nonzeroStarted:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not reach nonzero depth callback")
	}

	unsubscribeDone := make(chan struct{})
	go func() {
		unsubscribe()
		close(unsubscribeDone)
	}()

	select {
	case <-unsubscribeDone:
		t.Fatal("unsubscribe returned while a nonzero depth callback was still in flight")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseNonzero)
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after nonzero callback was released")
	}
	select {
	case <-unsubscribeDone:
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not return after nonzero callback was released")
	}

	depths.assertAll(0, 1, 0)
	assertNoDepthObservationAfter(t, depths, 25*time.Millisecond)
	assertSubscriptionClosed(t, ch)
}

func TestSubscriptionDepthResetsWhenSubscriptionContextCloses(t *testing.T) {
	bus := MustNewBus(3)
	ctx, cancel := context.WithCancel(context.Background())
	depths := newDepthRecorder(t)
	ch, unsubscribe := bus.SubscribeWithOptions(ctx, SubscriptionOptions{
		OnDepthChange: depths.record,
	})
	defer unsubscribe()

	publishEvent(t, bus, "one")
	depths.assertAll(0, 1)

	cancel()
	depths.assertEventuallyAll(0, 1, 0)
	assertSubscriptionClosedAfterDrain(t, ch)
	unsubscribe()
	depths.assertAll(0, 1, 0)
}

func TestSubscriptionDepthResetsOnBusClose(t *testing.T) {
	bus := MustNewBus(3)
	depths := newDepthRecorder(t)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: depths.record,
	})
	defer unsubscribe()

	publishEvent(t, bus, "one")
	publishEvent(t, bus, "two")
	depths.assertAll(0, 1, 2)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	depths.assertAll(0, 1, 2, 0)
	assertSubscriptionClosedAfterDrain(t, ch)
	unsubscribe()
	depths.assertAll(0, 1, 2, 0)
	if err := bus.Publish(context.Background(), Event{ID: "after-close"}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Publish() after close error = %v, want %v", err, ErrBusClosed)
	}
}

func TestDepthCallbackDoesNotPublishLateNonzeroAfterCloseReturns(t *testing.T) {
	bus := MustNewBus(2)
	depths := newDepthRecorder(t)
	nonzeroStarted := make(chan struct{})
	releaseNonzero := make(chan struct{})
	var startedOnce sync.Once
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: func(depth int) {
			if depth > 0 {
				startedOnce.Do(func() {
					close(nonzeroStarted)
				})
				<-releaseNonzero
			}
			depths.record(depth)
		},
	})
	defer unsubscribe()

	depths.assertAll(0)

	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(context.Background(), Event{ID: "one", Source: SourceExternal, Type: "test"})
	}()

	select {
	case <-nonzeroStarted:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not reach nonzero depth callback")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- bus.Close()
	}()

	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned while a nonzero depth callback was still in flight: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseNonzero)
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after nonzero callback was released")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after nonzero callback was released")
	}

	depths.assertAll(0, 1, 0)
	assertNoDepthObservationAfter(t, depths, 25*time.Millisecond)
	assertSubscriptionClosed(t, ch)
}

func TestClosedBusSubscriptionReportsZeroDepth(t *testing.T) {
	bus := MustNewBus(1)
	if err := bus.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	depths := newDepthRecorder(t)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: depths.record,
	})
	defer unsubscribe()

	depths.assertAll(0)
	assertSubscriptionClosed(t, ch)
}

func TestDepthCallbackPanicRecoveredOnPublish(t *testing.T) {
	bus := MustNewBus(1)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: func(int) {
			panic("depth callback failed")
		},
	})
	defer unsubscribe()

	assertDoesNotPanic(t, "Publish", func() {
		publishEvent(t, bus, "one")
	})
	assertEventID(t, receiveEvent(t, ch), "one")
}

func TestDepthCallbackPanicRecoveredOnClose(t *testing.T) {
	bus := MustNewBus(1)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: func(int) {
			panic("depth callback failed")
		},
	})
	defer unsubscribe()

	assertDoesNotPanic(t, "Close", func() {
		if err := bus.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	assertSubscriptionClosed(t, ch)
}

func TestDepthCallbackPanicRecoveredOnUnsubscribe(t *testing.T) {
	bus := MustNewBus(1)
	ch, unsubscribe := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: func(int) {
			panic("depth callback failed")
		},
	})

	assertDoesNotPanic(t, "unsubscribe", unsubscribe)
	assertSubscriptionClosed(t, ch)
}

func TestDepthCallbackPanicRecoveredOnSubscriptionContextCancel(t *testing.T) {
	bus := MustNewBus(1)
	ctx, cancel := context.WithCancel(context.Background())
	ch, unsubscribe := bus.SubscribeWithOptions(ctx, SubscriptionOptions{
		OnDepthChange: func(int) {
			panic("depth callback failed")
		},
	})
	defer unsubscribe()

	cancel()
	assertSubscriptionClosed(t, ch)
}

func TestDepthCallbackPanicRecoveredOnClosedBusSubscribe(t *testing.T) {
	bus := MustNewBus(1)
	if err := bus.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var ch <-chan Event
	var unsubscribe func()
	assertDoesNotPanic(t, "SubscribeWithOptions", func() {
		ch, unsubscribe = bus.SubscribeWithOptions(nil, SubscriptionOptions{
			OnDepthChange: func(int) {
				panic("depth callback failed")
			},
		})
	})
	defer unsubscribe()
	assertSubscriptionClosed(t, ch)
}

func TestDepthCallbackPanicDoesNotStopOtherDepthObservers(t *testing.T) {
	bus := MustNewBus(2)
	panickingCh, unsubscribePanicking := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: func(int) {
			panic("depth callback failed")
		},
	})
	defer unsubscribePanicking()

	depths := newDepthRecorder(t)
	recordingCh, unsubscribeRecording := bus.SubscribeWithOptions(nil, SubscriptionOptions{
		OnDepthChange: depths.record,
	})
	defer unsubscribeRecording()
	depths.assertAll(0)

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, panickingCh), "one")
	assertEventID(t, receiveEvent(t, recordingCh), "one")
	depths.assertAll(0, 1)
}

func TestPublishBlocksUntilFullSubscriberReceivesEvent(t *testing.T) {
	bus := MustNewBus(1)
	ch, unsubscribe := bus.Subscribe(nil)
	defer unsubscribe()

	publishEvent(t, bus, "one")

	published := make(chan error, 1)
	go func() {
		published <- bus.Publish(context.Background(), Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	assertPublishStillBlocked(t, published)
	assertEventID(t, receiveEvent(t, ch), "one")

	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("blocked Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() remained blocked after subscriber received an event")
	}
	assertEventID(t, receiveEvent(t, ch), "two")
}

func TestPublishRecordsBackpressureWaitWhenFullSubscriberReceivesEvent(t *testing.T) {
	backpressure := newBackpressureRecorder(t)
	bus, err := NewBusWithOptions(1, BusOptions{
		OnPublishBackpressureWait:    backpressure.recordWait,
		OnPublishBackpressureTimeout: backpressure.recordTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := bus.Subscribe(nil)
	defer unsubscribe()

	publishEvent(t, bus, "one")

	published := make(chan error, 1)
	go func() {
		published <- bus.Publish(context.Background(), Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	assertPublishStillBlocked(t, published)
	assertEventID(t, receiveEvent(t, ch), "one")

	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("blocked Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() remained blocked after subscriber received an event")
	}
	assertEventID(t, receiveEvent(t, ch), "two")
	backpressure.assertWaitCount(1)
	backpressure.assertTimeouts(0)
}

func TestPublishReturnsContextErrorWhenBlockedBehindFullSubscriber(t *testing.T) {
	bus := MustNewBus(1)
	ch, unsubscribe := bus.Subscribe(nil)
	defer unsubscribe()

	publishEvent(t, bus, "one")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Publish(ctx, Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Publish() error = %v, want %v", err, context.DeadlineExceeded)
	}

	assertEventID(t, receiveEvent(t, ch), "one")
	assertNoEvent(t, ch)
}

func TestUnsubscribeWaitsForBlockedPublishContextCancellation(t *testing.T) {
	bus := MustNewBus(1)
	earlierCh, unsubscribeEarlier := bus.Subscribe(nil)
	defer unsubscribeEarlier()
	laterCh, unsubscribeLater := bus.Subscribe(nil)

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, earlierCh), "one")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(ctx, Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	assertEventID(t, receiveEvent(t, earlierCh), "two")

	unsubscribeDone := make(chan struct{})
	go func() {
		unsubscribeLater()
		close(unsubscribeDone)
	}()

	assertUnsubscribeStillBlocked(t, unsubscribeDone)

	cancel()
	select {
	case err := <-publishDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context cancellation")
	}
	select {
	case <-unsubscribeDone:
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not return after blocked Publish() was canceled")
	}
	assertSubscriptionClosedAfterDrain(t, laterCh)
}

func TestUnsubscribeWaitsForBlockedPublishUntilFullSubscriberReceives(t *testing.T) {
	bus := MustNewBus(1)
	earlierCh, unsubscribeEarlier := bus.Subscribe(nil)
	defer unsubscribeEarlier()
	laterCh, unsubscribeLater := bus.Subscribe(nil)

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, earlierCh), "one")

	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(context.Background(), Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	// Seeing the event on the earlier subscriber proves Publish has advanced to
	// the later subscriber, whose channel is still full with "one".
	assertEventID(t, receiveEvent(t, earlierCh), "two")

	unsubscribeDone := make(chan struct{})
	go func() {
		unsubscribeLater()
		close(unsubscribeDone)
	}()

	assertUnsubscribeStillBlocked(t, unsubscribeDone)

	// Unsubscribe is not a publisher release mechanism under the v1 contract.
	// Only subscriber progress or the publish context can release this Publish.
	assertEventID(t, receiveEvent(t, laterCh), "one")
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("blocked Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() remained blocked after subscriber received an event")
	}
	select {
	case <-unsubscribeDone:
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not return after blocked Publish() completed")
	}
	assertSubscriptionClosedAfterDrain(t, laterCh)
}

func TestCloseWaitsForBlockedPublishUntilFullSubscriberReceives(t *testing.T) {
	bus := MustNewBus(1)
	earlierCh, _ := bus.Subscribe(nil)
	laterCh, _ := bus.Subscribe(nil)

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, earlierCh), "one")

	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(context.Background(), Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	// Seeing the event on the earlier subscriber proves Publish has advanced to
	// the later subscriber, whose channel is still full with "one".
	assertEventID(t, receiveEvent(t, earlierCh), "two")

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- bus.Close()
	}()

	assertCloseStillBlocked(t, closeDone)

	// Close is not a publisher release mechanism under the v1 contract. Only
	// subscriber progress or the publish context can release this Publish.
	assertEventID(t, receiveEvent(t, laterCh), "one")

	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("blocked Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() remained blocked after subscriber received an event")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after blocked Publish() completed")
	}
	assertSubscriptionClosedAfterDrain(t, earlierCh)
	assertSubscriptionClosedAfterDrain(t, laterCh)
}

func TestCloseWaitsForBlockedPublishContextCancellation(t *testing.T) {
	bus := MustNewBus(1)
	earlierCh, _ := bus.Subscribe(nil)
	laterCh, _ := bus.Subscribe(nil)

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, earlierCh), "one")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(ctx, Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	// Seeing the event on the earlier subscriber proves Publish has advanced to
	// the later subscriber, whose channel is still full with "one".
	assertEventID(t, receiveEvent(t, earlierCh), "two")

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- bus.Close()
	}()

	assertCloseStillBlocked(t, closeDone)

	// Close is not a publisher release mechanism under the v1 contract. Only
	// subscriber progress or the publish context can release this Publish.
	cancel()

	select {
	case err := <-publishDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context cancellation")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after blocked Publish() was canceled")
	}
	assertSubscriptionClosedAfterDrain(t, earlierCh)
	assertSubscriptionClosedAfterDrain(t, laterCh)
}

func TestCloseWaitsForBlockedPublishContextDeadline(t *testing.T) {
	bus := MustNewBus(1)
	earlierCh, _ := bus.Subscribe(nil)
	laterCh, _ := bus.Subscribe(nil)

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, earlierCh), "one")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(ctx, Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	// Seeing the event on the earlier subscriber proves Publish has advanced to
	// the later subscriber, whose channel is still full with "one".
	assertEventID(t, receiveEvent(t, earlierCh), "two")

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- bus.Close()
	}()

	assertCloseStillBlocked(t, closeDone)

	select {
	case err := <-publishDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Publish() error = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context deadline")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after blocked Publish() deadline expired")
	}
	assertSubscriptionClosedAfterDrain(t, earlierCh)
	assertSubscriptionClosedAfterDrain(t, laterCh)
}

func TestPublishCanPartiallyDeliverBeforeContextErrorBehindLaterFullSubscriber(t *testing.T) {
	bus := MustNewBus(1)
	earlierCh, unsubscribeEarlier := bus.Subscribe(nil)
	defer unsubscribeEarlier()
	laterCh, unsubscribeLater := bus.Subscribe(nil)
	defer unsubscribeLater()

	publishEvent(t, bus, "one")
	assertEventID(t, receiveEvent(t, earlierCh), "one")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Publish(ctx, Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	assertEventID(t, receiveEvent(t, earlierCh), "two")
	cancel()

	var publishErr error
	select {
	case publishErr = <-errCh:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context cancellation")
	}
	if !errors.Is(publishErr, context.Canceled) {
		t.Fatalf("Publish() error = %v, want %v", publishErr, context.Canceled)
	}

	assertEventID(t, receiveEvent(t, laterCh), "one")
	assertNoEvent(t, laterCh)
}

func TestPublishRecordsBackpressureTimeoutWhenContextExpiresBehindFullSubscriber(t *testing.T) {
	backpressure := newBackpressureRecorder(t)
	bus, err := NewBusWithOptions(1, BusOptions{
		OnPublishBackpressureWait:    backpressure.recordWait,
		OnPublishBackpressureTimeout: backpressure.recordTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := bus.Subscribe(nil)
	defer unsubscribe()

	publishEvent(t, bus, "one")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Publish(ctx, Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()

	var publishErr error
	select {
	case publishErr = <-errCh:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context timeout")
	}
	if !errors.Is(publishErr, context.DeadlineExceeded) {
		t.Fatalf("Publish() error = %v, want %v", publishErr, context.DeadlineExceeded)
	}

	backpressure.assertWaitCount(1)
	backpressure.assertTimeouts(1)
	assertEventID(t, receiveEvent(t, ch), "one")
	assertNoEvent(t, ch)
}

func TestPublishBackpressureCallbackPanicsRecovered(t *testing.T) {
	bus, err := NewBusWithOptions(1, BusOptions{
		OnPublishBackpressureWait: func(time.Duration) {
			panic("backpressure wait callback failed")
		},
		OnPublishBackpressureTimeout: func() {
			panic("backpressure timeout callback failed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := bus.Subscribe(nil)
	defer unsubscribe()

	publishEvent(t, bus, "one")
	published := make(chan error, 1)
	go func() {
		published <- bus.Publish(context.Background(), Event{ID: "two", Source: SourceExternal, Type: "test"})
	}()
	assertPublishStillBlocked(t, published)
	assertEventID(t, receiveEvent(t, ch), "one")
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("blocked Publish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() remained blocked after subscriber received an event")
	}
	assertEventID(t, receiveEvent(t, ch), "two")

	publishEvent(t, bus, "three")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Publish(ctx, Event{ID: "four", Source: SourceExternal, Type: "test"})
	}()
	select {
	case publishErr := <-errCh:
		if !errors.Is(publishErr, context.DeadlineExceeded) {
			t.Fatalf("Publish() error = %v, want %v", publishErr, context.DeadlineExceeded)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after context timeout")
	}
}

type depthRecorder struct {
	t      *testing.T
	mu     sync.Mutex
	depths []int
}

func newDepthRecorder(t *testing.T) *depthRecorder {
	t.Helper()
	return &depthRecorder{t: t}
}

type backpressureRecorder struct {
	t        *testing.T
	mu       sync.Mutex
	waits    []time.Duration
	timeouts int
}

func newBackpressureRecorder(t *testing.T) *backpressureRecorder {
	t.Helper()
	return &backpressureRecorder{t: t}
}

func (r *backpressureRecorder) recordWait(wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, wait)
}

func (r *backpressureRecorder) recordTimeout() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timeouts++
}

func (r *backpressureRecorder) assertWaitCount(want int) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.waits) != want {
		r.t.Fatalf("backpressure wait observations = %v, want %d observations", r.waits, want)
	}
	for _, wait := range r.waits {
		if wait <= 0 {
			r.t.Fatalf("backpressure wait = %s, want positive duration", wait)
		}
	}
}

func (r *backpressureRecorder) assertTimeouts(want int) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timeouts != want {
		r.t.Fatalf("backpressure timeouts = %d, want %d", r.timeouts, want)
	}
}

func (r *depthRecorder) record(depth int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.depths = append(r.depths, depth)
}

func (r *depthRecorder) assertAll(want ...int) {
	r.t.Helper()
	got := r.snapshot()
	if len(got) != len(want) {
		r.t.Fatalf("depth observations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			r.t.Fatalf("depth observations = %v, want %v", got, want)
		}
	}
}

func (r *depthRecorder) assertEventuallyAll(want ...int) {
	r.t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got := r.snapshot()
		if equalInts(got, want) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("depth observations = %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *depthRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.depths...)
}

func assertNoDepthObservationAfter(t *testing.T, depths *depthRecorder, wait time.Duration) {
	t.Helper()
	before := depths.snapshot()
	time.Sleep(wait)
	after := depths.snapshot()
	if !equalInts(after, before) {
		t.Fatalf("depth observations changed after terminal cleanup: got %v, before %v", after, before)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func publishEvent(t *testing.T, bus *Bus, id string) {
	t.Helper()
	if err := bus.Publish(context.Background(), Event{ID: id, Source: SourceExternal, Type: "test"}); err != nil {
		t.Fatalf("Publish(%q) error = %v", id, err)
	}
}

func receiveAndRecordBacklogDepth(ch <-chan Event, record func(int)) Event {
	event := <-ch
	record(len(ch))
	return event
}

func receiveEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("subscription channel closed before event")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func assertPublishStillBlocked(t *testing.T, published <-chan error) {
	t.Helper()
	select {
	case err := <-published:
		t.Fatalf("Publish() returned before subscriber received from full channel: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
}

func assertUnsubscribeStillBlocked(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("unsubscribe returned while Publish() was blocked behind a full subscriber")
	case <-time.After(25 * time.Millisecond):
	}
}

func assertCloseStillBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Close() returned while Publish() was blocked behind a full subscriber: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
}

func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-ch:
		if ok {
			t.Fatalf("subscription received event %q, want no event", event.ID)
		}
		t.Fatal("subscription channel closed unexpectedly")
	default:
	}
}

func assertEventID(t *testing.T, event Event, want string) {
	t.Helper()
	if event.ID != want {
		t.Fatalf("received event ID = %q, want %q", event.ID, want)
	}
}

func assertDoesNotPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v", name, recovered)
		}
	}()
	fn()
}

func assertSubscriptionClosedAfterDrain(t *testing.T, ch <-chan Event) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("subscription channel did not close")
		}
	}
}

func assertSubscriptionClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-ch:
		if ok {
			t.Fatalf("subscription received event %q, want closed channel", event.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription channel did not close")
	}
}
