package events

import "context"

type EventSource interface {
	Name() string
	Run(ctx context.Context, publish func(Event) error) error
}
