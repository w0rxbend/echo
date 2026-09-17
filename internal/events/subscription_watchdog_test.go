package events

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// countSubscriptionWatchdogs reports how many context-watchdog goroutines
// SubscribeWithOptions currently has parked. Naming the frame rather than
// counting goroutines keeps the assertion immune to whatever else the package's
// other tests happen to be running.
func countSubscriptionWatchdogs() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "SubscribeWithOptions.func")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitForSubscriptionWatchdogs(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = countSubscriptionWatchdogs()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("subscription watchdog goroutines = %d, want %d", got, want)
}

// TestUnsubscribeReleasesTheContextWatchdog pins the goroutine leak that let a
// subscription outlive itself.
//
// The watchdog that turns context cancellation into an unsubscribe used to have
// no other exit path, so unsubscribing a subscription whose context was never
// cancelled left it parked forever. Nothing observable went wrong -- the
// subscriber was removed and the channel closed -- which is exactly why it could
// accumulate unnoticed: a caller that subscribed and unsubscribed repeatedly
// under one application-lifetime context leaked a goroutine per subscription.
func TestUnsubscribeReleasesTheContextWatchdog(t *testing.T) {
	baseline := countSubscriptionWatchdogs()

	// A context that stays alive for the whole test: cancellation must not be
	// what releases these goroutines.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := newTestBus(t, 1)
	const subscriptions = 50
	unsubscribes := make([]func(), 0, subscriptions)
	for range subscriptions {
		_, unsubscribe := bus.Subscribe(ctx)
		unsubscribes = append(unsubscribes, unsubscribe)
	}
	waitForSubscriptionWatchdogs(t, baseline+subscriptions)

	for _, unsubscribe := range unsubscribes {
		unsubscribe()
		// A second call must stay a no-op: the watchdog's exit signal is closed
		// inside the same sync.Once as the removal, so closing it twice would
		// panic.
		unsubscribe()
	}

	if err := ctx.Err(); err != nil {
		t.Fatalf("subscription context ended during the test: %v", err)
	}
	waitForSubscriptionWatchdogs(t, baseline)
}

// TestCancellingTheContextReleasesTheWatchdog keeps the original exit path
// working: the watchdog still unsubscribes on cancellation and then exits.
func TestCancellingTheContextReleasesTheWatchdog(t *testing.T) {
	baseline := countSubscriptionWatchdogs()

	bus := newTestBus(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	ch, unsubscribe := bus.Subscribe(ctx)
	defer unsubscribe()
	waitForSubscriptionWatchdogs(t, baseline+1)

	cancel()
	waitForSubscriptionWatchdogs(t, baseline)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("subscriber channel delivered an event after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context never closed the subscriber channel")
	}
}
