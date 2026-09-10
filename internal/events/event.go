package events

import "time"

type Source string

// All Source values live in this one block, beside the type. Splitting them across
// files made the generated OpenAPI enum order depend on swag's file ordering, so
// the spec-drift check in CI could fail on an unchanged tree.
const (
	SourceHTTP     Source = "http"
	SourceExternal Source = "external"
	SourceTwitch   Source = "twitch"
	SourceYouTube  Source = "youtube"
	SourceTelegram Source = "telegram"
	SourceDiscord  Source = "discord"
)

type Event struct {
	ID     string `json:"id" yaml:"id"`
	Source Source `json:"source" yaml:"source"`
	Type   string `json:"type" yaml:"type"`
	// Target is the device ID this event is routed to. Set automatically from
	// the URL path parameter when submitted via /api/v1/devices/{device}/events.
	Target     string            `json:"target,omitempty" yaml:"target,omitempty"`
	Actor      string            `json:"actor,omitempty" yaml:"actor,omitempty"`
	Text       string            `json:"text,omitempty" yaml:"text,omitempty"`
	Channel    string            `json:"channel,omitempty" yaml:"channel,omitempty"`
	ReceivedAt time.Time         `json:"received_at" yaml:"received_at"`
	Priority   int               `json:"priority,omitempty" yaml:"priority,omitempty"`
	DedupKey   string            `json:"dedup_key,omitempty" yaml:"dedup_key,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
}
