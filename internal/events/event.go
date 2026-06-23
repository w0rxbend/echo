package events

import "time"

type Source string

const (
	SourceHTTP     Source = "http"
	SourceExternal Source = "external"
)

type Event struct {
	ID         string            `json:"id" yaml:"id"`
	Source     Source            `json:"source" yaml:"source"`
	Type       string            `json:"type" yaml:"type"`
	Actor      string            `json:"actor,omitempty" yaml:"actor,omitempty"`
	Text       string            `json:"text,omitempty" yaml:"text,omitempty"`
	Channel    string            `json:"channel,omitempty" yaml:"channel,omitempty"`
	ReceivedAt time.Time         `json:"received_at" yaml:"received_at"`
	Priority   int               `json:"priority,omitempty" yaml:"priority,omitempty"`
	DedupKey   string            `json:"dedup_key,omitempty" yaml:"dedup_key,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
}
