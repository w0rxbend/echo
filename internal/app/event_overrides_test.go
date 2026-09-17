package app

import (
	"testing"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
)

// The /events and /notify routes are the only device routes without adminOnly,
// so everything applyEventOverrides reads is attacker-controlled. These cases
// pin the two limits that keep an anonymous caller from outranking, evicting, or
// cancelling work queued through the admin-only /play route.
func TestApplyEventOverridesClampsPriorityToTheMatchedRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     int
		event    int
		expected int
	}{
		{name: "escalation above the rule is ignored", rule: 50, event: 9000, expected: 50},
		{name: "equal priority is a no-op", rule: 50, event: 50, expected: 50},
		{name: "an event may lower its own priority", rule: 50, event: 10, expected: 10},
		{name: "zero means unset and keeps the rule", rule: 50, event: 0, expected: 50},
		{name: "a rule may still author a high ceiling", rule: 9000, event: 8000, expected: 8000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := animations.AnimationRequest{Priority: test.rule}
			event := events.Event{
				Priority: test.event,
				// applyEventOverrides returns early on an empty attribute map, and a
				// caller controls that trivially, so never rely on it as a guard.
				Attributes: map[string]string{"title": "build failed"},
			}

			applyEventOverrides(&request, event)

			if request.Priority != test.expected {
				t.Fatalf("priority = %d, want %d (rule %d, event %d)",
					request.Priority, test.expected, test.rule, test.event)
			}
		})
	}
}

// interrupt_mode is validated at the HTTP boundary so a typo fails loudly, but
// applying it would let an unauthenticated event overrule a rule that says
// interrupt: none -- preemption is the operator's decision.
func TestApplyEventOverridesLeavesInterruptModeToTheRule(t *testing.T) {
	t.Parallel()

	request := animations.AnimationRequest{InterruptMode: animations.InterruptNone}
	event := events.Event{Attributes: map[string]string{"interrupt_mode": "critical"}}

	applyEventOverrides(&request, event)

	if request.InterruptMode != animations.InterruptNone {
		t.Fatalf("interrupt mode = %q, want the rule's %q", request.InterruptMode, animations.InterruptNone)
	}
}
