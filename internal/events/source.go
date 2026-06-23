package events

import "context"

const (
	SourceTwitch   Source = "twitch"
	SourceYouTube  Source = "youtube"
	SourceTelegram Source = "telegram"
	SourceDiscord  Source = "discord"
)

type EventSource interface {
	Name() string
	Run(ctx context.Context, publish func(Event) error) error
}
