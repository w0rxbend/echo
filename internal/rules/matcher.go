package rules

import (
	"strings"

	"github.com/worxbend/echo/internal/events"
)

func Matches(condition Condition, event events.Event) bool {
	if condition.Source != "" && event.Source != condition.Source {
		return false
	}
	if condition.Type != "" && event.Type != condition.Type {
		return false
	}
	if condition.Channel != "" && event.Channel != condition.Channel {
		return false
	}
	if condition.Contains != "" && !strings.Contains(event.Text, condition.Contains) {
		return false
	}
	for key, expected := range condition.Attributes {
		if event.Attributes == nil {
			return false
		}
		if event.Attributes[key] != expected {
			return false
		}
	}

	return true
}
