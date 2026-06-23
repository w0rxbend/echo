package rules

import (
	"fmt"
	"os"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
	"gopkg.in/yaml.v3"
)

const DefaultNotificationAnimationID = "notification"

type Engine struct {
	rules []Rule
}

type File struct {
	Rules []Rule `yaml:"rules"`
}

type Rule struct {
	ID   string    `yaml:"id"`
	When Condition `yaml:"when"`
	Play Play      `yaml:"play"`
}

type Condition struct {
	Source     events.Source     `yaml:"source,omitempty"`
	Type       string            `yaml:"type,omitempty"`
	Channel    string            `yaml:"channel,omitempty"`
	Contains   string            `yaml:"contains,omitempty"`
	Attributes map[string]string `yaml:"attributes,omitempty"`
}

type Play struct {
	Animation string                   `yaml:"animation"`
	Priority  int                      `yaml:"priority,omitempty"`
	Duration  Duration                 `yaml:"duration,omitempty"`
	Interrupt animations.InterruptMode `yaml:"interrupt,omitempty"`
	Restore   animations.RestorePolicy `yaml:"restore,omitempty"`
	Params    map[string]string        `yaml:"params,omitempty"`
}

func New(rules []Rule) (*Engine, error) {
	copied := make([]Rule, len(rules))
	copy(copied, rules)

	if len(copied) == 0 {
		copied = DefaultRules()
	}

	for i := range copied {
		if err := validateRule(copied[i]); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
	}

	return &Engine{rules: copied}, nil
}

func LoadFile(path string) (*Engine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rules file: %w", err)
	}

	return Load(data)
}

func Load(data []byte) (*Engine, error) {
	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse rules file: %w", err)
	}

	return New(file.Rules)
}

func DefaultRules() []Rule {
	return []Rule{
		{
			ID: "http_notify_default",
			When: Condition{
				Source: events.SourceHTTP,
				Type:   "notify",
			},
			Play: Play{
				Animation: DefaultNotificationAnimationID,
				Priority:  50,
				Duration:  Duration{Duration: defaultNotificationDuration},
				Interrupt: animations.InterruptNone,
				Restore:   animations.RestoreBackground,
			},
		},
	}
}

func (e *Engine) Rules() []Rule {
	if e == nil {
		return nil
	}

	copied := make([]Rule, len(e.rules))
	copy(copied, e.rules)

	return copied
}

func (e *Engine) Map(event events.Event) (animations.AnimationRequest, bool) {
	if e == nil {
		return animations.AnimationRequest{}, false
	}

	for _, rule := range e.rules {
		if Matches(rule.When, event) {
			return mapRule(rule, event), true
		}
	}

	return animations.AnimationRequest{}, false
}

func validateRule(rule Rule) error {
	if rule.ID == "" {
		return fmt.Errorf("id is required")
	}
	if rule.Play.Animation == "" {
		return fmt.Errorf("play.animation is required")
	}
	if rule.Play.Duration.Duration < 0 {
		return fmt.Errorf("play.duration cannot be negative")
	}

	return nil
}
