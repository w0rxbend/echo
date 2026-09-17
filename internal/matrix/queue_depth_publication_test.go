package matrix

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

// TestQueueDepthPublicationNeverEndsOnAStaleDepth pins the ordering bug that
// left matrix_proxy_play_queue_depth reporting 1 for a queue whose only item
// had already run to completion.
//
// Depth reports come from two goroutines that never meet: the caller admitting
// an item and the worker popping it. Each used to observe a depth under the
// queue's lock and publish it after releasing that lock, so the two could reach
// the observer in either order. When admission's "1" landed after the worker's
// "0", the gauge kept a depth the queue no longer had -- and kept it forever,
// because a gauge is only rewritten when the depth next changes.
//
// The race detector cannot see this: both writes go through Prometheus's own
// mutex, so there is no data race, only a losing interleaving. This test forces
// that interleaving instead of waiting for it. The observer stalls on its first
// call, which parks the admission report mid-publication; the drain report then
// races it. Serialised publication makes the outcome the same either way -- the
// report that runs last is the one that read the queue last.
func TestQueueDepthPublicationNeverEndsOnAStaleDepth(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var stalled atomic.Bool
	depths := newQueueDepthRecorder()

	scheduler := newTestScheduler(t, newFakeMatrixClient(), animations.NewRegistry(), SchedulerOptions{
		OnQueueDepthChange: func(depth int) {
			if stalled.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			depths.record(depth)
		},
	})

	ctx := context.Background()
	if _, _, err := scheduler.queue.enqueueScheduled(ctx, ScheduledItem{PlayItem: PlayItem{ID: "only"}}); err != nil {
		t.Fatal(err)
	}

	admission := make(chan struct{})
	go func() {
		defer close(admission)
		scheduler.reportQueueDepth()
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("admission report never reached the queue-depth observer")
	}

	// The item is gone before the admission report has published anything.
	if _, err := scheduler.queue.next(ctx); err != nil {
		t.Fatal(err)
	}
	drain := make(chan struct{})
	go func() {
		defer close(drain)
		scheduler.reportQueueDepth()
	}()

	// The drain report should not be able to finish while the admission report
	// is still inside the observer; if it does, the two are unordered and the
	// stale value is about to land last.
	select {
	case <-drain:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	<-admission
	<-drain

	published := depths.values()
	if len(published) == 0 {
		t.Fatal("no queue depth was published; the test never exercised the reporter")
	}
	last := published[len(published)-1]
	if want := scheduler.queue.len(); last != want {
		t.Fatalf("last published depth = %d, want %d (published %v); a stale depth landing after a newer one leaves the gauge permanently wrong", last, want, published)
	}
}
