package rules

import (
	"fmt"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
)

const defaultNotificationDuration = 2 * time.Second

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var text string
	if err := unmarshal(&text); err == nil {
		if text == "" {
			d.Duration = 0
			return nil
		}

		parsed, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", text, err)
		}
		d.Duration = parsed
		return nil
	}

	var nanos int64
	if err := unmarshal(&nanos); err != nil {
		return err
	}
	d.Duration = time.Duration(nanos)

	return nil
}

func mapRule(rule Rule, event events.Event) animations.AnimationRequest {
	interrupt := rule.Play.Interrupt
	if interrupt == "" {
		interrupt = animations.InterruptNone
	}

	restore := rule.Play.Restore
	if restore == "" {
		restore = animations.RestoreBackground
	}

	return animations.AnimationRequest{
		ID:            requestID(rule, event),
		EventID:       event.ID,
		AnimationID:   rule.Play.Animation,
		Params:        cloneParams(rule.Play.Params),
		Priority:      rule.Play.Priority,
		MaxDuration:   rule.Play.Duration.Duration,
		InterruptMode: interrupt,
		RestorePolicy: restore,
		CreatedAt:     time.Now().UTC(),
	}
}

func requestID(rule Rule, event events.Event) string {
	if event.ID == "" {
		return rule.ID
	}
	return event.ID + ":" + rule.ID
}

func cloneParams(params map[string]string) animations.Params {
	if len(params) == 0 {
		return nil
	}

	copied := make(animations.Params, len(params))
	for key, value := range params {
		copied[key] = value
	}

	return copied
}
