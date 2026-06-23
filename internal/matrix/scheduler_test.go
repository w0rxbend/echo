package matrix

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/metrics"
)

func TestSchedulerSerialPlaybackTiming(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "first", testAnimation(2, 25*time.Millisecond, 10))
	mustRegisterTestAnimation(t, registry, "second", testAnimation(1, 10*time.Millisecond, 20))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	start := time.Now()
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "first",
		AnimationID:   "first",
		MaxDuration:   50 * time.Millisecond,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "second",
		AnimationID:   "second",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 3)
	commands := client.commands()
	if len(commands) < 3 {
		t.Fatalf("commands = %v, want at least 3 frame commands", commands)
	}
	if commands[0].kind != "frame:10" || commands[1].kind != "frame:11" || commands[2].kind != "frame:20" {
		t.Fatalf("command order = %v, want first animation before second", commandKinds(commands))
	}
	if commands[2].at.Sub(start) < 45*time.Millisecond {
		t.Fatalf("second animation started after %s, want first item to block about 50ms", commands[2].at.Sub(start))
	}
}

func TestSchedulerPriorityOrdersBeforeStartAndFIFOWithinPriority(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "low-1", testAnimation(1, time.Millisecond, 1))
	mustRegisterTestAnimation(t, registry, "high", testAnimation(1, time.Millisecond, 9))
	mustRegisterTestAnimation(t, registry, "low-2", testAnimation(1, time.Millisecond, 2))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, request := range []animations.AnimationRequest{
		{ID: "low-1", AnimationID: "low-1", Priority: 10, RestorePolicy: animations.RestoreLeave},
		{ID: "high", AnimationID: "high", Priority: 50, RestorePolicy: animations.RestoreLeave},
		{ID: "low-2", AnimationID: "low-2", Priority: 10, RestorePolicy: animations.RestoreLeave},
	} {
		if err := scheduler.EnqueueRequest(ctx, request); err != nil {
			t.Fatal(err)
		}
	}

	runScheduler(t, ctx, scheduler)
	client.waitFrames(t, 3)

	got := commandKinds(client.commands())
	want := []string{"frame:9", "frame:1", "frame:2"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
}

func TestSchedulerRestoresBackgroundPresetAfterNotification(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 30))
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreBackground,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 4)
	got := commandKinds(client.commands())
	want := []string{"preset:12", "frame:30", "frame:31", "preset:12"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
}

func TestSchedulerLeaveRestoreConvergesToFirmwarePresetBackgroundWhenIdle(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 30))
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	depths := newQueueDepthRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		OnQueueDepthChange: depths.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 4)
	got := commandKinds(client.commands())
	want := []string{"preset:12", "frame:30", "frame:31", "preset:12"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background convergence must stay out of ordinary queue", got)
	}
	if got := depths.values(); !reflect.DeepEqual(got, []int{1, 0}) {
		t.Fatalf("queue depth changes = %v, want only ordinary playback admission/removal", got)
	}
}

func TestSchedulerPreviousFrameRestoreConvergesToFirmwarePresetBackgroundWithoutIdleDuplicate(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 30))
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	depths := newQueueDepthRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		OnQueueDepthChange: depths.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 4)
	time.Sleep(50 * time.Millisecond)
	got := commandKinds(client.commands())
	want := []string{"preset:12", "frame:30", "frame:31", "preset:12"}
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background convergence must stay out of ordinary queue", got)
	}
	if got := depths.values(); !reflect.DeepEqual(got, []int{1, 0}) {
		t.Fatalf("queue depth changes = %v, want only ordinary playback admission/removal", got)
	}
	health := scheduler.Health()
	if health.BackgroundConvergenceState != BackgroundConvergenceConverged || health.BackgroundDirty || !health.BackgroundConverged {
		t.Fatalf("background health = %+v, want clean converged after exact previous-frame restore", health)
	}
}

func TestSchedulerPreviousFrameRestoreConvergesToRenderableBackgroundWithoutIdleDuplicate(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "background", testAnimation(1, time.Millisecond, 70))
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 80))

	depths := newQueueDepthRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "background",
		},
		OnQueueDepthChange: depths.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})
	initialState := scheduler.snapshotDisplayState()
	if initialState.Kind != displayStateFrame || initialState.BackgroundID != "background" {
		t.Fatalf("initial display state = %+v, want scheduler-owned renderable background identity", initialState)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 4)
	time.Sleep(50 * time.Millisecond)
	got := commandKinds(client.commands())
	want := []string{"frame:70", "frame:80", "frame:81", "frame:70"}
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background convergence must stay out of ordinary queue", got)
	}
	if got := depths.values(); !reflect.DeepEqual(got, []int{1, 0}) {
		t.Fatalf("queue depth changes = %v, want only ordinary playback admission/removal", got)
	}
	health := scheduler.Health()
	if health.BackgroundConvergenceState != BackgroundConvergenceConverged || health.BackgroundDirty || !health.BackgroundConverged {
		t.Fatalf("background health = %+v, want clean converged after exact previous-frame restore", health)
	}
	restoredState := scheduler.snapshotDisplayState()
	if restoredState.Kind != displayStateFrame || restoredState.BackgroundID != "background" {
		t.Fatalf("restored display state = %+v, want previous frame to retain renderable background identity", restoredState)
	}
}

func TestSchedulerPreviousFrameRestoreDoesNotConvergeRenderableBackgroundFromVisuallyIdenticalNonBackgroundFrame(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "background", testAnimation(1, time.Millisecond, 70))
	mustRegisterTestAnimation(t, registry, "lookalike", testAnimation(1, time.Millisecond, 70))
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 80))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "background",
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})
	if state := scheduler.snapshotDisplayState(); state.Kind != displayStateFrame || state.BackgroundID != "background" {
		t.Fatalf("initial display state = %+v, want scheduler-owned renderable background identity", state)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "lookalike",
		AnimationID:   "lookalike",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFor(t, func() bool {
		if len(client.commandsLog) < 4 {
			return false
		}
		for _, command := range client.commandsLog {
			if command.kind == "frame:80" {
				return true
			}
		}
		return false
	})

	got := commandKinds(client.commands())
	if len(got) == 0 {
		t.Fatalf("commands = %v, want at least one command", got)
	}
	if countFrames(client.commands()) == 0 {
		t.Fatalf("commands = %v, want at least one renderable frame command", got)
	}
	notifyIdx := -1
	for i, command := range got {
		if command == "frame:80" {
			notifyIdx = i
			break
		}
	}
	if notifyIdx < 2 {
		t.Fatalf("commands = %v, want sequence with background render, lookalike render, then notify render", got)
	}
	if got[0] != "frame:70" {
		t.Fatalf("commands = %v, want first command to render configured background frame", got)
	}

	beforeNotify := got[:notifyIdx]
	restoresAfterNotify := 0
	restoresBeforeNotify := 0
	for i := notifyIdx + 1; i < len(got); i++ {
		if got[i] == "frame:70" {
			restoresAfterNotify++
		}
	}
	for _, command := range beforeNotify {
		if command == "frame:70" {
			restoresBeforeNotify++
		}
	}
	if restoresBeforeNotify >= 3 && restoresAfterNotify > 0 {
		t.Fatalf("commands = %v, want no duplicate restore after notify when display identity already matched background identity", got)
	}
	if restoresBeforeNotify <= 2 && restoresAfterNotify < 2 {
		t.Fatalf("commands = %v, want at least two post-notify restores when lookalike has no background identity", got)
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background convergence must stay out of ordinary queue", got)
	}
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})
	health := scheduler.Health()
	if health.BackgroundConvergenceState != BackgroundConvergenceConverged || health.BackgroundDirty || !health.BackgroundConverged {
		t.Fatalf("background health = %+v, want clean converged only after idle background restore", health)
	}
	finalState := scheduler.snapshotDisplayState()
	if finalState.Kind != displayStateFrame || finalState.BackgroundID != "background" {
		t.Fatalf("final display state = %+v, want idle restore to reestablish renderable background identity", finalState)
	}
}

func TestSchedulerFailedPreviousFrameRestoreKeepsBackgroundDirty(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 90))
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
	})
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(runCtx)
	}()

	client.waitCommands(t, 1)
	client.failCommand("preset:12", &StatusError{Status: StatusUnknownCommand})
	if err := scheduler.EnqueueRequest(runCtx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}

	waitClientAttempts(t, client, "preset:12", 2)
	select {
	case err := <-done:
		if !sameStatusError(err, &StatusError{Status: StatusUnknownCommand}) {
			t.Fatalf("scheduler Run() error = %v, want status error from failed previous-frame restore", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after permanent previous-frame restore failure")
	}
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundDirty && !health.BackgroundConverged && health.BackgroundConvergenceState != BackgroundConvergenceConverged
	})
	if got := commandKinds(client.commands()); !reflect.DeepEqual(got, []string{"preset:12", "frame:90", "frame:91"}) {
		t.Fatalf("commands = %v, want failed previous-frame restore not recorded as successful preset", got)
	}
}

func TestSchedulerAppliesFirmwarePresetBackgroundOnceAtStartup(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
		OnItemOutcome:     outcomes.record,
	})
	initialHealth := scheduler.Health()
	if initialHealth.BackgroundID != "rain" ||
		initialHealth.BackgroundKind != BackgroundKindFirmwarePreset ||
		initialHealth.BackgroundConvergenceState != BackgroundConvergenceUnknown ||
		initialHealth.BackgroundDirty ||
		initialHealth.BackgroundConverged {
		t.Fatalf("initial background health = %+v, want configured unknown and not dirty/converged", initialHealth)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundID == "rain" &&
			health.BackgroundKind == BackgroundKindFirmwarePreset &&
			health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreAttempt != nil &&
			health.BackgroundLastRestoreSuccess != nil &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone
	})
	time.Sleep(50 * time.Millisecond)
	got := commandKinds(client.commands())
	want := []string{"preset:12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background must not be ordinary playback", got)
	}
	time.Sleep(25 * time.Millisecond)
	if got := outcomes.reports(); len(got) != 0 {
		t.Fatalf("background outcomes = %+v, want none", got)
	}
}

func TestSchedulerAppliesRenderableBackgroundAtStartup(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "background", testAnimation(2, time.Millisecond, 41))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "background",
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitFrames(t, 2)
	got := commandKinds(client.commands())
	want := []string{"frame:41", "frame:42"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
}

func TestSchedulerRetriesRenderableBackgroundAfterStartupRenderFailure(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	background := newControlledTestAnimation(errors.New("render failed"), testFrame(41, time.Millisecond), testFrame(42, time.Millisecond))
	background.failNext(1)
	mustRegisterTestAnimation(t, registry, "background", background)

	depths := newQueueDepthRecorder()
	start := time.Date(2026, 6, 23, 8, 0, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "background",
		},
		HeartbeatInterval:  25 * time.Millisecond,
		Now:                clock.Now,
		OnQueueDepthChange: depths.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	background.waitAttempts(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundID == "background" &&
			health.BackgroundKind == BackgroundKindRenderable &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundLastRestoreAttempt != nil &&
			health.BackgroundLastRestoreSuccess == nil &&
			strings.Contains(health.BackgroundLastRestoreError, "render failed") &&
			health.BackgroundLastRestoreErrorClass == ErrorKindRetryable &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1 &&
			health.LastFailure != nil
	})
	time.Sleep(75 * time.Millisecond)
	if got := background.attemptCount(); got != 1 {
		t.Fatalf("render attempts before retry deadline = %d, want 1", got)
	}
	clock.Advance(backgroundRetryableMinDelay)
	background.waitAttempts(t, 2)
	client.waitFrames(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreSuccess != nil &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})

	got := commandKinds(client.commands())
	want := []string{"frame:41", "frame:42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background restore must stay scheduler-owned", got)
	}
	if got := depths.values(); len(got) != 0 {
		t.Fatalf("queue depth changes = %v, want none for background restore", got)
	}
}

func TestSchedulerRetryableFirmwarePresetBackgroundSetPresetFailureRecordsDirtyAndRecovers(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	reconnectAttempts := make(chan ReconnectAttempt, 4)
	reconnectRecoveries := make(chan ReconnectRecovery, 4)
	var failBackgroundReconnectProbe sync.Once
	start := time.Date(2026, 6, 23, 8, 10, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			reconnectAttempts <- attempt
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			reconnectRecoveries <- recovery
		},
		OnMatrixConnectedChange: func(connected bool) {
			if connected {
				failBackgroundReconnectProbe.Do(func() {
					client.failCommand("ping", net.ErrClosed)
				})
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundID == "rain" &&
			health.BackgroundKind == BackgroundKindFirmwarePreset &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundLastRestoreAttempt != nil &&
			health.BackgroundLastRestoreSuccess == nil &&
			strings.Contains(health.BackgroundLastRestoreError, net.ErrClosed.Error()) &&
			health.BackgroundLastRestoreErrorClass == ErrorKindRetryable &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1 &&
			health.LastFailure != nil
	})

	attempt := waitReconnectAttempt(t, reconnectAttempts)
	if attempt.Source != ReconnectSourceSchedulerBackoff ||
		attempt.Attempt != 1 ||
		attempt.ErrorKind != ErrorKindRetryable ||
		!strings.Contains(attempt.Error, net.ErrClosed.Error()) {
		t.Fatalf("reconnect attempt = %+v, want scheduler retryable preset failure attempt", attempt)
	}
	recovery := waitReconnectRecovery(t, reconnectRecoveries)
	if recovery.Source != ReconnectSourceSchedulerBackoff ||
		recovery.Attempt != 1 ||
		recovery.State != StateReady {
		t.Fatalf("reconnect recovery = %+v, want scheduler ready recovery after preset failure", recovery)
	}

	time.Sleep(150 * time.Millisecond)
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts before retry deadline = %d, want 1", attempts)
	}
	clock.Advance(backgroundRetryableMinDelay)
	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreSuccess != nil &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	if attempts := client.attempts("preset:12"); attempts != 2 {
		t.Fatalf("preset attempts = %d, want failed attempt plus retry", attempts)
	}
	if got := commandKinds(client.commands()); !reflect.DeepEqual(got, []string{"preset:12"}) {
		t.Fatalf("commands = %v, want one successful preset restore", got)
	}
}

func TestSchedulerRetryableRenderableBackgroundSetFullFrameFailureRecordsDirtyAndRecovers(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("frame:41", net.ErrClosed)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "background", testAnimation(2, time.Millisecond, 41))

	reconnectAttempts := make(chan ReconnectAttempt, 4)
	reconnectRecoveries := make(chan ReconnectRecovery, 4)
	var failBackgroundReconnectProbe sync.Once
	start := time.Date(2026, 6, 23, 8, 20, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "background",
		},
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			reconnectAttempts <- attempt
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			reconnectRecoveries <- recovery
		},
		OnMatrixConnectedChange: func(connected bool) {
			if connected {
				failBackgroundReconnectProbe.Do(func() {
					client.failCommand("ping", net.ErrClosed)
				})
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundID == "background" &&
			health.BackgroundKind == BackgroundKindRenderable &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundLastRestoreAttempt != nil &&
			health.BackgroundLastRestoreSuccess == nil &&
			strings.Contains(health.BackgroundLastRestoreError, net.ErrClosed.Error()) &&
			health.BackgroundLastRestoreErrorClass == ErrorKindRetryable &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1 &&
			health.LastFailure != nil
	})

	attempt := waitReconnectAttempt(t, reconnectAttempts)
	if attempt.Source != ReconnectSourceSchedulerBackoff ||
		attempt.Attempt != 1 ||
		attempt.ErrorKind != ErrorKindRetryable ||
		!strings.Contains(attempt.Error, net.ErrClosed.Error()) {
		t.Fatalf("reconnect attempt = %+v, want scheduler retryable frame failure attempt", attempt)
	}
	recovery := waitReconnectRecovery(t, reconnectRecoveries)
	if recovery.Source != ReconnectSourceSchedulerBackoff ||
		recovery.Attempt != 1 ||
		recovery.State != StateReady {
		t.Fatalf("reconnect recovery = %+v, want scheduler ready recovery after frame failure", recovery)
	}

	time.Sleep(150 * time.Millisecond)
	if attempts := client.attempts("frame:41"); attempts != 1 {
		t.Fatalf("frame:41 attempts before retry deadline = %d, want 1", attempts)
	}
	clock.Advance(backgroundRetryableMinDelay)
	client.waitFrames(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreSuccess != nil &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	if attempts := client.attempts("frame:41"); attempts != 2 {
		t.Fatalf("frame:41 attempts = %d, want failed attempt plus retry", attempts)
	}
	got := commandKinds(client.commands())
	want := []string{"frame:41", "frame:42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestSchedulerBackgroundRetryBackoffSuppressesHeartbeatAndRestorePolicyAttempts(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 61))
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 30, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundLastRestoreErrorClass == ErrorKindRetryable &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1
	})
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts after first failure = %d, want 1", attempts)
	}

	time.Sleep(40 * time.Millisecond)
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts after heartbeat opportunities before retry deadline = %d, want 1", attempts)
	}

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreBackground,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)
	time.Sleep(40 * time.Millisecond)
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts after forced playback restore before retry deadline = %d, want 1", attempts)
	}
	health := scheduler.Health()
	if !health.BackgroundDirty || health.BackgroundConverged ||
		health.BackgroundConvergenceState != BackgroundConvergenceRetrying ||
		health.BackgroundNextRetry == nil ||
		!health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) ||
		health.BackgroundRetryFailureCount != 1 {
		t.Fatalf("background health before retry deadline = %+v, want pending retry state", health)
	}

	clock.Advance(backgroundRetryableMinDelay)
	client.waitCommands(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	if attempts := client.attempts("preset:12"); attempts != 2 {
		t.Fatalf("preset attempts after retry deadline = %d, want failed attempt plus retry", attempts)
	}

	time.Sleep(40 * time.Millisecond)
	if attempts := client.attempts("preset:12"); attempts != 2 {
		t.Fatalf("preset attempts after clean success = %d, want retry state reset and no clean resend", attempts)
	}
}

func TestSchedulerDueBackgroundRetryReportsFailedWhileLongPlaybackActive(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "hold", testAnimation(1, 500*time.Millisecond, 71))
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 31, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	nextRetry := start.Add(backgroundRetryableMinDelay)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(nextRetry) &&
			health.BackgroundRetryFailureCount == 1
	})
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "hold",
		AnimationID:   "hold",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)
	clock.Advance(backgroundRetryableMinDelay)

	health := scheduler.Health()
	if !health.BackgroundDirty ||
		health.BackgroundConverged ||
		health.BackgroundConvergenceState != BackgroundConvergenceFailed ||
		health.BackgroundNextRetry == nil ||
		!health.BackgroundNextRetry.Equal(nextRetry) ||
		health.BackgroundRetryFailureCount != 1 ||
		health.BackgroundLastRestoreSuccess != nil {
		t.Fatalf("background health during due retry playback = %+v, want dirty failed with retained retry state", health)
	}
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts while playback blocks due retry = %d, want 1", attempts)
	}
}

func TestSchedulerDueBackgroundRetryReportsFailedWhileDisconnected(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 32, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	nextRetry := start.Add(backgroundRetryableMinDelay)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(nextRetry) &&
			health.BackgroundRetryFailureCount == 1
	})
	client.setDisconnected(true)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return !health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(nextRetry) &&
			health.BackgroundRetryFailureCount == 1
	})
	clock.Advance(backgroundRetryableMinDelay)

	health := scheduler.Health()
	if health.MatrixConnected ||
		!health.BackgroundDirty ||
		health.BackgroundConverged ||
		health.BackgroundConvergenceState != BackgroundConvergenceFailed ||
		health.BackgroundNextRetry == nil ||
		!health.BackgroundNextRetry.Equal(nextRetry) ||
		health.BackgroundRetryFailureCount != 1 ||
		health.BackgroundLastRestoreSuccess != nil {
		t.Fatalf("background health during due retry disconnect = %+v, want disconnected dirty failed with retained retry state", health)
	}
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts while disconnected due retry = %d, want 1", attempts)
	}
}

func TestSchedulerDueBackgroundRetryReportsFailedBeforeLoopAttemptsRestore(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 33, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: time.Hour,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	nextRetry := start.Add(backgroundRetryableMinDelay)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(nextRetry) &&
			health.BackgroundRetryFailureCount == 1
	})
	clock.Advance(backgroundRetryableMinDelay)

	health := scheduler.Health()
	if !health.BackgroundDirty ||
		health.BackgroundConverged ||
		health.BackgroundConvergenceState != BackgroundConvergenceFailed ||
		health.BackgroundNextRetry == nil ||
		!health.BackgroundNextRetry.Equal(nextRetry) ||
		health.BackgroundRetryFailureCount != 1 ||
		health.BackgroundLastRestoreSuccess != nil {
		t.Fatalf("background health immediately after retry deadline = %+v, want dirty failed with retained retry state", health)
	}
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts immediately after retry deadline = %d, want 1 before loop observes deadline", attempts)
	}
}

func TestProjectBackgroundConvergenceDueRetryEdges(t *testing.T) {
	nextRetry := time.Date(2026, 6, 23, 8, 34, 0, 0, time.UTC)
	beforeRetry := nextRetry.Add(-time.Nanosecond)
	atRetry := nextRetry
	afterRetry := nextRetry.Add(time.Nanosecond)

	for _, tc := range []struct {
		name      string
		input     BackgroundConvergenceProjectionInput
		now       time.Time
		wantState BackgroundConvergenceState
		wantDirty bool
	}{
		{
			name: "dirty failed restore remains retrying before retry deadline",
			input: BackgroundConvergenceProjectionInput{
				State:                 BackgroundConvergenceRetrying,
				Dirty:                 true,
				LastRestoreError:      "matrix closed",
				LastRestoreErrorClass: ErrorKindRetryable,
				NextRetry:             &nextRetry,
				FailureCount:          1,
			},
			now:       beforeRetry,
			wantState: BackgroundConvergenceRetrying,
			wantDirty: true,
		},
		{
			name: "dirty failed restore is failed exactly when retry is due",
			input: BackgroundConvergenceProjectionInput{
				State:                 BackgroundConvergenceRetrying,
				Dirty:                 true,
				LastRestoreError:      "matrix closed",
				LastRestoreErrorClass: ErrorKindRetryable,
				NextRetry:             &nextRetry,
				FailureCount:          1,
			},
			now:       atRetry,
			wantState: BackgroundConvergenceFailed,
			wantDirty: true,
		},
		{
			name: "dirty failed restore is failed after retry deadline until attempted",
			input: BackgroundConvergenceProjectionInput{
				State:                 BackgroundConvergenceRetrying,
				Dirty:                 true,
				LastRestoreError:      "matrix closed",
				LastRestoreErrorClass: ErrorKindRetryable,
				NextRetry:             &nextRetry,
				FailureCount:          1,
			},
			now:       afterRetry,
			wantState: BackgroundConvergenceFailed,
			wantDirty: true,
		},
		{
			name: "in flight restore is attempting even when an old retry deadline exists",
			input: BackgroundConvergenceProjectionInput{
				State:                 BackgroundConvergenceAttempting,
				Dirty:                 true,
				LastRestoreError:      "matrix closed",
				LastRestoreErrorClass: ErrorKindRetryable,
				NextRetry:             &nextRetry,
				FailureCount:          1,
			},
			now:       afterRetry,
			wantState: BackgroundConvergenceAttempting,
			wantDirty: true,
		},
		{
			name: "new dirty state without failed attempt remains dirty",
			input: BackgroundConvergenceProjectionInput{
				State: BackgroundConvergenceDirty,
				Dirty: true,
			},
			now:       afterRetry,
			wantState: BackgroundConvergenceDirty,
			wantDirty: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectBackgroundConvergence(tc.input, tc.now)
			if got.State != tc.wantState || got.Dirty != tc.wantDirty || got.Converged {
				t.Fatalf("projection = %+v, want state=%q dirty=%v converged=false", got, tc.wantState, tc.wantDirty)
			}
		})
	}
}

func TestSchedulerBackgroundRetryStateSurvivesDisconnectAndRecoversAfterReconnect(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 35, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1
	})

	client.setDisconnected(true)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return !health.MatrixConnected &&
			health.State == StateDisconnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1
	})
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts while disconnected before retry deadline = %d, want 1", attempts)
	}

	client.setDisconnected(false)
	waitClientAttempts(t, client, "preset:12", 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	if clock.Now().Sub(start) != 0 {
		t.Fatalf("manual clock advanced unexpectedly")
	}
}

func TestSchedulerBackgroundRetryGetsPromptAttemptAfterVerifiedReconnect(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 40, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1
	})
	time.Sleep(40 * time.Millisecond)
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts before verified reconnect = %d, want 1", attempts)
	}

	client.noteReconnectRecovery()
	waitClientAttempts(t, client, "preset:12", 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	if clock.Now().Sub(start) != 0 {
		t.Fatalf("manual clock advanced unexpectedly")
	}
	if got := commandKinds(client.commands()); !reflect.DeepEqual(got, []string{"preset:12"}) {
		t.Fatalf("commands = %v, want one successful prompt reconnect restore", got)
	}
}

func TestSchedulerBackgroundRetryResetsAfterSuccessfulDisplayControl(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("preset:12", net.ErrClosed)
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 23, 8, 45, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "rain"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1
	})
	time.Sleep(40 * time.Millisecond)
	if attempts := client.attempts("preset:12"); attempts != 1 {
		t.Fatalf("preset attempts before display control = %d, want 1", attempts)
	}

	if err := scheduler.EnqueueControl(ctx, ControlRequest{
		Kind:     ControlFill,
		Priority: 100,
		Color:    RGB{R: 9},
	}); err != nil {
		t.Fatal(err)
	}
	waitClientAttempts(t, client, "fill", 1)
	waitClientAttempts(t, client, "preset:12", 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	if clock.Now().Sub(start) != 0 {
		t.Fatalf("manual clock advanced unexpectedly")
	}
	if got := commandKinds(client.commands()); !reflect.DeepEqual(got, []string{"fill", "preset:12"}) {
		t.Fatalf("commands = %v, want successful control followed by prompt background convergence", got)
	}
}

func TestSchedulerDeterministicRenderableBackgroundRenderFailureBacksOff(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	background := newControlledTestAnimation(errors.New("render failed deterministically"), testFrame(41, time.Millisecond))
	background.failNext(3)
	mustRegisterTestAnimation(t, registry, "background", background)

	start := time.Date(2026, 6, 23, 8, 50, 0, 0, time.UTC)
	clock := newManualClock(start)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "background"},
		HeartbeatInterval: 10 * time.Millisecond,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	background.waitAttempts(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			strings.Contains(health.BackgroundLastRestoreError, "render failed deterministically") &&
			health.BackgroundLastRestoreErrorClass == ErrorKindRetryable &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 1
	})
	time.Sleep(40 * time.Millisecond)
	if attempts := background.attemptCount(); attempts != 1 {
		t.Fatalf("render attempts before retry deadline = %d, want 1", attempts)
	}
	if got := commandKinds(client.commands()); len(got) != 0 {
		t.Fatalf("commands before successful render = %v, want none", got)
	}

	clock.Advance(backgroundRetryableMinDelay)
	background.waitAttempts(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			strings.Contains(health.BackgroundLastRestoreError, "render failed deterministically") &&
			health.BackgroundNextRetry != nil &&
			health.BackgroundNextRetry.Equal(start.Add(backgroundRetryableMinDelay+2*backgroundRetryableMinDelay)) &&
			health.BackgroundRetryFailureCount == 2
	})
}

func TestSchedulerPartialRenderableBackgroundFailureRetriesFromFirstFrame(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("frame:52", net.ErrClosed)
	registry := animations.NewRegistry()
	background := newControlledTestAnimation(nil,
		testFrame(51, time.Millisecond),
		testFrame(52, time.Millisecond),
		testFrame(53, time.Millisecond),
	)
	mustRegisterTestAnimation(t, registry, "background", background)

	clock := newManualClock(time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC))
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background:        BackgroundConfig{AnimationID: "background"},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
		Now:               clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	background.waitAttempts(t, 1)
	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
			health.BackgroundDirty &&
			!health.BackgroundConverged &&
			strings.Contains(health.BackgroundLastRestoreError, net.ErrClosed.Error()) &&
			health.BackgroundLastRestoreErrorClass == ErrorKindRetryable
	})
	if got := commandKinds(client.commands()); !reflect.DeepEqual(got, []string{"frame:51"}) {
		t.Fatalf("commands after partial failure = %v, want only first frame committed", got)
	}
	time.Sleep(40 * time.Millisecond)
	if attempts := background.attemptCount(); attempts != 1 {
		t.Fatalf("render attempts before retry deadline = %d, want 1", attempts)
	}
	if attempts := client.attempts("frame:51"); attempts != 1 {
		t.Fatalf("frame:51 attempts before retry deadline = %d, want 1", attempts)
	}
	if attempts := client.attempts("frame:52"); attempts != 1 {
		t.Fatalf("frame:52 attempts before retry deadline = %d, want 1", attempts)
	}

	clock.Advance(backgroundRetryableMinDelay)
	background.waitAttempts(t, 2)
	client.waitFrames(t, 4)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone
	})
	got := commandKinds(client.commands())
	want := []string{"frame:51", "frame:51", "frame:52", "frame:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want full retry from frame zero %v", got, want)
	}
}

func TestBackgroundRetryBoundsAreFixedV1Contract(t *testing.T) {
	retryableMin, retryableMax := backgroundRetryBounds(ErrorKindRetryable)
	if retryableMin != time.Second || retryableMax != 30*time.Second {
		t.Fatalf("retryable background retry bounds = %s..%s, want 1s..30s", retryableMin, retryableMax)
	}
	if delay := exponentialReconnectDelay(1, retryableMin, retryableMax); delay != time.Second {
		t.Fatalf("retryable first delay = %s, want 1s", delay)
	}
	if delay := exponentialReconnectDelay(6, retryableMin, retryableMax); delay != 30*time.Second {
		t.Fatalf("retryable capped delay = %s, want 30s", delay)
	}
	if delay := exponentialReconnectDelay(10, retryableMin, retryableMax); delay != 30*time.Second {
		t.Fatalf("retryable high-attempt capped delay = %s, want 30s", delay)
	}

	permanentMin, permanentMax := backgroundRetryBounds(ErrorKindPermanent)
	if permanentMin != 30*time.Second || permanentMax != 5*time.Minute {
		t.Fatalf("permanent background retry bounds = %s..%s, want 30s..5m", permanentMin, permanentMax)
	}
	if delay := exponentialReconnectDelay(1, permanentMin, permanentMax); delay != 30*time.Second {
		t.Fatalf("permanent first delay = %s, want 30s", delay)
	}
	if delay := exponentialReconnectDelay(5, permanentMin, permanentMax); delay != 5*time.Minute {
		t.Fatalf("permanent capped delay = %s, want 5m", delay)
	}
	if delay := exponentialReconnectDelay(10, permanentMin, permanentMax); delay != 5*time.Minute {
		t.Fatalf("permanent high-attempt capped delay = %s, want 5m", delay)
	}
}

func TestSchedulerPermanentBackgroundMatrixFailuresRemainDirtyAndVisible(t *testing.T) {
	tests := []struct {
		name      string
		kind      BackgroundKind
		err       error
		errorText string
		configure func(*testing.T, *animations.Registry) BackgroundConfig
		failKind  string
	}{
		{
			name:      "firmware status",
			kind:      BackgroundKindFirmwarePreset,
			err:       &StatusError{Status: StatusUnknownCommand},
			errorText: "status",
			configure: func(t *testing.T, registry *animations.Registry) BackgroundConfig {
				t.Helper()
				if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
					EffectID: 12,
					Interval: 90 * time.Millisecond,
					Color:    RGB{R: 0, G: 255, B: 85},
				}); err != nil {
					t.Fatal(err)
				}
				return BackgroundConfig{AnimationID: "rain"}
			},
			failKind: "preset:12",
		},
		{
			name:      "protocol",
			kind:      BackgroundKindRenderable,
			err:       ErrProtocol,
			errorText: ErrProtocol.Error(),
			configure: func(t *testing.T, registry *animations.Registry) BackgroundConfig {
				t.Helper()
				mustRegisterTestAnimation(t, registry, "background", testAnimation(1, time.Millisecond, 41))
				return BackgroundConfig{AnimationID: "background"}
			},
			failKind: "frame:41",
		},
		{
			name:      "validation",
			kind:      BackgroundKindFirmwarePreset,
			err:       ErrInvalidDuration,
			errorText: ErrInvalidDuration.Error(),
			configure: func(t *testing.T, registry *animations.Registry) BackgroundConfig {
				t.Helper()
				if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
					EffectID: 12,
					Interval: 90 * time.Millisecond,
					Color:    RGB{R: 0, G: 255, B: 85},
				}); err != nil {
					t.Fatal(err)
				}
				return BackgroundConfig{AnimationID: "rain"}
			},
			failKind: "preset:12",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeMatrixClient()
			for i := 0; i < 8; i++ {
				client.failCommand(tc.failKind, tc.err)
			}
			registry := animations.NewRegistry()
			background := tc.configure(t, registry)

			reconnectAttempts := make(chan ReconnectAttempt, 4)
			reconnectRecoveries := make(chan ReconnectRecovery, 4)
			start := time.Date(2026, 6, 23, 9, 10, 0, 0, time.UTC)
			clock := newManualClock(start)
			scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
				Background:           background,
				HeartbeatInterval:    25 * time.Millisecond,
				ReconnectMinDelay:    time.Millisecond,
				ReconnectMaxDelay:    time.Millisecond,
				ReconnectJitter:      noReconnectJitter,
				Now:                  clock.Now,
				OnReconnectDelay:     func(attempt ReconnectAttempt) { reconnectAttempts <- attempt },
				OnReconnectRecovered: func(recovery ReconnectRecovery) { reconnectRecoveries <- recovery },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runScheduler(t, ctx, scheduler)

			waitSchedulerHealth(t, scheduler, func(health Health) bool {
				return health.MatrixConnected &&
					health.BackgroundKind == tc.kind &&
					health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
					health.BackgroundDirty &&
					!health.BackgroundConverged &&
					health.BackgroundLastRestoreAttempt != nil &&
					health.BackgroundLastRestoreSuccess == nil &&
					strings.Contains(health.BackgroundLastRestoreError, tc.errorText) &&
					health.BackgroundLastRestoreErrorClass == ErrorKindPermanent &&
					health.BackgroundNextRetry != nil &&
					health.BackgroundNextRetry.Equal(start.Add(backgroundPermanentMinDelay)) &&
					health.BackgroundRetryFailureCount == 1 &&
					health.LastFailure != nil
			})

			select {
			case attempt := <-reconnectAttempts:
				t.Fatalf("reconnect attempt = %+v, want none for permanent background restore failure", attempt)
			default:
			}
			select {
			case recovery := <-reconnectRecoveries:
				t.Fatalf("reconnect recovery = %+v, want none for permanent background restore failure", recovery)
			default:
			}
			if got := commandKinds(client.commands()); len(got) != 0 {
				t.Fatalf("commands = %v, want no successful background command after permanent failure", got)
			}
			health := scheduler.Health()
			if !health.BackgroundDirty || health.BackgroundConverged || health.BackgroundConvergenceState == BackgroundConvergenceConverged {
				t.Fatalf("background health = %+v, want dirty non-converged failure visible", health)
			}

			time.Sleep(75 * time.Millisecond)
			if attempts := client.attempts(tc.failKind); attempts != 1 {
				t.Fatalf("%s attempts before permanent retry deadline = %d, want 1", tc.failKind, attempts)
			}
			health = scheduler.Health()
			if !health.BackgroundDirty ||
				health.BackgroundConverged ||
				health.BackgroundConvergenceState != BackgroundConvergenceRetrying ||
				health.BackgroundNextRetry == nil ||
				!health.BackgroundNextRetry.Equal(start.Add(backgroundPermanentMinDelay)) ||
				health.BackgroundRetryFailureCount != 1 {
				t.Fatalf("background health before permanent retry deadline = %+v, want pending retry state", health)
			}

			clock.Advance(backgroundPermanentMinDelay)
			waitClientAttempts(t, client, tc.failKind, 2)
			waitSchedulerHealth(t, scheduler, func(health Health) bool {
				return health.BackgroundConvergenceState == BackgroundConvergenceRetrying &&
					health.BackgroundDirty &&
					!health.BackgroundConverged &&
					health.BackgroundLastRestoreErrorClass == ErrorKindPermanent &&
					health.BackgroundNextRetry != nil &&
					health.BackgroundNextRetry.Equal(start.Add(backgroundPermanentMinDelay+2*backgroundPermanentMinDelay)) &&
					health.BackgroundRetryFailureCount == 2
			})
		})
	}
}

func TestSchedulerDoesNotApplyBackgroundWhenNoneConfigured(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		HeartbeatInterval: 10 * time.Millisecond,
	})
	initialHealth := scheduler.Health()
	if initialHealth.BackgroundID != "" ||
		initialHealth.BackgroundKind != "" ||
		initialHealth.BackgroundConvergenceState != BackgroundConvergenceUnknown ||
		initialHealth.BackgroundDirty ||
		initialHealth.BackgroundConverged ||
		initialHealth.BackgroundLastRestoreAttempt != nil ||
		initialHealth.BackgroundLastRestoreSuccess != nil ||
		initialHealth.BackgroundLastRestoreError != "" ||
		initialHealth.BackgroundLastRestoreErrorClass != ErrorKindNone ||
		initialHealth.BackgroundNextRetry != nil ||
		initialHealth.BackgroundRetryFailureCount != 0 {
		t.Fatalf("initial no-background health = %+v, want empty unknown defaults", initialHealth)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.State == StateReady &&
			health.BackgroundID == "" &&
			health.BackgroundKind == "" &&
			health.BackgroundConvergenceState == BackgroundConvergenceUnknown &&
			!health.BackgroundDirty &&
			!health.BackgroundConverged &&
			health.BackgroundLastRestoreAttempt == nil &&
			health.BackgroundLastRestoreSuccess == nil &&
			health.BackgroundLastRestoreError == "" &&
			health.BackgroundLastRestoreErrorClass == ErrorKindNone &&
			health.BackgroundNextRetry == nil &&
			health.BackgroundRetryFailureCount == 0
	})
	time.Sleep(50 * time.Millisecond)
	if got := commandKinds(client.commands()); len(got) != 0 {
		t.Fatalf("commands = %v, want no background commands", got)
	}
}

func TestSchedulerDoesNotResendCleanBackgroundWhileIdle(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	time.Sleep(50 * time.Millisecond)
	got := commandKinds(client.commands())
	want := []string{"preset:12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestSchedulerRestoresBackgroundAfterVerifiedReconnect(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	client.failCommand("ping", net.ErrClosed)
	waitClientAttempts(t, client, "ping", 3)
	client.waitCommands(t, 2)
	got := commandKinds(client.commands())
	want := []string{"preset:12", "preset:12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestSchedulerMarksBackgroundDirtyWhileReconnectPending(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})

	client.setDisconnected(true)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return !health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceDirty &&
			health.BackgroundDirty &&
			!health.BackgroundConverged
	})

	client.setDisconnected(false)
	client.waitCommands(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})
}

func TestSchedulerMarksBackgroundDirtyAfterDisplayControl(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}
	mustRegisterTestAnimation(t, registry, "hold", testAnimation(1, 75*time.Millisecond, 44))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})

	client.setCommandDelay("fill", 25*time.Millisecond)
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- scheduler.EnqueueControl(ctx, ControlRequest{
			Kind:     ControlFill,
			Priority: 100,
			Color:    RGB{R: 9},
		})
	}()
	waitClientAttempts(t, client, "fill", 1)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "hold",
		AnimationID:   "hold",
		Priority:      1,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-controlErr:
		if err != nil {
			t.Fatalf("control error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control did not complete")
	}

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundDirty &&
			!health.BackgroundConverged &&
			(health.BackgroundConvergenceState == BackgroundConvergenceDirty ||
				health.BackgroundConvergenceState == BackgroundConvergenceAttempting)
	})
	client.waitCommands(t, 4)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.BackgroundConvergenceState == BackgroundConvergenceConverged &&
			!health.BackgroundDirty &&
			health.BackgroundConverged
	})
}

func TestSchedulerRetriesRenderableBackgroundAfterReconnectRenderFailure(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	background := newControlledTestAnimation(errors.New("render failed"), testFrame(51, time.Millisecond), testFrame(52, time.Millisecond))
	mustRegisterTestAnimation(t, registry, "background", background)

	depths := newQueueDepthRecorder()
	clock := newManualClock(time.Date(2026, 6, 23, 9, 20, 0, 0, time.UTC))
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "background",
		},
		HeartbeatInterval:  5 * time.Millisecond,
		Now:                clock.Now,
		OnQueueDepthChange: depths.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitFrames(t, 2)
	background.waitAttempts(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady && health.LastFailure == nil
	})
	background.failNext(1)
	client.noteReconnectRecovery()

	background.waitAttempts(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.LastFailure != nil
	})
	time.Sleep(25 * time.Millisecond)
	if attempts := background.attemptCount(); attempts != 2 {
		t.Fatalf("render attempts before retry deadline = %d, want 2", attempts)
	}
	clock.Advance(backgroundRetryableMinDelay)
	background.waitAttempts(t, 3)
	client.waitFrames(t, 4)

	got := commandKinds(client.commands())
	want := []string{"frame:51", "frame:52", "frame:51", "frame:52"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want 0; background restore must stay scheduler-owned", got)
	}
	if got := depths.values(); len(got) != 0 {
		t.Fatalf("queue depth changes = %v, want none for background restore", got)
	}
}

func TestSchedulerRestoresBackgroundAfterClientImmediateReconnect(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("rain", animations.FirmwarePreset{
		EffectID: 12,
		Interval: 90 * time.Millisecond,
		Color:    RGB{R: 0, G: 255, B: 85},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Background: BackgroundConfig{
			AnimationID: "rain",
		},
		HeartbeatInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	client.waitCommands(t, 1)
	client.noteReconnectRecovery()
	client.waitCommands(t, 2)
	got := commandKinds(client.commands())
	want := []string{"preset:12", "preset:12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestNewSchedulerRejectsInvalidBackgroundPresetBeforeStartup(t *testing.T) {
	client := newFakeMatrixClient()
	registry := invalidFirmwarePresetRegistry{
		Registry: animations.NewRegistry(),
		preset: animations.FirmwarePreset{
			EffectID: 9,
			Interval: -time.Millisecond,
		},
	}

	_, err := NewScheduler(SchedulerOptions{
		Client:     client,
		Registry:   registry,
		Background: BackgroundConfig{AnimationID: "bad_rain"},
	})
	if err == nil || !strings.Contains(err.Error(), `background animation "bad_rain": firmware preset interval cannot be negative`) {
		t.Fatalf("NewScheduler() error = %v, want invalid background preset bounds", err)
	}
	if len(client.commands()) != 0 {
		t.Fatalf("matrix commands = %v, want none before scheduler startup", client.commands())
	}
}

func TestNewSchedulerAcceptsFirmwarePresetBackgroundBeforeStartup(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("matrix_rain_background", animations.FirmwarePreset{
		EffectID: 8,
		Interval: 100 * time.Millisecond,
		Color:    RGB{R: 0, G: 120, B: 255},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler, err := NewScheduler(SchedulerOptions{
		Client:     client,
		Registry:   registry,
		Background: BackgroundConfig{AnimationID: "matrix_rain_background"},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v, want firmware preset background accepted", err)
	}
	t.Cleanup(scheduler.Close)
	if len(client.commands()) != 0 {
		t.Fatalf("matrix commands = %v, want none before scheduler startup", client.commands())
	}
}

func TestSchedulerResolveRequestRejectsFirmwarePresetAnimationAsNonRenderable(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	if err := registry.RegisterFirmwarePreset("matrix_rain_background", animations.FirmwarePreset{
		EffectID: 8,
		Interval: 100 * time.Millisecond,
		Color:    RGB{R: 0, G: 120, B: 255},
	}); err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	err := scheduler.EnqueueRequest(context.Background(), animations.AnimationRequest{
		ID:          "bad-playback",
		AnimationID: "matrix_rain_background",
	})
	if !errors.Is(err, ErrNonRenderableAnimation) {
		t.Fatalf("EnqueueRequest() error = %v, want ErrNonRenderableAnimation", err)
	}
	if errors.Is(err, ErrMissingAnimation) {
		t.Fatalf("EnqueueRequest() error = %v, must not be ErrMissingAnimation", err)
	}
	if !strings.Contains(err.Error(), "matrix_rain_background") || !strings.Contains(err.Error(), "playable") {
		t.Fatalf("EnqueueRequest() error = %v, want animation id and playable context", err)
	}
	if len(client.commands()) != 0 {
		t.Fatalf("matrix commands = %v, want no preset command for ordinary playback", client.commands())
	}
}

func TestSchedulerResolveRequestRejectsUnknownAnimationAsMissing(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})

	_, err := scheduler.ResolveRequest(context.Background(), animations.AnimationRequest{
		ID:          "missing-playback",
		AnimationID: "does_not_exist",
	})
	if !errors.Is(err, ErrMissingAnimation) {
		t.Fatalf("ResolveRequest() error = %v, want ErrMissingAnimation", err)
	}
	if errors.Is(err, ErrNonRenderableAnimation) {
		t.Fatalf("ResolveRequest() error = %v, must not be ErrNonRenderableAnimation", err)
	}
}

func TestSchedulerPreviousFrameRestoresPreItemFill(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 100))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	initialColor := RGB{R: 1, G: 2, B: 3}
	if err := scheduler.Fill(ctx, initialColor); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 4)
	commands := client.commands()
	got := commandKinds(commands)
	want := []string{"fill", "frame:100", "frame:101", "fill"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
	if commands[3].color != initialColor {
		t.Fatalf("restored fill color = %+v, want %+v", commands[3].color, initialColor)
	}
}

func TestSchedulerPreviousFrameRestoresPreItemFrame(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "initial", testAnimation(1, time.Millisecond, 7))
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 120))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "initial",
		AnimationID:   "initial",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 4)
	got := commandKinds(client.commands())
	want := []string{"frame:7", "frame:120", "frame:121", "frame:7"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
}

func TestSchedulerPreviousFrameDoesNothingWithoutKnownDisplayState(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(2, time.Millisecond, 140))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestorePreviousFrame,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 2)
	time.Sleep(20 * time.Millisecond)
	got := commandKinds(client.commands())
	want := []string{"frame:140", "frame:141"}
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want %v", got, want)
		}
	}
}

func TestSchedulerDoesNotRunMatrixCommandsConcurrently(t *testing.T) {
	client := newFakeMatrixClient()
	client.commandDelay = 10 * time.Millisecond
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "a", testAnimation(2, time.Millisecond, 40))
	mustRegisterTestAnimation(t, registry, "b", testAnimation(2, time.Millisecond, 50))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "a",
		AnimationID:   "a",
		RestorePolicy: animations.RestoreClear,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "b",
		AnimationID:   "b",
		RestorePolicy: animations.RestoreClear,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 6)
	if client.maxConcurrent() > 1 {
		t.Fatalf("max concurrent matrix commands = %d, want 1", client.maxConcurrent())
	}
}

func TestSchedulerQueuedControlRunsAfterCurrentAnimationCompletes(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(3, 25*time.Millisecond, 80))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.Fill(ctx, RGB{R: 1, G: 2, B: 3})
	}()

	client.waitCommands(t, 4)
	got := commandKinds(client.commands())
	want := []string{"frame:80", "frame:81", "frame:82", "fill"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Fill() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fill() did not return after scheduled execution")
	}
}

func TestSchedulerInvalidPresetControlReturnsImmediatelyWithoutEnqueueing(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 250*time.Millisecond, 85))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	start := time.Now()
	err := scheduler.SetPreset(ctx, 7, (maxMilliseconds+1)*time.Millisecond, RGB{B: 3})
	if !errors.Is(err, ErrInvalidDuration) {
		t.Fatalf("SetPreset() error = %v, want %v", err, ErrInvalidDuration)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("SetPreset() returned after %s, want immediate validation failure", elapsed)
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("queue length = %d, want invalid control not enqueued", got)
	}
	if attempts := client.attempts("preset:" + stringMarker(7)); attempts != 0 {
		t.Fatalf("preset attempts = %d, want 0", attempts)
	}

	got := commandKinds(client.commands())
	if len(got) != 1 || got[0] != "frame:85" {
		t.Fatalf("commands = %v, want only the in-flight frame", got)
	}
}

func TestSchedulerResolveControlRejectsMissingAndUnsupportedKinds(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})

	tests := []struct {
		name    string
		request ControlRequest
	}{
		{name: "missing", request: ControlRequest{}},
		{name: "unsupported", request: ControlRequest{Kind: ControlKind("unknown")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := scheduler.ResolveControl(context.Background(), tt.request); !errors.Is(err, ErrInvalidControl) {
				t.Fatalf("ResolveControl() error = %v, want %v", err, ErrInvalidControl)
			}
			if got := scheduler.QueueLen(); got != 0 {
				t.Fatalf("queue length = %d, want invalid control not enqueued", got)
			}
			if got := len(client.commands()); got != 0 {
				t.Fatalf("commands = %d, want 0", got)
			}
		})
	}
}

func TestSchedulerControlsDoNotRunMatrixCommandsConcurrently(t *testing.T) {
	client := newFakeMatrixClient()
	client.commandDelay = 10 * time.Millisecond
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(3, time.Millisecond, 90))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 1)
	errCh := make(chan error, 4)
	go func() { errCh <- scheduler.Fill(ctx, RGB{R: 9}) }()
	go func() { errCh <- scheduler.Clear(ctx) }()
	go func() { errCh <- scheduler.SetBrightness(ctx, 42) }()
	go func() { errCh <- scheduler.SetPreset(ctx, 7, 100*time.Millisecond, RGB{B: 3}) }()

	client.waitCommands(t, 7)
	for i := 0; i < 4; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("control error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("control did not return after scheduled execution")
		}
	}

	if client.maxConcurrent() > 1 {
		t.Fatalf("max concurrent matrix commands = %d, want 1", client.maxConcurrent())
	}
}

func TestInternalQueueDoesNotExposeRawMutationMethods(t *testing.T) {
	queueType := reflect.TypeOf(&playQueue{})
	for _, name := range []string{"Enqueue", "EnqueueScheduled", "Clear"} {
		if _, ok := queueType.MethodByName(name); ok {
			t.Fatalf("playQueue exposes %s; callers must use Scheduler admission and clearing APIs", name)
		}
	}
}

func TestSchedulerOptionsDoesNotExposeRawQueue(t *testing.T) {
	optionsType := reflect.TypeOf(SchedulerOptions{})
	if _, ok := optionsType.FieldByName("Queue"); ok {
		t.Fatal("SchedulerOptions exposes Queue; scheduler must own queue construction")
	}
	if _, ok := optionsType.FieldByName("QueueCapacity"); !ok {
		t.Fatal("SchedulerOptions does not expose QueueCapacity")
	}
}

func TestSchedulerOptionsDoesNotExposeCriticalPathOutcomeSink(t *testing.T) {
	optionsType := reflect.TypeOf(SchedulerOptions{})
	if _, ok := optionsType.FieldByName("OnItemOutcomeRecordedCriticalPath"); ok {
		t.Fatal("SchedulerOptions exposes critical-path outcome sink; use the reliable app metrics constructor path")
	}
}

func TestReliableOutcomeRecorderConstructorOnlyUsedByAppProduction(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var uses []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/matrix/scheduler.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "NewSchedulerWithReliableAppOutcomeRecorder") {
			uses = append(uses, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0] != "internal/app/app.go" {
		t.Fatalf("reliable app outcome recorder production uses = %v, want [internal/app/app.go]", uses)
	}
}

func TestSchedulerDoesNotExposeMutableQueue(t *testing.T) {
	schedulerType := reflect.TypeOf(&Scheduler{})
	if _, ok := schedulerType.MethodByName("Queue"); ok {
		t.Fatal("Scheduler exposes Queue(); callers must use scheduler-owned queue views")
	}
}

func TestSchedulerQueueSnapshotControlStatusIsImmutable(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(1, 300*time.Millisecond, 21))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current",
		AnimationID:   "current",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	createdAt := time.Unix(1700000000, 123).UTC()
	deadline := time.Now().Add(5 * time.Second).UTC()
	originalColor := RGB{R: 7, G: 8, B: 9}
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:        "queued-control",
			Kind:      ControlFill,
			Priority:  41,
			Color:     originalColor,
			CreatedAt: createdAt,
			Deadline:  deadline,
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	snapshot := scheduler.QueueSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("QueueSnapshot() length = %d, want 1", len(snapshot))
	}
	if snapshot[0].Kind != QueueItemControl || snapshot[0].Control == nil {
		t.Fatalf("snapshot item = %+v, want queued control status", snapshot[0])
	}
	expected := cloneQueueSnapshot(snapshot)

	mutateQueueItemStatus(&snapshot[0])

	after := scheduler.QueueSnapshot()
	if !reflect.DeepEqual(after, expected) {
		t.Fatalf("QueueSnapshot() after DTO mutation = %+v, want unchanged %+v", after, expected)
	}

	client.waitCommands(t, 2)
	select {
	case err := <-controlErr:
		if err != nil {
			t.Fatalf("queued control error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued control did not complete after current item finished")
	}

	commands := client.commands()
	got := commandKinds(commands)
	want := []string{"frame:21", "fill"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
	if commands[1].color != originalColor {
		t.Fatalf("executed control color = %+v, want originally admitted color %+v", commands[1].color, originalColor)
	}
}

func TestSchedulerQueueSnapshotAnimationStatusIsImmutable(t *testing.T) {
	assertQueueItemStatusExposesNoFrameOrHookFields(t)

	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(1, 300*time.Millisecond, 31))
	mustRegisterTestAnimation(t, registry, "queued", testAnimation(1, time.Millisecond, 77))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current",
		AnimationID:   "current",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	createdAt := time.Unix(1700000100, 456).UTC()
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "queued-animation",
		EventID:       "event-1",
		AnimationID:   "queued",
		Priority:      23,
		RestorePolicy: animations.RestorePreviousFrame,
		CreatedAt:     createdAt,
	}); err != nil {
		t.Fatal(err)
	}
	waitSchedulerQueueLen(t, scheduler, 1)

	snapshot := scheduler.QueueSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("QueueSnapshot() length = %d, want 1", len(snapshot))
	}
	if snapshot[0].Kind != QueueItemAnimation || snapshot[0].Control != nil {
		t.Fatalf("snapshot item = %+v, want queued animation status", snapshot[0])
	}
	expected := cloneQueueSnapshot(snapshot)

	mutateQueueItemStatus(&snapshot[0])

	after := scheduler.QueueSnapshot()
	if !reflect.DeepEqual(after, expected) {
		t.Fatalf("QueueSnapshot() after DTO mutation = %+v, want unchanged %+v", after, expected)
	}

	client.waitFrames(t, 3)
	got := commandKinds(client.commands())
	want := []string{"frame:31", "frame:77", "frame:31"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
}

func TestInternalQueueRemovePreservesSnapshotOrdering(t *testing.T) {
	queue := newPlayQueue(8)
	ctx := context.Background()
	var dropHandle queueHandle
	for _, item := range []ScheduledItem{
		{PlayItem: PlayItem{ID: "low-1", Priority: 10}},
		{PlayItem: PlayItem{ID: "drop", Priority: 40}},
		{PlayItem: PlayItem{ID: "high", Priority: 50}},
		{PlayItem: PlayItem{ID: "low-2", Priority: 10}},
	} {
		handle, _, err := queue.enqueueScheduled(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if item.ID == "drop" {
			dropHandle = handle
		}
	}

	if _, _, ok := queue.remove(dropHandle); !ok {
		t.Fatal("queue.remove(drop handle) = false, want true")
	}

	got := queueIDs(queue.snapshot())
	want := []string{"high", "low-1", "low-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot ids = %v, want %v", got, want)
	}
}

func TestSchedulerDuplicateControlIDsCancelOnlyMatchingQueuedControl(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 220*time.Millisecond, 32))

	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: outcomes.record,
	})
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	runScheduler(t, runCtx, scheduler)

	if err := scheduler.EnqueueRequest(runCtx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- scheduler.EnqueueControl(runCtx, ControlRequest{
			ID:    "duplicate-control-id",
			Kind:  ControlFill,
			Color: RGB{R: 1},
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	secondCtx, cancelSecond := context.WithCancel(runCtx)
	secondErr := make(chan error, 1)
	go func() {
		secondErr <- scheduler.EnqueueControl(secondCtx, ControlRequest{
			ID:    "duplicate-control-id",
			Kind:  ControlFill,
			Color: RGB{G: 2},
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 2)

	cancelSecond()
	select {
	case err := <-secondErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second control error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second control did not return promptly after cancellation")
	}
	waitSchedulerQueueLen(t, scheduler, 1)

	select {
	case err := <-firstErr:
		t.Fatalf("first control completed before execution with error %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	client.waitCommands(t, 2)
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatalf("first control error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first control did not complete after scheduled execution")
	}

	commands := client.commands()
	got := commandKinds(commands)
	want := []string{"frame:32", "fill"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}
	if commands[1].color != (RGB{R: 1}) {
		t.Fatalf("executed fill color = %+v, want first control color", commands[1].color)
	}

	reports := waitOutcomeReportsMatching(t, outcomes, func(report OutcomeReport) bool {
		if report.ItemKind != QueueItemControl {
			return false
		}
		if report.ItemID != "duplicate-control-id" || report.ControlKind != ControlFill {
			return false
		}
		return report.Outcome == ItemOutcomeCanceled || report.Outcome == ItemOutcomeExecuted
	}, 2)
	if len(reports) != 2 {
		t.Fatalf("control outcomes = %+v, want 2 reports", reports)
	}
	canceled := 0
	executed := 0
	for _, report := range reports {
		if report.ItemID != "duplicate-control-id" {
			t.Fatalf("control outcome item ID = %q, want duplicate-control-id", report.ItemID)
		}
		if report.ControlKind != ControlFill {
			t.Fatalf("control outcome kind = %q, want %q", report.ControlKind, ControlFill)
		}
		switch report.Outcome {
		case ItemOutcomeCanceled:
			canceled++
		case ItemOutcomeExecuted:
			executed++
		default:
			t.Fatalf("control outcome = %q, want canceled or executed", report.Outcome)
		}
	}
	if canceled != 1 || executed != 1 {
		t.Fatalf("duplicate ID outcomes canceled=%d executed=%d, want 1 each", canceled, executed)
	}
}

func TestSchedulerClearQueueCompletesWaitingControls(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 250*time.Millisecond, 10))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	errCh := make(chan error, 2)
	go func() {
		errCh <- scheduler.Fill(ctx, RGB{R: 1, G: 2, B: 3})
	}()
	go func() {
		errCh <- scheduler.SetBrightness(ctx, 42)
	}()
	waitSchedulerQueueLen(t, scheduler, 2)

	if cleared := scheduler.ClearQueue(); cleared != 2 {
		t.Fatalf("ClearQueue() = %d, want 2", cleared)
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if !errors.Is(err, ErrControlQueueCleared) {
				t.Fatalf("control error = %v, want %v", err, ErrControlQueueCleared)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("control did not return promptly after queue clear")
		}
	}

	time.Sleep(300 * time.Millisecond)
	got := commandKinds(client.commands())
	if len(got) != 1 || got[0] != "frame:10" {
		t.Fatalf("commands = %v, want only the in-flight frame", got)
	}
}

func TestSchedulerClearQueueReportsAnimationOutcomes(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "low-animation", testAnimation(1, time.Millisecond, 21))
	mustRegisterTestAnimation(t, registry, "high-animation", testAnimation(1, time.Millisecond, 22))

	now := time.Date(2026, 6, 23, 10, 11, 12, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx := context.Background()

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "queued-low",
		EventID:       "event-low",
		AnimationID:   "low-animation",
		Priority:      10,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "queued-high",
		EventID:       "event-high",
		AnimationID:   "high-animation",
		Priority:      30,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	if cleared := scheduler.ClearQueue(); cleared != 2 {
		t.Fatalf("ClearQueue() = %d, want 2", cleared)
	}

	reports := waitOutcomeReports(t, outcomes, 2)
	assertQueueClearedOutcome(t, reports, "queued-low", OutcomeWant{
		Kind:           QueueItemAnimation,
		EventID:        "event-low",
		AnimationID:    "low-animation",
		Priority:       10,
		Depth:          2,
		AdmissionDepth: 1,
		Timestamp:      now,
	})
	assertQueueClearedOutcome(t, reports, "queued-high", OutcomeWant{
		Kind:           QueueItemAnimation,
		EventID:        "event-high",
		AnimationID:    "high-animation",
		Priority:       30,
		Depth:          2,
		AdmissionDepth: 2,
		Timestamp:      now,
	})
}

func TestSchedulerClearQueueReportsControlOutcomesAndCompletesWaiters(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	now := time.Date(2026, 6, 23, 11, 12, 13, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx := context.Background()

	errCh := make(chan error, 2)
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:       "fill-control",
			Kind:     ControlFill,
			Priority: 40,
			Color:    RGB{R: 1, G: 2, B: 3},
		})
	}()
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:         "brightness-control",
			Kind:       ControlSetBrightness,
			Priority:   20,
			Brightness: 42,
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 2)

	if cleared := scheduler.ClearQueue(); cleared != 2 {
		t.Fatalf("ClearQueue() = %d, want 2", cleared)
	}
	assertControlErrors(t, errCh, 2, ErrControlQueueCleared)

	reports := waitOutcomeReports(t, outcomes, 2)
	assertQueueClearedOutcome(t, reports, "fill-control", OutcomeWant{
		Kind:        QueueItemControl,
		ControlKind: ControlFill,
		Priority:    40,
		Depth:       2,
		Timestamp:   now,
	})
	assertQueueClearedOutcome(t, reports, "brightness-control", OutcomeWant{
		Kind:        QueueItemControl,
		ControlKind: ControlSetBrightness,
		Priority:    20,
		Depth:       2,
		Timestamp:   now,
	})
}

func TestSchedulerClearQueueClearsMixedAnimationsAndControls(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(1, 250*time.Millisecond, 11))

	now := time.Date(2026, 6, 23, 12, 13, 14, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current",
		AnimationID:   "current",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	var hooksMu sync.Mutex
	started := 0
	finished := 0
	hook := func(counter *int) Hook {
		return func(context.Context) error {
			hooksMu.Lock()
			defer hooksMu.Unlock()
			(*counter)++
			return nil
		}
	}

	for _, item := range []ScheduledItem{
		{
			PlayItem: PlayItem{
				ID:       "queued-animation-1",
				EventID:  "event-animation-1",
				Priority: 10,
				Frames:   []Frame{testFrame(80, time.Millisecond)},
				OnStart:  hook(&started),
				OnFinish: hook(&finished),
			},
			AnimationID:   "queued-animation-1",
			RestorePolicy: animations.RestoreLeave,
		},
		{
			PlayItem: PlayItem{
				ID:       "queued-animation-2",
				EventID:  "event-animation-2",
				Priority: 20,
				Frames:   []Frame{testFrame(90, time.Millisecond)},
				OnStart:  hook(&started),
				OnFinish: hook(&finished),
			},
			AnimationID:   "queued-animation-2",
			RestorePolicy: animations.RestoreLeave,
		},
	} {
		if _, _, err := scheduler.queue.enqueueScheduled(ctx, item); err != nil {
			t.Fatal(err)
		}
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- scheduler.Fill(ctx, RGB{R: 1, G: 2, B: 3})
	}()
	go func() {
		errCh <- scheduler.SetBrightness(ctx, 42)
	}()
	waitSchedulerQueueLen(t, scheduler, 4)

	if !errors.Is(scheduler.completeClearedPlayItem(PlayItem{}), ErrPlayItemQueueCleared) {
		t.Fatalf("cleared play item outcome does not match %v", ErrPlayItemQueueCleared)
	}
	if cleared := scheduler.ClearQueue(); cleared != 4 {
		t.Fatalf("ClearQueue() = %d, want 4", cleared)
	}
	assertControlErrors(t, errCh, 2, ErrControlQueueCleared)

	time.Sleep(300 * time.Millisecond)
	got := commandKinds(client.commands())
	if len(got) != 1 || got[0] != "frame:11" {
		t.Fatalf("commands = %v, want only the in-flight animation frame", got)
	}

	hooksMu.Lock()
	defer hooksMu.Unlock()
	if started != 0 || finished != 0 {
		t.Fatalf("cleared animation hooks start=%d finish=%d, want both 0", started, finished)
	}

	reports := waitOutcomeReportsMatching(t, outcomes, func(report OutcomeReport) bool {
		if report.Outcome != ItemOutcomeQueueCleared {
			return false
		}
		switch report.ItemID {
		case "queued-animation-1":
			return report.ItemKind == QueueItemAnimation && report.AnimationID == "queued-animation-1"
		case "queued-animation-2":
			return report.ItemKind == QueueItemAnimation && report.AnimationID == "queued-animation-2"
		default:
			return report.ItemKind == QueueItemControl &&
				(report.ControlKind == ControlFill || report.ControlKind == ControlSetBrightness)
		}
	}, 4)
	if len(reports) != 4 {
		t.Fatalf("queue-cleared outcomes = %+v, want 4 reports", reports)
	}
	assertQueueClearedOutcome(t, reports, "queued-animation-1", OutcomeWant{
		Kind:        QueueItemAnimation,
		EventID:     "event-animation-1",
		AnimationID: "queued-animation-1",
		Priority:    10,
		Depth:       4,
		Timestamp:   now,
	})
	assertQueueClearedOutcome(t, reports, "queued-animation-2", OutcomeWant{
		Kind:        QueueItemAnimation,
		EventID:     "event-animation-2",
		AnimationID: "queued-animation-2",
		Priority:    20,
		Depth:       4,
		Timestamp:   now,
	})
	controlReports := reportsByKind(reports, QueueItemControl)
	if len(controlReports) != 2 {
		t.Fatalf("control outcomes = %+v, want 2 reports", controlReports)
	}
	for _, report := range controlReports {
		if report.ControlKind != ControlFill && report.ControlKind != ControlSetBrightness {
			t.Fatalf("control outcome kind = %q, want fill or brightness", report.ControlKind)
		}
		if report.QueueDepthBeforeClear != 4 {
			t.Fatalf("control outcome depth = %d, want 4", report.QueueDepthBeforeClear)
		}
	}
}

func TestSchedulerClearQueueReportsNoOutcomesForEmptyQueue(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: outcomes.record,
	})

	if cleared := scheduler.ClearQueue(); cleared != 0 {
		t.Fatalf("ClearQueue() = %d, want 0", cleared)
	}
	if reports := outcomes.reports(); len(reports) != 0 {
		t.Fatalf("outcomes = %+v, want none", reports)
	}
}

func TestSchedulerOutcomeObserverPanicDoesNotBreakClearQueueOrControlCompletion(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: func(OutcomeReport) {
			panic("observer failed")
		},
	})
	ctx := context.Background()

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:   "clear-control",
			Kind: ControlClear,
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	if cleared := scheduler.ClearQueue(); cleared != 1 {
		t.Fatalf("ClearQueue() = %d, want 1", cleared)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrControlQueueCleared) {
			t.Fatalf("control error = %v, want %v", err, ErrControlQueueCleared)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("control did not complete promptly after observer panic")
	}
}

func TestSchedulerOutcomeObserverBlockDoesNotDelayClearQueueOrControlCompletion(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: func(OutcomeReport) {
			close(observerEntered)
			<-releaseObserver
		},
	})
	defer close(releaseObserver)
	ctx := context.Background()

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:   "queued-control",
			Kind: ControlClear,
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	done := make(chan int, 1)
	go func() {
		done <- scheduler.ClearQueue()
	}()

	select {
	case cleared := <-done:
		if cleared != 1 {
			t.Fatalf("ClearQueue() = %d, want 1", cleared)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ClearQueue blocked behind outcome observer")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrControlQueueCleared) {
			t.Fatalf("control error = %v, want %v", err, ErrControlQueueCleared)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("control completion blocked behind outcome observer")
	}

	select {
	case <-observerEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome observer was not invoked")
	}
}

func TestSchedulerOutcomeObserverBlockDoesNotSpawnPerOutcomeGoroutine(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "queued", testAnimation(1, time.Millisecond, 21))

	recordedOutcomes := newOutcomeRecorder()
	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	var observerMu sync.Mutex
	observerCalls := 0
	release := func() {
		releaseOnce.Do(func() {
			close(releaseObserver)
		})
	}
	scheduler := newTestSchedulerWithReliableOutcomeRecorder(t, client, registry, SchedulerOptions{
		QueueCapacity: outcomeObserverQueueCapacity + 8,
		OnItemOutcome: func(OutcomeReport) {
			observerMu.Lock()
			observerCalls++
			observerMu.Unlock()
			enteredOnce.Do(func() {
				close(observerEntered)
			})
			<-releaseObserver
		},
	}, recordedOutcomes.record)
	defer release()

	ctx := context.Background()
	totalOutcomes := outcomeObserverQueueCapacity + 8
	for i := 0; i < totalOutcomes; i++ {
		if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
			ID:            "queued-animation-" + strconv.Itoa(i),
			AnimationID:   "queued",
			RestorePolicy: animations.RestoreLeave,
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitSchedulerQueueLen(t, scheduler, totalOutcomes)

	if cleared := scheduler.ClearQueue(); cleared != totalOutcomes {
		t.Fatalf("ClearQueue() = %d, want %d", cleared, totalOutcomes)
	}
	if reports := recordedOutcomes.reports(); len(reports) != totalOutcomes {
		t.Fatalf("recorded outcomes = %d, want every outcome %d", len(reports), totalOutcomes)
	}
	if got := scheduler.OutcomeReportsDropped(); got == 0 {
		t.Fatalf("OutcomeReportsDropped() = %d, want drops under observer backpressure", got)
	}
	if got := scheduler.Health().OutcomeReportsDropped; got != scheduler.OutcomeReportsDropped() {
		t.Fatalf("Health().OutcomeReportsDropped = %d, want %d", got, scheduler.OutcomeReportsDropped())
	}
	select {
	case <-observerEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome observer was not invoked")
	}
	time.Sleep(25 * time.Millisecond)
	observerMu.Lock()
	callsWhileBlocked := observerCalls
	observerMu.Unlock()
	if callsWhileBlocked != 1 {
		t.Fatalf("observer calls while first callback is blocked = %d, want 1", callsWhileBlocked)
	}

	release()
	time.Sleep(25 * time.Millisecond)
	observerMu.Lock()
	gotObserverCalls := observerCalls
	observerMu.Unlock()
	if gotObserverCalls >= totalOutcomes {
		t.Fatalf("observer calls = %d after %d outcomes, want saturated notifications dropped", gotObserverCalls, totalOutcomes)
	}
}

func TestSchedulerCriticalPathOutcomeCallbackRunsBeforeBestEffortObserver(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "queued", testAnimation(1, time.Millisecond, 21))

	recordedEntered := make(chan struct{})
	releaseRecorded := make(chan struct{})
	observerCalled := make(chan struct{}, 1)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseRecorded)
		})
	}
	scheduler := newTestSchedulerWithReliableOutcomeRecorder(t, client, registry, SchedulerOptions{
		OnItemOutcome: func(OutcomeReport) {
			observerCalled <- struct{}{}
		},
	}, func(OutcomeReport) {
		close(recordedEntered)
		<-releaseRecorded
	})
	defer scheduler.Close()
	t.Cleanup(func() {
		release()
		if scheduler.outcomeDispatcher != nil {
			scheduler.outcomeDispatcher.wait()
		}
	})

	ctx := context.Background()
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "queued-animation",
		AnimationID:   "queued",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	waitSchedulerQueueLen(t, scheduler, 1)

	clearDone := make(chan int, 1)
	go func() {
		clearDone <- scheduler.ClearQueue()
	}()

	select {
	case <-recordedEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("critical-path outcome callback was not invoked")
	}
	select {
	case cleared := <-clearDone:
		t.Fatalf("ClearQueue() = %d before critical-path callback was released, want blocked", cleared)
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-observerCalled:
		t.Fatal("best-effort observer ran before critical-path callback returned")
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case cleared := <-clearDone:
		if cleared != 1 {
			t.Fatalf("ClearQueue() = %d, want 1", cleared)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ClearQueue() did not return after critical-path callback was released")
	}
	select {
	case <-observerCalled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("best-effort observer was not invoked after critical-path callback returned")
	}
}

func TestSchedulerCriticalPathOutcomeCallbackPanicIsCountedAndDoesNotBreakClearQueueOrObserver(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	outcomes := newOutcomeRecorder()
	now := time.Date(2026, 6, 23, 13, 14, 15, 0, time.UTC)
	scheduler := newTestSchedulerWithReliableOutcomeRecorder(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	}, func(OutcomeReport) {
		panic("recording sink failed")
	})
	defer scheduler.Close()
	t.Cleanup(func() {
		if scheduler.outcomeDispatcher != nil {
			scheduler.outcomeDispatcher.wait()
		}
	})
	ctx := context.Background()

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:   "panic-recorded-control",
			Kind: ControlClear,
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	if cleared := scheduler.ClearQueue(); cleared != 1 {
		t.Fatalf("ClearQueue() = %d, want 1", cleared)
	}
	if got := scheduler.QueueLen(); got != 0 {
		t.Fatalf("QueueLen() = %d, want 0", got)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrControlQueueCleared) {
			t.Fatalf("control error = %v, want %v", err, ErrControlQueueCleared)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("control did not complete after critical-path outcome callback panic")
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertQueueClearedOutcome(t, reports, "panic-recorded-control", OutcomeWant{
		Kind:        QueueItemControl,
		ControlKind: ControlClear,
		Depth:       1,
		Timestamp:   now,
	})
	if got := scheduler.OutcomeRecordingPanics(); got != 1 {
		t.Fatalf("OutcomeRecordingPanics() = %d, want 1", got)
	}
	health := scheduler.Health()
	if health.OutcomeRecordingPanics != 1 {
		t.Fatalf("Health().OutcomeRecordingPanics = %d, want 1", health.OutcomeRecordingPanics)
	}
	if got := scheduler.OutcomeReportsDropped(); got != 0 {
		t.Fatalf("OutcomeReportsDropped() = %d, want 0", got)
	}
	if health.OutcomeReportsDropped != 0 {
		t.Fatalf("Health().OutcomeReportsDropped = %d, want 0", health.OutcomeReportsDropped)
	}
}

func TestSchedulerOutcomeDispatcherLifecycleStopsAfterRun(t *testing.T) {
	const cycles = 10
	for i := 0; i < cycles; i++ {
		client := newFakeMatrixClient()
		registry := animations.NewRegistry()
		scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
			OnItemOutcome: func(OutcomeReport) {},
		})
		if scheduler.outcomeDispatcher == nil {
			t.Fatal("scheduler outcome dispatcher is nil")
		}

		runCtx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() {
			runDone <- scheduler.Run(runCtx)
		}()
		waitSchedulerHealth(t, scheduler, func(health Health) bool {
			return health.MatrixConnected && health.State == StateReady
		})

		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Run did not stop promptly")
		}

		waitOutcomeDispatcherStopped(t, scheduler)
	}
}

func TestSchedulerCloseStopsNeverRunOutcomeDispatcher(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: func(OutcomeReport) {},
	})
	if scheduler.outcomeDispatcher == nil {
		t.Fatal("scheduler outcome dispatcher is nil")
	}

	scheduler.Close()
	scheduler.Close()
	waitOutcomeDispatcherStopped(t, scheduler)
}

func TestSchedulerOutcomeDispatcherCloseDoesNotWaitForBlockedObserverAndDropsReports(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	var callsMu sync.Mutex
	observerCalls := 0
	release := func() {
		releaseOnce.Do(func() {
			close(releaseObserver)
		})
	}
	t.Cleanup(release)

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: func(OutcomeReport) {
			callsMu.Lock()
			observerCalls++
			callsMu.Unlock()
			enteredOnce.Do(func() {
				close(observerEntered)
			})
			<-releaseObserver
		},
	})
	if scheduler.outcomeDispatcher == nil {
		t.Fatal("scheduler outcome dispatcher is nil")
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- scheduler.Run(runCtx)
	}()
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady
	})

	if err := scheduler.EnqueueControl(runCtx, ControlRequest{
		ID:   "blocked-observer-control",
		Kind: ControlClear,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observerEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome observer was not invoked")
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run waited behind blocked outcome observer")
	}

	if accepted := scheduler.outcomeDispatcher.dispatch(OutcomeReport{
		ItemKind: QueueItemControl,
		ItemID:   "after-close",
		Outcome:  ItemOutcomeExecuted,
	}); accepted {
		t.Fatal("outcome dispatcher accepted report after scheduler shutdown")
	}
	dropsBeforeClosedReport := scheduler.OutcomeReportsDropped()
	scheduler.reportOutcome(OutcomeReport{
		ItemKind: QueueItemControl,
		ItemID:   "after-close-counted",
		Outcome:  ItemOutcomeExecuted,
	})
	if got := scheduler.OutcomeReportsDropped(); got != dropsBeforeClosedReport+1 {
		t.Fatalf("OutcomeReportsDropped() after closed dispatch = %d, want %d", got, dropsBeforeClosedReport+1)
	}

	select {
	case <-scheduler.outcomeDispatcher.doneCh():
		t.Fatal("outcome dispatcher stopped before blocked observer returned")
	default:
	}

	release()
	select {
	case <-scheduler.outcomeDispatcher.doneCh():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome dispatcher did not stop after blocked observer returned")
	}

	callsMu.Lock()
	gotCalls := observerCalls
	callsMu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("observer calls = %d, want only the pre-shutdown report", gotCalls)
	}
}

func TestSchedulerClearQueueReportsOnlyQueuedItemsWhileCurrentContinues(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(2, 120*time.Millisecond, 31))
	mustRegisterTestAnimation(t, registry, "queued", testAnimation(1, time.Millisecond, 91))

	now := time.Date(2026, 6, 23, 13, 14, 15, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current-item",
		EventID:       "event-current",
		AnimationID:   "current",
		Priority:      100,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "queued-animation",
		EventID:       "event-queued",
		AnimationID:   "queued",
		Priority:      10,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:       "queued-control",
			Kind:     ControlFill,
			Priority: 50,
			Color:    RGB{R: 7, G: 8, B: 9},
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 2)

	if cleared := scheduler.ClearQueue(); cleared != 2 {
		t.Fatalf("ClearQueue() = %d, want 2", cleared)
	}
	assertControlErrors(t, errCh, 1, ErrControlQueueCleared)

	client.waitFrames(t, 2)
	time.Sleep(30 * time.Millisecond)
	got := commandKinds(client.commands())
	want := []string{"frame:31", "frame:32"}
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want exactly %v", got, want)
		}
	}

	reports := waitOutcomeReportsMatching(t, outcomes, func(report OutcomeReport) bool {
		if report.Outcome != ItemOutcomeQueueCleared {
			return false
		}
		switch report.ItemID {
		case "queued-animation":
			return report.ItemKind == QueueItemAnimation && report.AnimationID == "queued"
		case "queued-control":
			return report.ItemKind == QueueItemControl && report.ControlKind == ControlFill
		default:
			return false
		}
	}, 2)
	assertQueueClearedOutcome(t, reports, "queued-animation", OutcomeWant{
		Kind:        QueueItemAnimation,
		EventID:     "event-queued",
		AnimationID: "queued",
		Priority:    10,
		Depth:       2,
		Timestamp:   now,
	})
	assertQueueClearedOutcome(t, reports, "queued-control", OutcomeWant{
		Kind:        QueueItemControl,
		ControlKind: ControlFill,
		Priority:    50,
		Depth:       2,
		Timestamp:   now,
	})
}

func TestSchedulerControlExecutionReportsOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	now := time.Date(2026, 6, 23, 13, 14, 15, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueControl(ctx, ControlRequest{
		ID:       "executed-control",
		Kind:     ControlFill,
		Priority: 17,
		Color:    RGB{G: 9},
	}); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertControlOutcome(t, reports, "executed-control", OutcomeWant{
		ControlKind: ControlFill,
		Priority:    17,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeExecuted, ErrorKindNone)
}

func TestSchedulerStopCompletesWaitingControl(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 250*time.Millisecond, 15))

	now := time.Date(2026, 6, 23, 14, 15, 16, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	runCtx, stop := context.WithCancel(context.Background())
	runScheduler(t, runCtx, scheduler)

	if err := scheduler.EnqueueRequest(runCtx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(context.Background(), ControlRequest{
			ID:    "stopped-control",
			Kind:  ControlFill,
			Color: RGB{R: 8},
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)
	stop()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSchedulerStopped) {
			t.Fatalf("Fill() error = %v, want %v", err, ErrSchedulerStopped)
		}
	case <-time.After(time.Second):
		t.Fatal("Fill() did not return after scheduler stopped")
	}

	reports := waitOutcomeReports(t, outcomes, 2)
	assertAnimationOutcome(t, reports, "notify", OutcomeWant{
		AnimationID: "notify",
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeSchedulerStopped, ErrorKindPermanent)
	assertControlOutcome(t, reports, "stopped-control", OutcomeWant{
		ControlKind: ControlFill,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeSchedulerStopped, ErrorKindPermanent)
}

func TestSchedulerStopReportsActiveControlOutcomeAsSchedulerStopped(t *testing.T) {
	client := newFakeMatrixClient()
	client.setCommandDelay("fill", time.Hour)
	registry := animations.NewRegistry()

	now := time.Date(2026, 6, 23, 17, 18, 19, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	runCtx, stop := context.WithCancel(context.Background())
	runScheduler(t, runCtx, scheduler)

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(context.Background(), ControlRequest{
			ID:       "active-stopped-control",
			Kind:     ControlFill,
			Priority: 18,
			Color:    RGB{R: 7},
		})
	}()
	waitClientAttempts(t, client, "fill", 1)
	stop()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSchedulerStopped) {
			t.Fatalf("Fill() error = %v, want %v", err, ErrSchedulerStopped)
		}
	case <-time.After(time.Second):
		t.Fatal("Fill() did not return after scheduler stopped")
	}

	reports := waitOutcomeReportsMatching(t, outcomes, func(report OutcomeReport) bool {
		return report.ItemKind == QueueItemControl &&
			report.ItemID == "active-stopped-control" &&
			report.ControlKind == ControlFill &&
			report.Outcome == ItemOutcomeSchedulerStopped
	}, 1)
	assertControlOutcome(t, reports, "active-stopped-control", OutcomeWant{
		ControlKind: ControlFill,
		Priority:    18,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeSchedulerStopped, ErrorKindPermanent)
	assertOutcomeCount(t, outcomes.reports(), "active-stopped-control", ItemOutcomeSchedulerStopped, 1)
}

func TestSchedulerQueuedControlExpiresAndDoesNotExecute(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 250*time.Millisecond, 20))

	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:       "expired-control",
			Kind:     ControlFill,
			Priority: 12,
			Color:    RGB{R: 9},
			Deadline: time.Now().Add(30 * time.Millisecond),
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrPlayItemExpired) {
			t.Fatalf("Fill() error = %v, want %v", err, ErrPlayItemExpired)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Fill() did not return after its deadline expired")
	}

	time.Sleep(150 * time.Millisecond)
	got := commandKinds(client.commands())
	if len(got) != 1 || got[0] != "frame:20" {
		t.Fatalf("commands = %v, want only the in-flight frame", got)
	}

	reports := waitOutcomeReportsMatching(t, outcomes, func(report OutcomeReport) bool {
		return report.ItemKind == QueueItemControl &&
			report.ItemID == "expired-control" &&
			report.ControlKind == ControlFill &&
			report.Outcome == ItemOutcomeExpired
	}, 1)
	assertControlOutcome(t, reports, "expired-control", OutcomeWant{
		ControlKind: ControlFill,
		Priority:    12,
		Depth:       1,
	}, ItemOutcomeExpired, ErrorKindPermanent)
}

func TestSchedulerQueuedControlCanceledAndDoesNotExecute(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 250*time.Millisecond, 30))

	now := time.Date(2026, 6, 23, 16, 17, 18, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	runScheduler(t, runCtx, scheduler)

	if err := scheduler.EnqueueRequest(runCtx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	controlCtx, cancelControl := context.WithCancel(runCtx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.EnqueueControl(controlCtx, ControlRequest{
			ID:       "canceled-control",
			Kind:     ControlFill,
			Priority: 13,
			Color:    RGB{R: 5},
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)
	cancelControl()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Fill() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Fill() did not return after request context cancellation")
	}

	time.Sleep(200 * time.Millisecond)
	got := commandKinds(client.commands())
	if len(got) != 1 || got[0] != "frame:30" {
		t.Fatalf("commands = %v, want only the in-flight frame", got)
	}

	reports := waitOutcomeReportsMatching(t, outcomes, func(report OutcomeReport) bool {
		return report.ItemKind == QueueItemControl &&
			report.ItemID == "canceled-control" &&
			report.ControlKind == ControlFill &&
			report.Outcome == ItemOutcomeCanceled
	}, 1)
	assertControlOutcome(t, reports, "canceled-control", OutcomeWant{
		ControlKind: ControlFill,
		Priority:    13,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeCanceled, ErrorKindPermanent)
}

func TestSchedulerControlQueueFullReportsDroppedOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	now := time.Date(2026, 6, 23, 17, 18, 19, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		QueueCapacity: 1,
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx := context.Background()

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- scheduler.EnqueueControl(ctx, ControlRequest{
			ID:   "queued-control",
			Kind: ControlClear,
		})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	err := scheduler.EnqueueControl(ctx, ControlRequest{
		ID:       "dropped-control",
		Kind:     ControlFill,
		Priority: 23,
		Color:    RGB{B: 9},
	})
	if !errors.Is(err, ErrControlDropped) || !errors.Is(err, ErrPlayQueueFull) {
		t.Fatalf("EnqueueControl() error = %v, want dropped queue full", err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertControlOutcome(t, reports, "dropped-control", OutcomeWant{
		ControlKind: ControlFill,
		Priority:    23,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeDropped, ErrorKindPermanent)

	if cleared := scheduler.ClearQueue(); cleared != 1 {
		t.Fatalf("ClearQueue() = %d, want 1", cleared)
	}
	assertControlErrors(t, firstErr, 1, ErrControlQueueCleared)
}

func TestSchedulerControlDeadlineDuringRetryReportsExpiredOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: 5 * time.Millisecond,
		ReconnectMaxDelay: 5 * time.Millisecond,
		OnItemOutcome:     outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady
	})

	client.setDisconnected(true)
	err := scheduler.EnqueueControl(ctx, ControlRequest{
		ID:       "retry-expired-control",
		Kind:     ControlFill,
		Priority: 24,
		Color:    RGB{R: 3},
		Deadline: time.Now().Add(25 * time.Millisecond),
	})
	if !errors.Is(err, ErrPlayItemExpired) {
		t.Fatalf("EnqueueControl() error = %v, want %v", err, ErrPlayItemExpired)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertControlOutcome(t, reports, "retry-expired-control", OutcomeWant{
		ControlKind: ControlFill,
		Priority:    24,
		Depth:       1,
	}, ItemOutcomeExpired, ErrorKindPermanent)
}

func TestSchedulerCanceledOrExpiredQueuedControlsReleaseCapacityBeforePop(t *testing.T) {
	tests := []struct {
		name    string
		trigger func(context.Context, *Scheduler) (<-chan error, func())
		wantErr error
	}{
		{
			name: "canceled",
			trigger: func(ctx context.Context, scheduler *Scheduler) (<-chan error, func()) {
				controlCtx, cancel := context.WithCancel(ctx)
				errCh := make(chan error, 1)
				go func() {
					errCh <- scheduler.Fill(controlCtx, RGB{R: 5})
				}()
				return errCh, cancel
			},
			wantErr: context.Canceled,
		},
		{
			name: "expired",
			trigger: func(ctx context.Context, scheduler *Scheduler) (<-chan error, func()) {
				errCh := make(chan error, 1)
				go func() {
					errCh <- scheduler.EnqueueControl(ctx, ControlRequest{
						Kind:     ControlFill,
						Color:    RGB{R: 9},
						Deadline: time.Now().Add(25 * time.Millisecond),
					})
				}()
				return errCh, func() {}
			},
			wantErr: ErrPlayItemExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeMatrixClient()
			registry := animations.NewRegistry()
			mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 180*time.Millisecond, 35))

			scheduler := newTestScheduler(t, client, registry, SchedulerOptions{QueueCapacity: 1})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runScheduler(t, ctx, scheduler)

			if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
				ID:            "notify",
				AnimationID:   "notify",
				RestorePolicy: animations.RestoreLeave,
			}); err != nil {
				t.Fatal(err)
			}
			client.waitFrames(t, 1)

			errCh, trigger := tt.trigger(ctx, scheduler)
			waitSchedulerQueueLen(t, scheduler, 1)
			trigger()

			select {
			case err := <-errCh:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("control error = %v, want %v", err, tt.wantErr)
				}
			case <-time.After(150 * time.Millisecond):
				t.Fatal("control did not return promptly after cancellation or expiry")
			}
			waitSchedulerQueueLen(t, scheduler, 0)

			validErr := make(chan error, 1)
			go func() {
				validErr <- scheduler.Fill(ctx, RGB{G: 7})
			}()
			waitSchedulerQueueLen(t, scheduler, 1)

			client.waitCommands(t, 2)
			select {
			case err := <-validErr:
				if err != nil {
					t.Fatalf("valid Fill() error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("valid Fill() did not return after scheduled execution")
			}

			got := commandKinds(client.commands())
			want := []string{"frame:35", "fill"}
			if len(got) < len(want) {
				t.Fatalf("commands = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("commands = %v, want prefix %v", got, want)
				}
			}
		})
	}
}

func TestSchedulerQueuedControlCancellationLeavesAnimationOrder(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(1, 250*time.Millisecond, 100))
	mustRegisterTestAnimation(t, registry, "low-1", testAnimation(1, time.Millisecond, 110))
	mustRegisterTestAnimation(t, registry, "high", testAnimation(1, time.Millisecond, 120))
	mustRegisterTestAnimation(t, registry, "low-2", testAnimation(1, time.Millisecond, 111))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current",
		AnimationID:   "current",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	for _, request := range []animations.AnimationRequest{
		{ID: "low-1", AnimationID: "low-1", Priority: 10, RestorePolicy: animations.RestoreLeave},
		{ID: "high", AnimationID: "high", Priority: 50, RestorePolicy: animations.RestoreLeave},
		{ID: "low-2", AnimationID: "low-2", Priority: 10, RestorePolicy: animations.RestoreLeave},
	} {
		if err := scheduler.EnqueueRequest(ctx, request); err != nil {
			t.Fatal(err)
		}
	}

	controlCtx, cancelControl := context.WithCancel(ctx)
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- scheduler.Fill(controlCtx, RGB{B: 9})
	}()
	waitSchedulerQueueLen(t, scheduler, 4)
	cancelControl()

	select {
	case err := <-controlErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled control error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("control did not return promptly after cancellation")
	}
	waitSchedulerQueueLen(t, scheduler, 3)

	client.waitCommands(t, 4)
	got := commandKinds(client.commands())
	want := []string{"frame:100", "frame:120", "frame:110", "frame:111"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestSchedulerValidControlCanEnqueueAfterCancellationBurst(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, 250*time.Millisecond, 38))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{QueueCapacity: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	for i := 0; i < 6; i++ {
		controlCtx, cancelControl := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() {
			errCh <- scheduler.Fill(controlCtx, RGB{R: 1})
		}()
		waitSchedulerQueueLen(t, scheduler, 1)
		cancelControl()
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled control %d error = %v, want %v", i, err, context.Canceled)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("canceled control %d did not return promptly", i)
		}
		waitSchedulerQueueLen(t, scheduler, 0)
	}

	validErr := make(chan error, 1)
	go func() {
		validErr <- scheduler.Fill(ctx, RGB{B: 4})
	}()
	waitSchedulerQueueLen(t, scheduler, 1)

	client.waitCommands(t, 2)
	select {
	case err := <-validErr:
		if err != nil {
			t.Fatalf("valid Fill() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("valid Fill() did not return after cancellation burst")
	}
}

func TestSchedulerControlsRunBeforeAlreadyQueuedNotifications(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(2, 30*time.Millisecond, 40))
	mustRegisterTestAnimation(t, registry, "normal-1", testAnimation(1, time.Millisecond, 50))
	mustRegisterTestAnimation(t, registry, "normal-2", testAnimation(1, time.Millisecond, 60))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current",
		AnimationID:   "current",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)

	for _, request := range []animations.AnimationRequest{
		{ID: "normal-1", AnimationID: "normal-1", Priority: 100, RestorePolicy: animations.RestoreLeave},
		{ID: "normal-2", AnimationID: "normal-2", Priority: 100, RestorePolicy: animations.RestoreLeave},
	} {
		if err := scheduler.EnqueueRequest(ctx, request); err != nil {
			t.Fatal(err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- scheduler.Fill(ctx, RGB{G: 7})
	}()

	client.waitCommands(t, 5)
	got := commandKinds(client.commands())
	want := []string{"frame:40", "frame:41", "fill", "frame:50", "frame:60"}
	if len(got) < len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want prefix %v", got, want)
		}
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Fill() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Fill() did not return after scheduled execution")
	}
}

func TestSchedulerPermanentControlErrorsAreNotRetried(t *testing.T) {
	tests := []struct {
		name         string
		request      ControlRequest
		err          error
		kind         string
		wantAttempts int
	}{
		{
			name:         "firmware status",
			request:      ControlRequest{Kind: ControlFill, Color: RGB{R: 1}},
			err:          &StatusError{Status: StatusUnknownCommand},
			kind:         "fill",
			wantAttempts: 1,
		},
		{
			name: "validation",
			request: ControlRequest{
				Kind:     ControlSetPreset,
				EffectID: 1,
				Interval: 70 * time.Second,
			},
			err:          ErrInvalidDuration,
			kind:         "preset:1",
			wantAttempts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeMatrixClient()
			client.failCommand(tt.kind, tt.err)
			registry := animations.NewRegistry()
			scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runScheduler(t, ctx, scheduler)

			err := scheduler.EnqueueControl(ctx, tt.request)
			if !errors.Is(err, tt.err) && !sameStatusError(err, tt.err) {
				t.Fatalf("EnqueueControl() error = %v, want %v", err, tt.err)
			}
			if attempts := client.attempts(tt.kind); attempts != tt.wantAttempts {
				t.Fatalf("%s attempts = %d, want %d", tt.kind, attempts, tt.wantAttempts)
			}
		})
	}
}

func TestSchedulerControlPermanentErrorsReportOutcome(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{
			name:    "status",
			err:     &StatusError{Status: StatusUnknownCommand},
			wantErr: &StatusError{Status: StatusUnknownCommand},
		},
		{
			name:    "protocol",
			err:     ErrProtocol,
			wantErr: ErrProtocol,
		},
		{
			name:    "validation",
			err:     ErrInvalidDuration,
			wantErr: ErrInvalidDuration,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeMatrixClient()
			client.failCommand("fill", tt.err)
			registry := animations.NewRegistry()

			now := time.Date(2026, 6, 23, 18, 19, 20, 0, time.UTC)
			outcomes := newOutcomeRecorder()
			scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
				Now:           func() time.Time { return now },
				OnItemOutcome: outcomes.record,
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runScheduler(t, ctx, scheduler)

			err := scheduler.EnqueueControl(ctx, ControlRequest{
				ID:       "permanent-control",
				Kind:     ControlFill,
				Priority: 29,
				Color:    RGB{R: 1},
			})
			if !errors.Is(err, tt.wantErr) && !sameStatusError(err, tt.wantErr) {
				t.Fatalf("EnqueueControl() error = %v, want %v", err, tt.wantErr)
			}

			reports := waitOutcomeReports(t, outcomes, 1)
			assertControlOutcome(t, reports, "permanent-control", OutcomeWant{
				ControlKind: ControlFill,
				Priority:    29,
				Depth:       1,
				Timestamp:   now,
			}, ItemOutcomePermanentError, ErrorKindPermanent)
		})
	}
}

func TestSchedulerInvalidControlRejectedBeforeSchedulingReportsNoOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: outcomes.record,
	})

	err := scheduler.EnqueueControl(context.Background(), ControlRequest{
		Kind: ControlKind("unsupported"),
	})
	if !errors.Is(err, ErrInvalidControl) {
		t.Fatalf("EnqueueControl() error = %v, want %v", err, ErrInvalidControl)
	}

	time.Sleep(25 * time.Millisecond)
	if reports := outcomes.reports(); len(reports) != 0 {
		t.Fatalf("invalid control outcomes = %+v, want none", reports)
	}
}

func TestSchedulerAnimationExecutionReportsOutcomeAfterRestore(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 61))

	now := time.Date(2026, 6, 23, 19, 20, 21, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "executed-animation",
		EventID:       "event-1",
		AnimationID:   "notify",
		Priority:      31,
		RestorePolicy: animations.RestoreClear,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 2)
	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "executed-animation", OutcomeWant{
		EventID:     "event-1",
		AnimationID: "notify",
		Priority:    31,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeExecuted, ErrorKindNone)
	assertOutcomeCountStable(t, outcomes, "executed-animation", ItemOutcomeExecuted, 1)
	got := commandKinds(client.commands())
	want := []string{"frame:61", "clear"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestSchedulerAnimationCompletionGuardReportsTerminalOutcomeOnce(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		outcome    ItemOutcome
		errorClass ErrorKind
	}{
		{
			name:       "executed",
			outcome:    ItemOutcomeExecuted,
			errorClass: ErrorKindNone,
		},
		{
			name:       "permanent-error",
			err:        &StatusError{Status: StatusUnknownCommand},
			outcome:    ItemOutcomePermanentError,
			errorClass: ErrorKindPermanent,
		},
		{
			name:       "canceled",
			err:        context.Canceled,
			outcome:    ItemOutcomeCanceled,
			errorClass: ErrorKindPermanent,
		},
		{
			name:       "scheduler-stopped",
			err:        ErrSchedulerStopped,
			outcome:    ItemOutcomeSchedulerStopped,
			errorClass: ErrorKindPermanent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeMatrixClient()
			registry := animations.NewRegistry()
			mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 161))

			now := time.Date(2026, 6, 24, 5, 6, 7, 0, time.UTC)
			outcomes := newOutcomeRecorder()
			scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
				Now:           func() time.Time { return now },
				OnItemOutcome: outcomes.record,
			})
			defer scheduler.Close()
			t.Cleanup(func() {
				if scheduler.outcomeDispatcher != nil {
					scheduler.outcomeDispatcher.wait()
				}
			})

			item := mustResolveTestAnimationItem(t, scheduler, context.Background(), animations.AnimationRequest{
				ID:            "guarded-" + tt.name,
				AnimationID:   "notify",
				Priority:      131,
				RestorePolicy: animations.RestoreClear,
			})

			scheduler.completeAnimationWithOutcome(item, tt.err, 1)
			scheduler.completeAnimationWithOutcome(item, errors.New("duplicate completion"), 1)

			reports := waitOutcomeReports(t, outcomes, 1)
			assertAnimationOutcome(t, reports, item.ID, OutcomeWant{
				AnimationID: "notify",
				Priority:    131,
				Depth:       1,
				Timestamp:   now,
			}, tt.outcome, tt.errorClass)
			assertOutcomeCountStable(t, outcomes, item.ID, tt.outcome, 1)
		})
	}
}

func TestSchedulerQueueClearedAnimationCompletionGuardReportsTerminalOutcomeOnce(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 162))

	now := time.Date(2026, 6, 24, 6, 7, 8, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	defer scheduler.Close()
	t.Cleanup(func() {
		if scheduler.outcomeDispatcher != nil {
			scheduler.outcomeDispatcher.wait()
		}
	})

	item := mustResolveTestAnimationItem(t, scheduler, context.Background(), animations.AnimationRequest{
		ID:            "queue-cleared-guarded",
		EventID:       "queue-cleared-event",
		AnimationID:   "notify",
		Priority:      132,
		RestorePolicy: animations.RestoreClear,
	})
	item.QueueDepthAtAdmission = 1

	scheduler.completeQueueClearedItemWithOutcome(item, 1)
	scheduler.completeAnimationWithOutcome(item, nil, 1)

	reports := waitOutcomeReports(t, outcomes, 1)
	assertQueueClearedOutcome(t, reports, item.ID, OutcomeWant{
		Kind:           QueueItemAnimation,
		EventID:        "queue-cleared-event",
		AnimationID:    "notify",
		Priority:       132,
		Depth:          1,
		AdmissionDepth: 1,
		Timestamp:      now,
	})
	assertOutcomeCountStable(t, outcomes, item.ID, ItemOutcomeQueueCleared, 1)
	assertOutcomeCount(t, outcomes.reports(), item.ID, ItemOutcomeExecuted, 0)
}

func TestSchedulerExpiredAnimationBeforeStartReportsOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 62))

	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	item := mustResolveTestAnimationItem(t, scheduler, ctx, animations.AnimationRequest{
		ID:            "expired-before-start",
		AnimationID:   "notify",
		Priority:      32,
		RestorePolicy: animations.RestoreLeave,
	})
	item.Deadline = time.Now().Add(-time.Millisecond)
	if _, _, err := scheduler.queue.enqueueScheduled(ctx, item); err != nil {
		t.Fatal(err)
	}

	runScheduler(t, ctx, scheduler)
	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "expired-before-start", OutcomeWant{
		AnimationID: "notify",
		Priority:    32,
		Depth:       1,
	}, ItemOutcomeExpired, ErrorKindPermanent)
	if got := len(client.commands()); got != 0 {
		t.Fatalf("commands = %d, want expired animation to skip playback", got)
	}
}

func TestSchedulerAnimationDeadlineDuringReconnectReportsExpiredOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 63))

	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		OnItemOutcome:     outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady
	})

	client.setDisconnected(true)
	item := mustResolveTestAnimationItem(t, scheduler, ctx, animations.AnimationRequest{
		ID:            "expired-during-playback",
		AnimationID:   "notify",
		Priority:      33,
		RestorePolicy: animations.RestoreLeave,
	})
	item.Deadline = time.Now().Add(25 * time.Millisecond)
	if _, _, err := scheduler.queue.enqueueScheduled(ctx, item); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "expired-during-playback", OutcomeWant{
		AnimationID: "notify",
		Priority:    33,
		Depth:       1,
	}, ItemOutcomeExpired, ErrorKindPermanent)
	if got := countFrames(client.commands()); got != 0 {
		t.Fatalf("frames = %d, want failed playback before any recorded frame", got)
	}
}

func TestSchedulerAnimationQueueFullReportsDroppedOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 64))

	now := time.Date(2026, 6, 23, 20, 21, 22, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		QueueCapacity: 1,
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx := context.Background()

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "queued-animation",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "dropped-animation",
		AnimationID:   "notify",
		Priority:      34,
		RestorePolicy: animations.RestoreLeave,
	})
	if !errors.Is(err, ErrPlayQueueFull) {
		t.Fatalf("EnqueueRequest() error = %v, want %v", err, ErrPlayQueueFull)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "dropped-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    34,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeDropped, ErrorKindPermanent)
	if cleared := scheduler.ClearQueue(); cleared != 1 {
		t.Fatalf("ClearQueue() = %d, want 1", cleared)
	}
}

func TestSchedulerStopReportsActiveAndQueuedAnimationOutcomesOnce(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "current", testAnimation(1, 250*time.Millisecond, 65))
	mustRegisterTestAnimation(t, registry, "queued", testAnimation(1, time.Millisecond, 66))

	now := time.Date(2026, 6, 23, 21, 22, 23, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "current",
		AnimationID:   "current",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	client.waitFrames(t, 1)
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "stopped-animation",
		AnimationID:   "queued",
		Priority:      35,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}
	waitSchedulerQueueLen(t, scheduler, 1)
	cancel()

	reports := waitOutcomeReports(t, outcomes, 2)
	assertAnimationOutcome(t, reports, "current", OutcomeWant{
		AnimationID: "current",
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeSchedulerStopped, ErrorKindPermanent)
	assertAnimationOutcome(t, reports, "stopped-animation", OutcomeWant{
		AnimationID: "queued",
		Priority:    35,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeSchedulerStopped, ErrorKindPermanent)
	assertOutcomeCountStable(t, outcomes, "current", ItemOutcomeSchedulerStopped, 1)
	assertOutcomeCountStable(t, outcomes, "stopped-animation", ItemOutcomeSchedulerStopped, 1)
	got := commandKinds(client.commands())
	if !reflect.DeepEqual(got, []string{"frame:65"}) {
		t.Fatalf("commands = %v, want only current animation frame", got)
	}
}

func TestSchedulerAnimationOnStartCancellationReportsCanceledOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 69))

	now := time.Date(2026, 6, 24, 0, 1, 2, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	item := mustResolveTestAnimationItem(t, scheduler, ctx, animations.AnimationRequest{
		ID:            "onstart-canceled-animation",
		AnimationID:   "notify",
		Priority:      38,
		RestorePolicy: animations.RestoreLeave,
	})
	item.OnStart = func(context.Context) error {
		return context.Canceled
	}
	if _, _, err := scheduler.queue.enqueueScheduled(ctx, item); err != nil {
		t.Fatal(err)
	}

	runScheduler(t, ctx, scheduler)
	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "onstart-canceled-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    38,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeCanceled, ErrorKindPermanent)
	assertOutcomeCountStable(t, outcomes, "onstart-canceled-animation", ItemOutcomeCanceled, 1)
	if got := len(client.commands()); got != 0 {
		t.Fatalf("commands = %d, want OnStart cancellation before playback", got)
	}
}

func TestSchedulerAnimationSetFrameCancellationReportsCanceledOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("frame:70", context.Canceled)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 70))

	now := time.Date(2026, 6, 24, 1, 2, 3, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "setframe-canceled-animation",
		AnimationID:   "notify",
		Priority:      39,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "setframe-canceled-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    39,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeCanceled, ErrorKindPermanent)
	if attempts := client.attempts("frame:70"); attempts != 1 {
		t.Fatalf("frame attempts = %d, want 1", attempts)
	}
}

func TestSchedulerAnimationRestoreCancellationReportsCanceledOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("clear", context.Canceled)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 71))

	now := time.Date(2026, 6, 24, 2, 3, 4, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "restore-canceled-animation",
		AnimationID:   "notify",
		Priority:      40,
		RestorePolicy: animations.RestoreClear,
	}); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "restore-canceled-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    40,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeCanceled, ErrorKindPermanent)
	got := commandKinds(client.commands())
	if !reflect.DeepEqual(got, []string{"frame:71"}) {
		t.Fatalf("commands = %v, want frame before canceled restore", got)
	}
}

func TestSchedulerAnimationReconnectWaitReadyCancellationReportsCanceledOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 72))

	now := time.Date(2026, 6, 24, 3, 4, 5, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady
	})

	client.failCommand("frame:72", net.ErrClosed)
	client.failCommand("ping", context.Canceled)
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "waitready-canceled-animation",
		AnimationID:   "notify",
		Priority:      41,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "waitready-canceled-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    41,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomeCanceled, ErrorKindPermanent)
	if attempts := client.attempts("frame:72"); attempts != 1 {
		t.Fatalf("frame attempts = %d, want 1", attempts)
	}
}

func TestSchedulerAnimationPermanentMatrixErrorReportsOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("frame:67", &StatusError{Status: StatusUnknownCommand})
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 67))

	now := time.Date(2026, 6, 23, 22, 23, 24, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(ctx)
	}()

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "permanent-matrix-animation",
		AnimationID:   "notify",
		Priority:      36,
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "permanent-matrix-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    36,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomePermanentError, ErrorKindPermanent)
	select {
	case err := <-done:
		var statusErr *StatusError
		if !errors.As(err, &statusErr) || statusErr.Status != StatusUnknownCommand {
			t.Fatalf("Run() error = %v, want status unknown command", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after permanent matrix error")
	}
	assertOutcomeCountStable(t, outcomes, "permanent-matrix-animation", ItemOutcomePermanentError, 1)
}

func TestSchedulerAnimationPermanentRestoreErrorReportsOutcome(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("clear", &StatusError{Status: StatusUnknownCommand})
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 68))

	now := time.Date(2026, 6, 23, 23, 24, 25, 0, time.UTC)
	outcomes := newOutcomeRecorder()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		Now:           func() time.Time { return now },
		OnItemOutcome: outcomes.record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(ctx)
	}()

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "permanent-restore-animation",
		AnimationID:   "notify",
		Priority:      37,
		RestorePolicy: animations.RestoreClear,
	}); err != nil {
		t.Fatal(err)
	}

	reports := waitOutcomeReports(t, outcomes, 1)
	assertAnimationOutcome(t, reports, "permanent-restore-animation", OutcomeWant{
		AnimationID: "notify",
		Priority:    37,
		Depth:       1,
		Timestamp:   now,
	}, ItemOutcomePermanentError, ErrorKindPermanent)
	select {
	case err := <-done:
		var statusErr *StatusError
		if !errors.As(err, &statusErr) || statusErr.Status != StatusUnknownCommand {
			t.Fatalf("Run() error = %v, want status unknown command", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after permanent restore error")
	}
	assertOutcomeCountStable(t, outcomes, "permanent-restore-animation", ItemOutcomePermanentError, 1)
	got := commandKinds(client.commands())
	if !reflect.DeepEqual(got, []string{"frame:68"}) {
		t.Fatalf("commands = %v, want only successful frame before restore failure", got)
	}
}

func sameStatusError(got, want error) bool {
	var gotStatus *StatusError
	var wantStatus *StatusError
	if !errors.As(got, &gotStatus) || !errors.As(want, &wantStatus) {
		return false
	}
	return gotStatus.Status == wantStatus.Status
}

func TestSchedulerRetryableControlErrorReconnectsAndRetries(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("fill", net.ErrClosed)
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.Fill(ctx, RGB{R: 2}); err != nil {
		t.Fatalf("Fill() error = %v", err)
	}
	if attempts := client.attempts("fill"); attempts != 2 {
		t.Fatalf("fill attempts = %d, want 2", attempts)
	}
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.LastFailure != nil && health.LastSuccess != nil
	})
	got := commandKinds(client.commands())
	if len(got) != 1 || got[0] != "fill" {
		t.Fatalf("commands = %v, want one successful fill", got)
	}
}

func TestSchedulerLastFailureUpdatesAfterRecoverableFramePlaybackFailure(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("frame:44", net.ErrClosed)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 44))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.LastFailure == nil
	})

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 1)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.LastFailure != nil && health.LastSuccess != nil
	})
	if attempts := client.attempts("frame:44"); attempts != 2 {
		t.Fatalf("frame attempts = %d, want 2", attempts)
	}
}

func TestSchedulerLastFailureUpdatesAfterRecoverableRestoreFailure(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("clear", net.ErrClosed)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 46))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.LastFailure == nil
	})

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreClear,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitCommands(t, 2)
	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.LastFailure != nil && health.LastSuccess != nil
	})
	if attempts := client.attempts("clear"); attempts != 2 {
		t.Fatalf("clear attempts = %d, want 2", attempts)
	}
}

func TestSchedulerRejectsExpiredControlsWithoutMatrixCommand(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueControl(ctx, ControlRequest{
		Kind:     ControlFill,
		Color:    RGB{R: 1},
		Deadline: time.Now().Add(-time.Millisecond),
	}); !errors.Is(err, ErrPlayItemExpired) {
		t.Fatalf("EnqueueControl() error = %v, want %v", err, ErrPlayItemExpired)
	}

	time.Sleep(20 * time.Millisecond)
	if got := len(client.commands()); got != 0 {
		t.Fatalf("commands = %d, want expired item to be dropped", got)
	}
}

func TestSchedulerPausesWhileMatrixDisconnected(t *testing.T) {
	client := newFakeMatrixClient()
	client.setDisconnected(true)
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 70))

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)
	if got := countFrames(client.commands()); got != 0 {
		t.Fatalf("frames while disconnected = %d, want 0", got)
	}

	client.setDisconnected(false)
	client.waitFrames(t, 1)
	if got := countFrames(client.commands()); got != 1 {
		t.Fatalf("frames after reconnect = %d, want 1", got)
	}
}

func TestSchedulerStartupRetriesTransientUnreachableNetworkPing(t *testing.T) {
	client := newStartupRetryMatrixClient(&net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &os.SyscallError{
			Syscall: "connect",
			Err:     syscall.EHOSTUNREACH,
		},
	})
	registry := animations.NewRegistry()
	scheduler, err := NewScheduler(SchedulerOptions{
		Client:            client,
		Registry:          registry,
		ReconnectMinDelay: 25 * time.Millisecond,
		ReconnectMaxDelay: 25 * time.Millisecond,
		ReconnectJitter:   noReconnectJitter,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("scheduler run error after cancel: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("scheduler did not stop after cancel")
		}
	})

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return !health.MatrixConnected &&
			health.State == StateDisconnected &&
			health.LastFailure != nil
	})

	select {
	case err := <-done:
		t.Fatalf("Run exited after retryable startup ping failure: %v", err)
	default:
	}

	client.allowPingSuccess()

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected &&
			health.State == StateReady &&
			health.LastFailure != nil &&
			health.LastSuccess != nil
	})

	if attempts := client.pingAttempts(); attempts < 2 {
		t.Fatalf("ping attempts = %d, want at least 2", attempts)
	}
}

func TestSchedulerReconnectBackoffDelaysGrowToConfiguredMax(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("ping", net.ErrClosed, net.ErrClosed, net.ErrClosed, net.ErrClosed)
	registry := animations.NewRegistry()

	var attempts []ReconnectAttempt
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: 4 * time.Millisecond,
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			attempts = append(attempts, attempt)
		},
	})

	if err := scheduler.waitReady(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	got := reconnectDelays(attempts)
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reconnect delays = %v, want %v", got, want)
	}
	if gotAttempts := reconnectAttemptNumbers(attempts); !reflect.DeepEqual(gotAttempts, []int{1, 2, 3, 4}) {
		t.Fatalf("reconnect attempt numbers = %v, want [1 2 3 4]", gotAttempts)
	}
}

func TestSchedulerReconnectJitterIsDeterministicWhenInjected(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("ping", net.ErrClosed)
	registry := animations.NewRegistry()

	var attempts []ReconnectAttempt
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: 10 * time.Millisecond,
		ReconnectMaxDelay: 20 * time.Millisecond,
		ReconnectJitter: func(base time.Duration) time.Duration {
			return base / 2
		},
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			attempts = append(attempts, attempt)
		},
	})

	if err := scheduler.waitReady(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	if len(attempts) != 1 {
		t.Fatalf("reconnect attempts = %v, want 1 attempt", attempts)
	}
	if attempts[0].Attempt != 1 || attempts[0].BaseDelay != 10*time.Millisecond || attempts[0].Delay != 5*time.Millisecond {
		t.Fatalf("reconnect attempt = %+v, want attempt 1 base 10ms delay 5ms", attempts[0])
	}
}

func TestSchedulerReconnectAttemptObservableForRetryableTransportFailure(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("ping", net.ErrClosed)
	registry := animations.NewRegistry()

	var attempts []ReconnectAttempt
	var recoveries []ReconnectRecovery
	var connectedTransitions []bool
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: 10 * time.Millisecond,
		ReconnectMaxDelay: 10 * time.Millisecond,
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			attempts = append(attempts, attempt)
		},
		OnReconnectRecovered: func(recovery ReconnectRecovery) {
			recoveries = append(recoveries, recovery)
		},
		OnMatrixConnectedChange: func(connected bool) {
			connectedTransitions = append(connectedTransitions, connected)
		},
	})

	if err := scheduler.waitReady(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	if len(attempts) != 1 {
		t.Fatalf("reconnect attempts = %v, want one retryable attempt", attempts)
	}
	attempt := attempts[0]
	if attempt.Attempt != 1 ||
		attempt.BaseDelay != 10*time.Millisecond ||
		attempt.Delay != 10*time.Millisecond ||
		attempt.DeadlineCapped ||
		attempt.ErrorKind != ErrorKindRetryable ||
		attempt.Error == "" {
		t.Fatalf("reconnect attempt = %+v, want attempt/base/delay/error details", attempt)
	}
	if len(recoveries) != 1 ||
		recoveries[0].Source != ReconnectSourceSchedulerBackoff ||
		recoveries[0].Attempt != 1 ||
		recoveries[0].State != StateReady {
		t.Fatalf("reconnect recoveries = %+v, want one scheduler ready recovery after attempt 1", recoveries)
	}
	if !reflect.DeepEqual(connectedTransitions, []bool{true}) {
		t.Fatalf("connected transitions = %v, want [true]", connectedTransitions)
	}
}

func TestSchedulerObservabilityCallbackPanicsAreRecoveredAndCounted(t *testing.T) {
	client := newFakeMatrixClient()
	client.failCommand("ping", net.ErrClosed)
	registry := animations.NewRegistry()

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: time.Millisecond,
		OnReconnectDelay: func(ReconnectAttempt) {
			panic("reconnect delay")
		},
		OnReconnectRecovered: func(ReconnectRecovery) {
			panic("reconnect recovered")
		},
		OnProbeFailure: func(ProbeFailure) {
			panic("probe failure")
		},
		OnMatrixConnectedChange: func(bool) {
			panic("connected change")
		},
	})

	if err := scheduler.waitReady(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	counts := scheduler.ObservabilityCallbackPanicCounts()
	wantCounts := map[string]uint64{
		observabilityCallbackProbeFailure:          1,
		observabilityCallbackReconnectDelay:        1,
		observabilityCallbackMatrixConnectedChange: 1,
		observabilityCallbackReconnectRecovered:    1,
	}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("observability callback panic counts = %v, want %v", counts, wantCounts)
	}
	if got := scheduler.ObservabilityCallbackPanics(); got != 4 {
		t.Fatalf("ObservabilityCallbackPanics() = %d, want 4", got)
	}
	health := scheduler.Health()
	if health.ObservabilityCallbackPanics != 4 {
		t.Fatalf("Health().ObservabilityCallbackPanics = %d, want 4", health.ObservabilityCallbackPanics)
	}
	if !reflect.DeepEqual(health.ObservabilityCallbackCounts, wantCounts) {
		t.Fatalf("Health().ObservabilityCallbackCounts = %v, want %v", health.ObservabilityCallbackCounts, wantCounts)
	}
}

func TestSchedulerReconnectFailureCallbackPanicIsRecoveredAndCounted(t *testing.T) {
	client := newFakeMatrixClient()
	client.setDisconnected(true)
	registry := animations.NewRegistry()

	reconnectDelay := make(chan struct{}, 1)
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: 100 * time.Millisecond,
		ReconnectMaxDelay: 100 * time.Millisecond,
		OnReconnectDelay: func(ReconnectAttempt) {
			select {
			case reconnectDelay <- struct{}{}:
			default:
			}
		},
		OnReconnectFailure: func(ReconnectFailure) {
			panic("reconnect failure")
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- scheduler.waitReady(ctx, time.Time{})
	}()

	select {
	case <-reconnectDelay:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnect delay callback")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitReady() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitReady did not return after cancel")
	}

	counts := scheduler.ObservabilityCallbackPanicCounts()
	if counts[observabilityCallbackReconnectFailure] != 1 {
		t.Fatalf("reconnect failure callback panic count = %d, want 1; counts = %v", counts[observabilityCallbackReconnectFailure], counts)
	}
}

func TestSchedulerPermanentErrorsDoNotEmitReconnectObservability(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "protocol", err: ErrProtocol},
		{name: "status", err: &StatusError{Status: StatusUnknownCommand}},
		{name: "validation", err: ErrInvalidDuration},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeMatrixClient()
			client.failCommand("ping", tc.err)
			registry := animations.NewRegistry()

			var attempts []ReconnectAttempt
			var recoveries []ReconnectRecovery
			var connectedTransitions []bool
			scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
				OnReconnectDelay: func(attempt ReconnectAttempt) {
					attempts = append(attempts, attempt)
				},
				OnReconnectRecovered: func(recovery ReconnectRecovery) {
					recoveries = append(recoveries, recovery)
				},
				OnMatrixConnectedChange: func(connected bool) {
					connectedTransitions = append(connectedTransitions, connected)
				},
			})

			err := scheduler.waitReady(context.Background(), time.Time{})
			if err == nil {
				t.Fatal("waitReady() error = nil, want permanent error")
			}
			if len(attempts) != 0 {
				t.Fatalf("reconnect attempts = %+v, want none for permanent error", attempts)
			}
			if len(recoveries) != 0 {
				t.Fatalf("reconnect recoveries = %+v, want none for permanent error", recoveries)
			}
			if len(connectedTransitions) != 0 {
				t.Fatalf("connected transitions = %v, want none for permanent error", connectedTransitions)
			}
		})
	}
}

func TestSchedulerReconnectBackoffResetsAfterSuccessfulPing(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()

	var attempts []ReconnectAttempt
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: 8 * time.Millisecond,
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			attempts = append(attempts, attempt)
		},
	})

	client.failCommand("ping", net.ErrClosed, net.ErrClosed)
	if err := scheduler.waitReady(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	client.failCommand("ping", net.ErrClosed)
	if err := scheduler.waitReady(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	got := reconnectAttemptNumbers(attempts)
	want := []int{1, 2, 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reconnect attempt numbers = %v, want %v", got, want)
	}
}

func TestSchedulerReconnectBackoffResetsAfterSuccessfulCommand(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: time.Millisecond,
		ReconnectMaxDelay: 8 * time.Millisecond,
	})

	if got := scheduler.incrementReconnectAttempt(); got != 1 {
		t.Fatalf("first reconnect attempt = %d, want 1", got)
	}
	if got := scheduler.incrementReconnectAttempt(); got != 2 {
		t.Fatalf("second reconnect attempt = %d, want 2", got)
	}
	scheduler.setConnected(true)
	if got := scheduler.incrementReconnectAttempt(); got != 1 {
		t.Fatalf("reconnect attempt after command success reset = %d, want 1", got)
	}
}

func TestSchedulerReconnectSleepIsCappedByItemDeadline(t *testing.T) {
	client := newFakeMatrixClient()
	client.setDisconnected(true)
	registry := animations.NewRegistry()

	var attempts []ReconnectAttempt
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ReconnectMinDelay: 100 * time.Millisecond,
		ReconnectMaxDelay: 100 * time.Millisecond,
		OnReconnectDelay: func(attempt ReconnectAttempt) {
			attempts = append(attempts, attempt)
		},
	})

	err := scheduler.waitReady(context.Background(), time.Now().Add(20*time.Millisecond))
	if !errors.Is(err, ErrPlayItemExpired) {
		t.Fatalf("waitReady() error = %v, want %v", err, ErrPlayItemExpired)
	}
	if len(attempts) == 0 {
		t.Fatal("reconnect attempts = 0, want at least one observed retry delay")
	}
	first := attempts[0]
	if !first.DeadlineCapped {
		t.Fatalf("first reconnect attempt = %+v, want deadline capped delay", first)
	}
	if first.Delay <= 0 || first.Delay > 25*time.Millisecond {
		t.Fatalf("deadline-capped delay = %s, want >0 and <=25ms", first.Delay)
	}
}

func TestSchedulerReconnectFailureOnDeadlineAndCancel(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		client := newFakeMatrixClient()
		client.setDisconnected(true)
		registry := animations.NewRegistry()

		var failures []ReconnectFailure
		scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
			ReconnectMinDelay: 20 * time.Millisecond,
			ReconnectMaxDelay: 20 * time.Millisecond,
			OnReconnectFailure: func(failure ReconnectFailure) {
				failures = append(failures, failure)
			},
		})

		err := scheduler.waitReady(context.Background(), time.Now().Add(5*time.Millisecond))
		if !errors.Is(err, ErrPlayItemExpired) {
			t.Fatalf("waitReady() error = %v, want %v", err, ErrPlayItemExpired)
		}
		if len(failures) != 1 {
			t.Fatalf("reconnect failures = %+v, want one deadline failure", failures)
		}
		failure := failures[0]
		if failure.Source != ReconnectSourceSchedulerBackoff ||
			failure.Outcome != ReconnectFailureDeadlineExceeded ||
			failure.ErrorKind != ErrorKindPermanent ||
			failure.Attempt != 1 ||
			failure.Error == "" {
			t.Fatalf("reconnect failure = %+v, want scheduler deadline failure after attempt 1", failure)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		client := newFakeMatrixClient()
		client.setDisconnected(true)
		registry := animations.NewRegistry()

		var failures []ReconnectFailure
		reconnectDelay := make(chan struct{}, 1)
		scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
			ReconnectMinDelay: 100 * time.Millisecond,
			ReconnectMaxDelay: 100 * time.Millisecond,
			OnReconnectDelay: func(ReconnectAttempt) {
				select {
				case reconnectDelay <- struct{}{}:
				default:
				}
			},
			OnReconnectFailure: func(failure ReconnectFailure) {
				failures = append(failures, failure)
			},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- scheduler.waitReady(ctx, time.Time{})
		}()

		select {
		case <-reconnectDelay:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for reconnect delay callback")
		}
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waitReady() error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("waitReady did not return after cancel")
		}

		if len(failures) != 1 {
			t.Fatalf("reconnect failures = %+v, want one canceled failure", failures)
		}
		failure := failures[0]
		if failure.Source != ReconnectSourceSchedulerBackoff ||
			failure.Outcome != ReconnectFailureCanceled ||
			failure.ErrorKind != ErrorKindPermanent ||
			failure.Attempt != 1 ||
			failure.Error == "" {
			t.Fatalf("reconnect failure = %+v, want scheduler canceled failure after attempt 1", failure)
		}
	})
}

func TestSchedulerBackoffReconnectFailureMetricLabelsForDeadlineAndCancel(t *testing.T) {
	t.Run("deadline_exceeded", func(t *testing.T) {
		client := newFakeMatrixClient()
		client.setDisconnected(true)
		registry := animations.NewRegistry()
		metricRegistry, err := metrics.New()
		if err != nil {
			t.Fatal(err)
		}

		scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
			ReconnectMinDelay: 20 * time.Millisecond,
			ReconnectMaxDelay: 20 * time.Millisecond,
			OnReconnectFailure: func(failure ReconnectFailure) {
				metricRegistry.MatrixReconnectFailuresTotal.WithLabelValues(
					string(failure.Source),
					string(failure.ErrorKind),
					string(failure.Outcome),
				).Inc()
			},
		})

		err = scheduler.waitReady(context.Background(), time.Now().Add(5*time.Millisecond))
		if !errors.Is(err, ErrPlayItemExpired) {
			t.Fatalf("waitReady() error = %v, want %v", err, ErrPlayItemExpired)
		}
		if got := matrixReconnectFailureMetricValue(t, metricRegistry,
			string(ReconnectSourceSchedulerBackoff),
			string(ErrorKindPermanent),
			string(ReconnectFailureDeadlineExceeded),
		); got != 1 {
			t.Fatalf("scheduler_backoff deadline_exceeded reconnect failure metric = %g, want 1", got)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		client := newFakeMatrixClient()
		client.setDisconnected(true)
		registry := animations.NewRegistry()
		metricRegistry, err := metrics.New()
		if err != nil {
			t.Fatal(err)
		}
		reconnectDelay := make(chan struct{}, 1)

		scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
			ReconnectMinDelay: 100 * time.Millisecond,
			ReconnectMaxDelay: 100 * time.Millisecond,
			OnReconnectDelay: func(ReconnectAttempt) {
				select {
				case reconnectDelay <- struct{}{}:
				default:
				}
			},
			OnReconnectFailure: func(failure ReconnectFailure) {
				metricRegistry.MatrixReconnectFailuresTotal.WithLabelValues(
					string(failure.Source),
					string(failure.ErrorKind),
					string(failure.Outcome),
				).Inc()
			},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- scheduler.waitReady(ctx, time.Time{})
		}()

		select {
		case <-reconnectDelay:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for reconnect delay callback")
		}
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waitReady() error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("waitReady did not return after cancel")
		}
		if got := matrixReconnectFailureMetricValue(t, metricRegistry,
			string(ReconnectSourceSchedulerBackoff),
			string(ErrorKindPermanent),
			string(ReconnectFailureCanceled),
		); got != 1 {
			t.Fatalf("scheduler_backoff canceled reconnect failure metric = %g, want 1", got)
		}
	})
}

func matrixReconnectFailureMetricValue(t *testing.T, registry *metrics.Registry, source, errorKind, outcome string) float64 {
	t.Helper()
	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "matrix_proxy_matrix_reconnect_failures_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["source"] != source || labels["error_kind"] != errorKind || labels["outcome"] != outcome {
				continue
			}
			if counter := metric.GetCounter(); counter != nil {
				return counter.GetValue()
			}
		}
	}
	return 0
}

func TestSchedulerIdleHeartbeatMarksDisconnected(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		HeartbeatInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady && health.LastSuccess != nil
	})

	client.setDisconnected(true)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return !health.MatrixConnected &&
			health.State == StateDisconnected &&
			health.LastSuccess != nil &&
			health.LastFailure != nil
	})
}

func TestSchedulerHeartbeatProbeTimeoutMetricAndBoundsQueuedStartLatency(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	mustRegisterTestAnimation(t, registry, "notify", testAnimation(1, time.Millisecond, 210))
	metricRegistry, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}

	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		ProbeTimeout:      20 * time.Millisecond,
		OnProbeFailure: func(failure ProbeFailure) {
			metricRegistry.MatrixProbeFailuresTotal.WithLabelValues(
				string(failure.ErrorKind),
				string(failure.Reason),
			).Inc()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady
	})
	initialPings := client.attempts("ping")
	client.setCommandDelay("ping", 250*time.Millisecond)
	waitClientAttempts(t, client, "ping", initialPings+1)

	start := time.Now()
	if err := scheduler.EnqueueRequest(ctx, animations.AnimationRequest{
		ID:            "notify",
		AnimationID:   "notify",
		RestorePolicy: animations.RestoreLeave,
	}); err != nil {
		t.Fatal(err)
	}

	client.waitFrames(t, 1)
	commands := client.commands()
	if len(commands) != 1 || commands[0].kind != "frame:210" {
		t.Fatalf("commands = %v, want one frame command", commandKinds(commands))
	}
	if elapsed := commands[0].at.Sub(start); elapsed > 150*time.Millisecond {
		t.Fatalf("queued frame started after %s, want bounded by probe timeout", elapsed)
	}
	if got := matrixProbeFailureMetricValue(t, metricRegistry, string(ErrorKindRetryable), string(ProbeFailureProbeTimeout)); got < 1 {
		t.Fatalf("probe timeout failure metric = %g, want at least 1", got)
	}
	if got := matrixProbeFailureMetricValue(t, metricRegistry, string(ErrorKindRetryable), string(ProbeFailureTransport)); got != 0 {
		t.Fatalf("transport probe failure metric = %g, want 0 for heartbeat probe timeout", got)
	}
}

func TestSchedulerHeartbeatProbeTimeoutMarksDisconnected(t *testing.T) {
	client := newFakeMatrixClient()
	registry := animations.NewRegistry()
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		ProbeTimeout:      15 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runScheduler(t, ctx, scheduler)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return health.MatrixConnected && health.State == StateReady && health.LastFailure == nil
	})
	client.setCommandDelay("ping", 250*time.Millisecond)

	waitSchedulerHealth(t, scheduler, func(health Health) bool {
		return !health.MatrixConnected &&
			health.State == StateDisconnected &&
			health.LastSuccess != nil &&
			health.LastFailure != nil
	})
}

func TestSchedulerHeartbeatProbeTimeoutEmitsProbeFailure(t *testing.T) {
	client := newFakeMatrixClient()
	client.setCommandDelay("ping", 250*time.Millisecond)
	registry := animations.NewRegistry()

	var failures []ProbeFailure
	scheduler := newTestScheduler(t, client, registry, SchedulerOptions{
		ProbeTimeout: 5 * time.Millisecond,
		OnProbeFailure: func(failure ProbeFailure) {
			failures = append(failures, failure)
		},
	})

	if err := scheduler.heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 {
		t.Fatalf("probe failures = %+v, want one timeout failure", failures)
	}
	failure := failures[0]
	if failure.Reason != ProbeFailureProbeTimeout ||
		failure.ErrorKind != ErrorKindRetryable ||
		failure.Error == "" {
		t.Fatalf("probe failure = %+v, want retryable probe timeout", failure)
	}
}

func matrixProbeFailureMetricValue(t *testing.T, registry *metrics.Registry, errorKind, reason string) float64 {
	t.Helper()
	families, err := registry.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "matrix_proxy_matrix_probe_failures_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["error_kind"] != errorKind || labels["reason"] != reason {
				continue
			}
			if counter := metric.GetCounter(); counter != nil {
				return counter.GetValue()
			}
		}
	}
	return 0
}

func newTestScheduler(t *testing.T, client *fakeMatrixClient, registry *animations.Registry, options SchedulerOptions) *Scheduler {
	t.Helper()
	return newTestSchedulerWithReliableOutcomeRecorder(t, client, registry, options, nil)
}

func newTestSchedulerWithReliableOutcomeRecorder(
	t *testing.T,
	client *fakeMatrixClient,
	registry *animations.Registry,
	options SchedulerOptions,
	record func(OutcomeReport),
) *Scheduler {
	t.Helper()
	options.Client = client
	options.Registry = registry
	if options.ReconnectMinDelay <= 0 && options.RetryDelay <= 0 {
		options.ReconnectMinDelay = time.Millisecond
	}
	if options.ReconnectMaxDelay <= 0 {
		options.ReconnectMaxDelay = options.ReconnectMinDelay
		if options.ReconnectMaxDelay <= 0 {
			options.ReconnectMaxDelay = options.RetryDelay
		}
	}
	if options.ReconnectJitter == nil {
		options.ReconnectJitter = noReconnectJitter
	}
	scheduler, err := newScheduler(options, record)
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func runScheduler(t *testing.T, ctx context.Context, scheduler *Scheduler) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(ctx)
	}()
	t.Cleanup(func() {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("scheduler run error: %v", err)
			}
		default:
		}
	})
}

func waitSchedulerHealth(t *testing.T, scheduler *Scheduler, predicate func(Health) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last Health
	for time.Now().Before(deadline) {
		last = scheduler.Health()
		if predicate(last) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("scheduler health = %+v did not match predicate", last)
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func waitSchedulerQueueLen(t *testing.T, scheduler *Scheduler, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if got := scheduler.QueueLen(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue length = %d, want %d", scheduler.QueueLen(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitOutcomeDispatcherStopped(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	if scheduler.outcomeDispatcher == nil {
		t.Fatal("scheduler outcome dispatcher is nil")
	}
	select {
	case <-scheduler.outcomeDispatcher.doneCh():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("outcome dispatcher did not stop")
	}
}

func waitClientAttempts(t *testing.T, client *fakeMatrixClient, kind string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if got := client.attempts(kind); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s attempts = %d, want at least %d", kind, client.attempts(kind), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitReconnectAttempt(t *testing.T, attempts <-chan ReconnectAttempt) ReconnectAttempt {
	t.Helper()
	select {
	case attempt := <-attempts:
		return attempt
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnect attempt")
		return ReconnectAttempt{}
	}
}

func waitReconnectRecovery(t *testing.T, recoveries <-chan ReconnectRecovery) ReconnectRecovery {
	t.Helper()
	select {
	case recovery := <-recoveries:
		return recovery
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnect recovery")
		return ReconnectRecovery{}
	}
}

func noReconnectJitter(base time.Duration) time.Duration {
	return base
}

func reconnectDelays(attempts []ReconnectAttempt) []time.Duration {
	delays := make([]time.Duration, len(attempts))
	for i, attempt := range attempts {
		delays[i] = attempt.Delay
	}
	return delays
}

func reconnectAttemptNumbers(attempts []ReconnectAttempt) []int {
	numbers := make([]int, len(attempts))
	for i, attempt := range attempts {
		numbers[i] = attempt.Attempt
	}
	return numbers
}

func queueIDs(items []QueueItemStatus) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func cloneQueueSnapshot(items []QueueItemStatus) []QueueItemStatus {
	cloned := make([]QueueItemStatus, len(items))
	for i, item := range items {
		cloned[i] = item
		if item.Control != nil {
			control := *item.Control
			cloned[i].Control = &control
		}
	}
	return cloned
}

func mutateQueueItemStatus(item *QueueItemStatus) {
	item.Kind = QueueItemKind("mutated-kind")
	item.ID = "mutated-id"
	item.EventID = "mutated-event"
	item.AnimationID = "mutated-animation"
	item.Priority = -999
	item.RestorePolicy = animations.RestoreBackground
	item.CreatedAt = time.Unix(1, 0).UTC()
	item.Deadline = time.Unix(2, 0).UTC()
	if item.Control != nil {
		mutateQueueControlStatus(item.Control)
	}
	item.Control = &QueueControlStatus{
		ID:         "mutated-control",
		Kind:       ControlSetPreset,
		Priority:   -1000,
		Brightness: 200,
		EffectID:   201,
		Interval:   202 * time.Millisecond,
		Color:      RGB{R: 203, G: 204, B: 205},
		CreatedAt:  time.Unix(3, 0).UTC(),
		Deadline:   time.Unix(4, 0).UTC(),
	}
}

func mutateQueueControlStatus(control *QueueControlStatus) {
	control.ID = "mutated-control"
	control.Kind = ControlClear
	control.Priority = -1000
	control.Brightness = 200
	control.EffectID = 201
	control.Interval = 202 * time.Millisecond
	control.Color = RGB{R: 203, G: 204, B: 205}
	control.CreatedAt = time.Unix(3, 0).UTC()
	control.Deadline = time.Unix(4, 0).UTC()
}

type outcomeRecorder struct {
	mu      sync.Mutex
	entries []OutcomeReport
}

type queueDepthRecorder struct {
	mu      sync.Mutex
	entries []int
}

func newQueueDepthRecorder() *queueDepthRecorder {
	return &queueDepthRecorder{}
}

func (r *queueDepthRecorder) record(depth int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, depth)
}

func (r *queueDepthRecorder) values() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make([]int, len(r.entries))
	copy(values, r.entries)
	return values
}

func newOutcomeRecorder() *outcomeRecorder {
	return &outcomeRecorder{}
}

func (r *outcomeRecorder) record(report OutcomeReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, report)
}

func (r *outcomeRecorder) reports() []OutcomeReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	reports := make([]OutcomeReport, len(r.entries))
	copy(reports, r.entries)
	return reports
}

func waitOutcomeReports(t *testing.T, recorder *outcomeRecorder, want int) []OutcomeReport {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		reports := recorder.reports()
		if len(reports) == want {
			return reports
		}
		if time.Now().After(deadline) {
			t.Fatalf("outcomes = %+v, want %d reports", reports, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitOutcomeReportsMatching(t *testing.T, recorder *outcomeRecorder, predicate func(OutcomeReport) bool, wantCount int) []OutcomeReport {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		reports := recorder.reports()
		matches := make([]OutcomeReport, 0, wantCount)
		for _, report := range reports {
			if predicate(report) {
				matches = append(matches, report)
			}
		}
		if len(matches) == wantCount {
			return matches
		}
		if len(matches) > wantCount {
			t.Fatalf("matching outcomes = %+v, want %d matches in %+v", matches, wantCount, reports)
		}
		if time.Now().After(deadline) {
			t.Fatalf("matching outcomes = %+v, want %d matches in %+v", matches, wantCount, reports)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type OutcomeWant struct {
	Kind           QueueItemKind
	EventID        string
	AnimationID    string
	ControlKind    ControlKind
	Priority       int
	Depth          int
	AdmissionDepth int
	Timestamp      time.Time
}

func assertQueueClearedOutcome(t *testing.T, reports []OutcomeReport, itemID string, want OutcomeWant) {
	t.Helper()
	report, ok := outcomeByItemID(reports, itemID)
	if !ok {
		t.Fatalf("outcomes = %+v, want report for item %q", reports, itemID)
	}
	if report.Outcome != ItemOutcomeQueueCleared {
		t.Fatalf("outcome for %q = %q, want %q", itemID, report.Outcome, ItemOutcomeQueueCleared)
	}
	if report.ItemKind != want.Kind {
		t.Fatalf("item kind for %q = %q, want %q", itemID, report.ItemKind, want.Kind)
	}
	if report.EventID != want.EventID {
		t.Fatalf("event ID for %q = %q, want %q", itemID, report.EventID, want.EventID)
	}
	if report.AnimationID != want.AnimationID {
		t.Fatalf("animation ID for %q = %q, want %q", itemID, report.AnimationID, want.AnimationID)
	}
	if report.ControlKind != want.ControlKind {
		t.Fatalf("control kind for %q = %q, want %q", itemID, report.ControlKind, want.ControlKind)
	}
	if report.Priority != want.Priority {
		t.Fatalf("priority for %q = %d, want %d", itemID, report.Priority, want.Priority)
	}
	if report.QueueDepthBeforeClear != want.Depth {
		t.Fatalf("queue depth for %q = %d, want %d", itemID, report.QueueDepthBeforeClear, want.Depth)
	}
	if report.QueueDepthAtRemoval != want.Depth {
		t.Fatalf("removal queue depth for %q = %d, want %d", itemID, report.QueueDepthAtRemoval, want.Depth)
	}
	if want.AdmissionDepth > 0 && report.QueueDepthAtAdmission != want.AdmissionDepth {
		t.Fatalf("admission queue depth for %q = %d, want %d", itemID, report.QueueDepthAtAdmission, want.AdmissionDepth)
	}
	if report.Reason != string(ItemOutcomeQueueCleared) {
		t.Fatalf("reason for %q = %q, want %q", itemID, report.Reason, ItemOutcomeQueueCleared)
	}
	if !report.Timestamp.Equal(want.Timestamp) {
		t.Fatalf("timestamp for %q = %s, want %s", itemID, report.Timestamp, want.Timestamp)
	}
}

func assertControlOutcome(t *testing.T, reports []OutcomeReport, itemID string, want OutcomeWant, outcome ItemOutcome, errorClass ErrorKind) {
	t.Helper()
	report, ok := outcomeByItemID(reports, itemID)
	if !ok {
		t.Fatalf("outcomes = %+v, want report for control %q", reports, itemID)
	}
	if report.Outcome != outcome {
		t.Fatalf("outcome for %q = %q, want %q", itemID, report.Outcome, outcome)
	}
	if report.ItemKind != QueueItemControl {
		t.Fatalf("item kind for %q = %q, want %q", itemID, report.ItemKind, QueueItemControl)
	}
	if report.ControlKind != want.ControlKind {
		t.Fatalf("control kind for %q = %q, want %q", itemID, report.ControlKind, want.ControlKind)
	}
	if report.Priority != want.Priority {
		t.Fatalf("priority for %q = %d, want %d", itemID, report.Priority, want.Priority)
	}
	if want.Depth > 0 && report.QueueDepthAtAdmission != want.Depth && report.QueueDepthAtRemoval != want.Depth {
		t.Fatalf("queue depths for %q admission=%d removal=%d, want one depth %d", itemID, report.QueueDepthAtAdmission, report.QueueDepthAtRemoval, want.Depth)
	}
	if report.Reason != string(outcome) {
		t.Fatalf("reason for %q = %q, want %q", itemID, report.Reason, outcome)
	}
	if report.ErrorClass != errorClass {
		t.Fatalf("error class for %q = %q, want %q", itemID, report.ErrorClass, errorClass)
	}
	if want.Timestamp.IsZero() {
		if report.Timestamp.IsZero() {
			t.Fatalf("timestamp for %q is zero", itemID)
		}
		return
	}
	if !report.Timestamp.Equal(want.Timestamp) {
		t.Fatalf("timestamp for %q = %s, want %s", itemID, report.Timestamp, want.Timestamp)
	}
}

func assertAnimationOutcome(t *testing.T, reports []OutcomeReport, itemID string, want OutcomeWant, outcome ItemOutcome, errorClass ErrorKind) {
	t.Helper()
	report, ok := outcomeByItemID(reports, itemID)
	if !ok {
		t.Fatalf("outcomes = %+v, want report for animation %q", reports, itemID)
	}
	if report.Outcome != outcome {
		t.Fatalf("outcome for %q = %q, want %q", itemID, report.Outcome, outcome)
	}
	if report.ItemKind != QueueItemAnimation {
		t.Fatalf("item kind for %q = %q, want %q", itemID, report.ItemKind, QueueItemAnimation)
	}
	if report.EventID != want.EventID {
		t.Fatalf("event ID for %q = %q, want %q", itemID, report.EventID, want.EventID)
	}
	if report.AnimationID != want.AnimationID {
		t.Fatalf("animation ID for %q = %q, want %q", itemID, report.AnimationID, want.AnimationID)
	}
	if report.ControlKind != "" {
		t.Fatalf("control kind for animation %q = %q, want empty", itemID, report.ControlKind)
	}
	if report.Priority != want.Priority {
		t.Fatalf("priority for %q = %d, want %d", itemID, report.Priority, want.Priority)
	}
	if want.Depth > 0 && report.QueueDepthAtAdmission != want.Depth && report.QueueDepthAtRemoval != want.Depth {
		t.Fatalf("queue depths for %q admission=%d removal=%d, want one depth %d", itemID, report.QueueDepthAtAdmission, report.QueueDepthAtRemoval, want.Depth)
	}
	if report.Reason != string(outcome) {
		t.Fatalf("reason for %q = %q, want %q", itemID, report.Reason, outcome)
	}
	if report.ErrorClass != errorClass {
		t.Fatalf("error class for %q = %q, want %q", itemID, report.ErrorClass, errorClass)
	}
	if want.Timestamp.IsZero() {
		if report.Timestamp.IsZero() {
			t.Fatalf("timestamp for %q is zero", itemID)
		}
		return
	}
	if !report.Timestamp.Equal(want.Timestamp) {
		t.Fatalf("timestamp for %q = %s, want %s", itemID, report.Timestamp, want.Timestamp)
	}
}

func outcomeByItemID(reports []OutcomeReport, itemID string) (OutcomeReport, bool) {
	for _, report := range reports {
		if report.ItemID == itemID {
			return report, true
		}
	}
	return OutcomeReport{}, false
}

func assertOutcomeCount(t *testing.T, reports []OutcomeReport, itemID string, outcome ItemOutcome, want int) {
	t.Helper()
	got := 0
	for _, report := range reports {
		if report.ItemID == itemID && report.Outcome == outcome {
			got++
		}
	}
	if got != want {
		t.Fatalf("outcome count for %q/%q = %d, want %d in %+v", itemID, outcome, got, want, reports)
	}
}

func assertOutcomeCountStable(t *testing.T, recorder *outcomeRecorder, itemID string, outcome ItemOutcome, want int) {
	t.Helper()
	time.Sleep(25 * time.Millisecond)
	assertOutcomeCount(t, recorder.reports(), itemID, outcome, want)
}

func reportsByKind(reports []OutcomeReport, kind QueueItemKind) []OutcomeReport {
	matches := make([]OutcomeReport, 0)
	for _, report := range reports {
		if report.ItemKind == kind {
			matches = append(matches, report)
		}
	}
	return matches
}

func assertControlErrors(t *testing.T, errCh <-chan error, count int, want error) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case err := <-errCh:
			if !errors.Is(err, want) {
				t.Fatalf("control error = %v, want %v", err, want)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("control did not return promptly after queue clear")
		}
	}
}

func assertQueueItemStatusExposesNoFrameOrHookFields(t *testing.T) {
	t.Helper()

	statusType := reflect.TypeOf(QueueItemStatus{})
	for _, fieldName := range []string{"Frames", "Loop", "OnStart", "OnFinish"} {
		if _, ok := statusType.FieldByName(fieldName); ok {
			t.Fatalf("QueueItemStatus exposes %s; snapshots must not expose animation internals", fieldName)
		}
	}

	frameSliceType := reflect.TypeOf([]Frame{})
	hookType := reflect.TypeOf(Hook(nil))
	controlItemPtrType := reflect.TypeOf((*ControlItem)(nil))
	for _, typ := range []reflect.Type{statusType, reflect.TypeOf(QueueControlStatus{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Type == frameSliceType {
				t.Fatalf("%s.%s exposes frame slices", typ.Name(), field.Name)
			}
			if field.Type == hookType {
				t.Fatalf("%s.%s exposes hooks", typ.Name(), field.Name)
			}
			if field.Type == controlItemPtrType {
				t.Fatalf("%s.%s exposes live control items", typ.Name(), field.Name)
			}
		}
	}
}

func mustRegisterTestAnimation(t *testing.T, registry *animations.Registry, id string, animation animations.Animation) {
	t.Helper()
	if err := registry.Register(id, animation); err != nil {
		t.Fatal(err)
	}
}

type invalidFirmwarePresetRegistry struct {
	*animations.Registry
	preset animations.FirmwarePreset
}

func (r invalidFirmwarePresetRegistry) FirmwarePreset(id string) (animations.FirmwarePreset, bool) {
	if id == "bad_rain" {
		return r.preset, true
	}
	return r.Registry.FirmwarePreset(id)
}

func testAnimation(count int, delay time.Duration, first byte) animations.Animation {
	return animations.AnimationFunc(func(context.Context, animations.Params) ([]animations.Frame, error) {
		frames := make([]animations.Frame, count)
		for i := range frames {
			frames[i] = testFrame(first+byte(i), delay)
		}
		return frames, nil
	})
}

type controlledTestAnimation struct {
	mu                sync.Mutex
	cond              *sync.Cond
	err               error
	frames            []animations.Frame
	remainingFailures int
	attempts          int
}

func newControlledTestAnimation(err error, frames ...animations.Frame) *controlledTestAnimation {
	animation := &controlledTestAnimation{
		err:    err,
		frames: append([]animations.Frame(nil), frames...),
	}
	animation.cond = sync.NewCond(&animation.mu)
	return animation
}

func (a *controlledTestAnimation) Render(context.Context, animations.Params) ([]animations.Frame, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts++
	defer a.cond.Broadcast()
	if a.remainingFailures > 0 {
		a.remainingFailures--
		return nil, a.err
	}
	return append([]animations.Frame(nil), a.frames...), nil
}

func (a *controlledTestAnimation) failNext(count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.remainingFailures += count
	a.cond.Broadcast()
}

func (a *controlledTestAnimation) waitAttempts(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.attempts < want {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("render attempts = %d, want at least %d", a.attempts, want)
		}
		timer := time.AfterFunc(remaining, func() {
			a.mu.Lock()
			a.cond.Broadcast()
			a.mu.Unlock()
		})
		a.cond.Wait()
		timer.Stop()
	}
}

func (a *controlledTestAnimation) attemptCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attempts
}

func testFrame(marker byte, delay time.Duration) animations.Frame {
	frame := animations.NewFrame(delay)
	_ = frame.SetPixel(0, 0, animations.RGB{R: marker})
	return frame
}

func mustResolveTestAnimationItem(t *testing.T, scheduler *Scheduler, ctx context.Context, request animations.AnimationRequest) ScheduledItem {
	t.Helper()
	item, err := scheduler.ResolveRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

type fakeMatrixClient struct {
	mu            sync.Mutex
	cond          *sync.Cond
	commandsLog   []fakeCommand
	failures      map[string][]error
	attemptLog    map[string]int
	commandDelays map[string]time.Duration
	inFlight      int
	maxInFlight   int
	commandDelay  time.Duration
	disconnected  bool
	recoveryCount uint64
}

type fakeCommand struct {
	kind  string
	at    time.Time
	color RGB
}

func newFakeMatrixClient() *fakeMatrixClient {
	c := &fakeMatrixClient{
		failures:      make(map[string][]error),
		attemptLog:    make(map[string]int),
		commandDelays: make(map[string]time.Duration),
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

type startupRetryMatrixClient struct {
	*fakeMatrixClient

	gateMu       sync.Mutex
	attempts     int
	firstPingErr error
	allowSuccess chan struct{}
	allowOnce    sync.Once
}

func newStartupRetryMatrixClient(firstPingErr error) *startupRetryMatrixClient {
	return &startupRetryMatrixClient{
		fakeMatrixClient: newFakeMatrixClient(),
		firstPingErr:     firstPingErr,
		allowSuccess:     make(chan struct{}),
	}
}

func (c *startupRetryMatrixClient) Ping(ctx context.Context) error {
	c.gateMu.Lock()
	c.attempts++
	attempt := c.attempts
	c.gateMu.Unlock()

	if attempt == 1 {
		return c.firstPingErr
	}

	select {
	case <-c.allowSuccess:
		return c.fakeMatrixClient.Ping(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *startupRetryMatrixClient) allowPingSuccess() {
	c.allowOnce.Do(func() {
		close(c.allowSuccess)
	})
}

func (c *startupRetryMatrixClient) pingAttempts() int {
	c.gateMu.Lock()
	defer c.gateMu.Unlock()
	return c.attempts
}

func (c *fakeMatrixClient) Ping(ctx context.Context) error {
	return c.ping(ctx)
}

func (c *fakeMatrixClient) Clear(ctx context.Context) error {
	return c.record(ctx, "clear")
}

func (c *fakeMatrixClient) SetBrightness(ctx context.Context, value byte) error {
	return c.record(ctx, "brightness")
}

func (c *fakeMatrixClient) Fill(ctx context.Context, color RGB) error {
	return c.recordColor(ctx, "fill", color)
}

func (c *fakeMatrixClient) SetPixel(ctx context.Context, x, y byte, color RGB) error {
	return c.record(ctx, "pixel")
}

func (c *fakeMatrixClient) SetFrame(ctx context.Context, frame PackedFrame) error {
	return c.record(ctx, "frame:"+stringMarker(frame[0]))
}

func (c *fakeMatrixClient) SetPanelEnabled(ctx context.Context, enabled bool) error {
	return c.record(ctx, "panel")
}

func (c *fakeMatrixClient) SetStaticColor(ctx context.Context, color RGB) error {
	return c.record(ctx, "static")
}

func (c *fakeMatrixClient) SetPreset(ctx context.Context, effectID byte, interval time.Duration, color RGB) error {
	return c.recordColor(ctx, "preset:"+stringMarker(effectID), color)
}

func (c *fakeMatrixClient) UploadCustomFrame(ctx context.Context, index, count byte, delay time.Duration, frame PackedFrame) error {
	return c.record(ctx, "upload")
}

func (c *fakeMatrixClient) StopEffect(ctx context.Context) error {
	return c.record(ctx, "stop")
}

func (c *fakeMatrixClient) record(ctx context.Context, kind string) error {
	return c.recordColor(ctx, kind, RGB{})
}

func (c *fakeMatrixClient) ping(ctx context.Context) error {
	return c.execute(ctx, "ping", func() {})
}

func (c *fakeMatrixClient) recordColor(ctx context.Context, kind string, color RGB) error {
	return c.execute(ctx, kind, func() {
		c.commandsLog = append(c.commandsLog, fakeCommand{kind: kind, at: time.Now(), color: color})
	})
}

func (c *fakeMatrixClient) execute(ctx context.Context, kind string, record func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.disconnected {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.attemptLog[kind]++
	if err := c.nextFailureLocked(kind); err != nil {
		c.cond.Broadcast()
		c.mu.Unlock()
		return err
	}
	c.inFlight++
	if c.inFlight > c.maxInFlight {
		c.maxInFlight = c.inFlight
	}
	delay := c.delayLocked(kind)
	c.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			c.mu.Lock()
			c.inFlight--
			c.cond.Broadcast()
			c.mu.Unlock()
			return ctx.Err()
		case <-timer.C:
		}
	}

	c.mu.Lock()
	record()
	c.inFlight--
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *fakeMatrixClient) delayLocked(kind string) time.Duration {
	if delay := c.commandDelays[kind]; delay > 0 {
		return delay
	}
	return c.commandDelay
}

func (c *fakeMatrixClient) setCommandDelay(kind string, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commandDelays[kind] = delay
	c.cond.Broadcast()
}

func (c *fakeMatrixClient) failCommand(kind string, errs ...error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[kind] = append(c.failures[kind], errs...)
}

func (c *fakeMatrixClient) nextFailureLocked(kind string) error {
	failures := c.failures[kind]
	if len(failures) == 0 {
		return nil
	}
	err := failures[0]
	c.failures[kind] = failures[1:]
	return err
}

func (c *fakeMatrixClient) attempts(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attemptLog[kind]
}

func (c *fakeMatrixClient) setDisconnected(disconnected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnected = disconnected
	c.cond.Broadcast()
}

func (c *fakeMatrixClient) noteReconnectRecovery() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveryCount++
	c.cond.Broadcast()
}

func (c *fakeMatrixClient) reconnectRecoveryCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recoveryCount
}

func (c *fakeMatrixClient) waitFrames(t *testing.T, count int) {
	t.Helper()
	c.waitFor(t, func() bool {
		return countFrames(c.commandsLog) >= count
	})
}

func (c *fakeMatrixClient) waitCommands(t *testing.T, count int) {
	t.Helper()
	c.waitFor(t, func() bool {
		return len(c.commandsLog) >= count
	})
}

func (c *fakeMatrixClient) waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	for !predicate() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for commands, got %v", commandKindsLocked(c.commandsLog))
		}
		timer := time.AfterFunc(remaining, func() {
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		})
		c.cond.Wait()
		timer.Stop()
	}
}

func (c *fakeMatrixClient) commands() []fakeCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	commands := make([]fakeCommand, len(c.commandsLog))
	copy(commands, c.commandsLog)
	return commands
}

func (c *fakeMatrixClient) maxConcurrent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxInFlight
}

func commandKinds(commands []fakeCommand) []string {
	kinds := make([]string, len(commands))
	for i, command := range commands {
		kinds[i] = command.kind
	}
	return kinds
}

func commandKindsLocked(commands []fakeCommand) []string {
	kinds := make([]string, len(commands))
	for i, command := range commands {
		kinds[i] = command.kind
	}
	return kinds
}

func countFrames(commands []fakeCommand) int {
	frames := 0
	for _, command := range commands {
		if len(command.kind) >= len("frame:") && command.kind[:len("frame:")] == "frame:" {
			frames++
		}
	}
	return frames
}

func stringMarker(value byte) string {
	return strconv.Itoa(int(value))
}
