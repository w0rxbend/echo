package rules

import (
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
)

func TestEngineExactMatch(t *testing.T) {
	engine := mustEngine(t, []byte(`
rules:
  - id: http_notify
    when:
      source: http
      type: notify
      channel: ops
    play:
      animation: notification
      priority: 25
      duration: 2s
      interrupt: higher_priority
      restore: background
      params:
        accent: cyan
`))

	request, ok := engine.Map(events.Event{
		ID:      "evt-1",
		Source:  events.SourceHTTP,
		Type:    "notify",
		Channel: "ops",
	})
	if !ok {
		t.Fatal("expected rule match")
	}
	if request.ID != "evt-1:http_notify" {
		t.Fatalf("request ID = %q, want %q", request.ID, "evt-1:http_notify")
	}
	if request.EventID != "evt-1" {
		t.Fatalf("event ID = %q, want %q", request.EventID, "evt-1")
	}
	if request.AnimationID != "notification" {
		t.Fatalf("animation ID = %q, want %q", request.AnimationID, "notification")
	}
	if request.Priority != 25 {
		t.Fatalf("priority = %d, want %d", request.Priority, 25)
	}
	if request.MaxDuration != 2*time.Second {
		t.Fatalf("duration = %s, want %s", request.MaxDuration, 2*time.Second)
	}
	if request.InterruptMode != animations.InterruptHigherPriority {
		t.Fatalf("interrupt = %q, want %q", request.InterruptMode, animations.InterruptHigherPriority)
	}
	if request.RestorePolicy != animations.RestoreBackground {
		t.Fatalf("restore = %q, want %q", request.RestorePolicy, animations.RestoreBackground)
	}
	if request.Params["accent"] != "cyan" {
		t.Fatalf("params[accent] = %q, want %q", request.Params["accent"], "cyan")
	}
	if request.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestEngineContainsMatch(t *testing.T) {
	engine := mustEngine(t, []byte(`
rules:
  - id: build_failed
    when:
      source: external
      type: message
      contains: failed
    play:
      animation: alert
      duration: 1500ms
`))

	request, ok := engine.Map(events.Event{
		Source: events.SourceExternal,
		Type:   "message",
		Text:   "Build failed on main",
	})
	if !ok {
		t.Fatal("expected contains rule match")
	}
	if request.AnimationID != "alert" {
		t.Fatalf("animation ID = %q, want %q", request.AnimationID, "alert")
	}
	if request.MaxDuration != 1500*time.Millisecond {
		t.Fatalf("duration = %s, want %s", request.MaxDuration, 1500*time.Millisecond)
	}
}

func TestEngineAttributeMatch(t *testing.T) {
	engine := mustEngine(t, []byte(`
rules:
  - id: production_deploy
    when:
      type: deploy
      attributes:
        environment: production
        result: success
    play:
      animation: deploy_ok
      priority: 60
`))

	request, ok := engine.Map(events.Event{
		Type: "deploy",
		Attributes: map[string]string{
			"environment": "production",
			"result":      "success",
		},
	})
	if !ok {
		t.Fatal("expected attribute rule match")
	}
	if request.AnimationID != "deploy_ok" {
		t.Fatalf("animation ID = %q, want %q", request.AnimationID, "deploy_ok")
	}
	if request.Priority != 60 {
		t.Fatalf("priority = %d, want %d", request.Priority, 60)
	}
	if request.RestorePolicy != animations.RestoreBackground {
		t.Fatalf("default restore = %q, want %q", request.RestorePolicy, animations.RestoreBackground)
	}
	if request.InterruptMode != animations.InterruptNone {
		t.Fatalf("default interrupt = %q, want %q", request.InterruptMode, animations.InterruptNone)
	}
}

func TestEngineNoMatch(t *testing.T) {
	engine := mustEngine(t, []byte(`
rules:
  - id: only_ops
    when:
      source: http
      type: notify
      channel: ops
    play:
      animation: notification
`))

	if _, ok := engine.Map(events.Event{
		Source:  events.SourceHTTP,
		Type:    "notify",
		Channel: "dev",
	}); ok {
		t.Fatal("expected no match")
	}
}

func TestDefaultRuleMapsHTTPNotify(t *testing.T) {
	engine, err := New(nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	request, ok := engine.Map(events.Event{
		ID:     "evt-default",
		Source: events.SourceHTTP,
		Type:   "notify",
	})
	if !ok {
		t.Fatal("expected default HTTP notify rule match")
	}
	if request.AnimationID != DefaultNotificationAnimationID {
		t.Fatalf("animation ID = %q, want %q", request.AnimationID, DefaultNotificationAnimationID)
	}
	if request.MaxDuration != 2*time.Second {
		t.Fatalf("duration = %s, want %s", request.MaxDuration, 2*time.Second)
	}
	if request.RestorePolicy != animations.RestoreBackground {
		t.Fatalf("restore = %q, want %q", request.RestorePolicy, animations.RestoreBackground)
	}
}

func mustEngine(t *testing.T, data []byte) *Engine {
	t.Helper()

	engine, err := Load(data)
	if err != nil {
		t.Fatalf("load rules: %v", err)
	}

	return engine
}
