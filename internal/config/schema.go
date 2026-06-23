package config

import (
	"fmt"
	"time"
)

type schemaConfig struct {
	Server         schemaServerConfig     `yaml:"server"`
	Matrix         schemaMatrixConfig     `yaml:"matrix"`
	Queue          schemaQueueConfig      `yaml:"queue"`
	Background     schemaBackgroundConfig `yaml:"background"`
	AnimationsFile *string                `yaml:"animations_file"`
	RulesFile      *string                `yaml:"rules_file"`
}

type schemaServerConfig struct {
	Addr          *string `yaml:"addr"`
	AdminTokenEnv *string `yaml:"admin_token_env"`
}

type schemaMatrixConfig struct {
	Host              *string         `yaml:"host"`
	Port              *int            `yaml:"port"`
	ConnectTimeout    *schemaDuration `yaml:"connect_timeout"`
	ResponseTimeout   *schemaDuration `yaml:"response_timeout"`
	HeartbeatInterval *schemaDuration `yaml:"heartbeat_interval"`
	ProbeTimeout      *schemaDuration `yaml:"probe_timeout"`
	ReconnectMinDelay *schemaDuration `yaml:"reconnect_min_delay"`
	ReconnectMaxDelay *schemaDuration `yaml:"reconnect_max_delay"`
	Brightness        *uint8          `yaml:"brightness"`
	Layout            schemaLayout    `yaml:"layout"`
}

type schemaLayout struct {
	Width             *int    `yaml:"width"`
	Height            *int    `yaml:"height"`
	Wiring            *string `yaml:"wiring"`
	OddRowDisplayFlip *bool   `yaml:"odd_row_display_flip"`
}

type schemaQueueConfig struct {
	EventsBuffer   *int            `yaml:"events_buffer"`
	PlayBuffer     *int            `yaml:"play_buffer"`
	OverflowPolicy *string         `yaml:"overflow_policy"`
	DedupWindow    *schemaDuration `yaml:"dedup_window"`
}

type schemaBackgroundConfig struct {
	Animation     *string `yaml:"animation"`
	RestoreOnIdle *bool   `yaml:"restore_on_idle"`
}

type schemaDuration struct {
	time.Duration
}

func (d *schemaDuration) UnmarshalYAML(unmarshal func(any) error) error {
	var text string
	if err := unmarshal(&text); err == nil {
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

func (s schemaConfig) apply(cfg *Config) error {
	if s.Server.Addr != nil {
		cfg.Server.Addr = *s.Server.Addr
	}
	if s.Server.AdminTokenEnv != nil {
		cfg.Server.AdminTokenEnv = *s.Server.AdminTokenEnv
	}
	if s.Matrix.Host != nil {
		cfg.Matrix.Host = *s.Matrix.Host
	}
	if s.Matrix.Port != nil {
		cfg.Matrix.Port = *s.Matrix.Port
	}
	if s.Matrix.ConnectTimeout != nil {
		cfg.Matrix.ConnectTimeout = s.Matrix.ConnectTimeout.Duration
	}
	if s.Matrix.ResponseTimeout != nil {
		cfg.Matrix.ResponseTimeout = s.Matrix.ResponseTimeout.Duration
	}
	if s.Matrix.HeartbeatInterval != nil {
		cfg.Matrix.HeartbeatInterval = s.Matrix.HeartbeatInterval.Duration
	}
	if s.Matrix.ProbeTimeout != nil {
		cfg.Matrix.ProbeTimeout = s.Matrix.ProbeTimeout.Duration
	}
	if s.Matrix.ReconnectMinDelay != nil {
		cfg.Matrix.ReconnectMinDelay = s.Matrix.ReconnectMinDelay.Duration
	}
	if s.Matrix.ReconnectMaxDelay != nil {
		cfg.Matrix.ReconnectMaxDelay = s.Matrix.ReconnectMaxDelay.Duration
	}
	if s.Matrix.Brightness != nil {
		cfg.Matrix.Brightness = *s.Matrix.Brightness
	}
	if s.Matrix.Layout.Width != nil {
		cfg.Matrix.Layout.Width = *s.Matrix.Layout.Width
	}
	if s.Matrix.Layout.Height != nil {
		cfg.Matrix.Layout.Height = *s.Matrix.Layout.Height
	}
	if s.Matrix.Layout.Wiring != nil {
		cfg.Matrix.Layout.Wiring = *s.Matrix.Layout.Wiring
	}
	if s.Matrix.Layout.OddRowDisplayFlip != nil {
		cfg.Matrix.Layout.OddRowDisplayFlip = *s.Matrix.Layout.OddRowDisplayFlip
	}
	if s.Queue.EventsBuffer != nil {
		cfg.Queue.EventsBuffer = *s.Queue.EventsBuffer
	}
	if s.Queue.PlayBuffer != nil {
		cfg.Queue.PlayBuffer = *s.Queue.PlayBuffer
	}
	if s.Queue.OverflowPolicy != nil {
		cfg.Queue.OverflowPolicy = *s.Queue.OverflowPolicy
	}
	if s.Queue.DedupWindow != nil {
		cfg.Queue.DedupWindow = s.Queue.DedupWindow.Duration
	}
	if s.Background.Animation != nil {
		cfg.Background.Animation = *s.Background.Animation
	}
	if s.Background.RestoreOnIdle != nil {
		cfg.Background.RestoreOnIdle = *s.Background.RestoreOnIdle
	}
	if s.AnimationsFile != nil {
		cfg.AnimationsFile = *s.AnimationsFile
	}
	if s.RulesFile != nil {
		cfg.RulesFile = *s.RulesFile
	}

	return nil
}
