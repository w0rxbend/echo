package config

import (
	"errors"
	"fmt"
	"time"
)

// schemaConfig is the YAML-decoded intermediate representation.
// It supports both the legacy single-device format (matrix: / background: top-level keys)
// and the new multi-device format (devices: map). The two formats are mutually exclusive.
type schemaConfig struct {
	Server         schemaServerConfig                `yaml:"server"`
	// Legacy single-device keys — still accepted for backward compatibility.
	Matrix         *schemaDeviceConfig               `yaml:"matrix"`
	Background     *schemaBackgroundConfig           `yaml:"background"`
	// New multi-device key.
	Devices        map[string]*schemaDeviceConfig    `yaml:"devices"`
	Queue          schemaQueueConfig                 `yaml:"queue"`
	AnimationsFile *string                           `yaml:"animations_file"`
	RulesFile      *string                           `yaml:"rules_file"`
}

type schemaServerConfig struct {
	Addr          *string `yaml:"addr"`
	AdminTokenEnv *string `yaml:"admin_token_env"`
}

// schemaDeviceConfig is shared by both the legacy "matrix:" key and each entry in "devices:".
type schemaDeviceConfig struct {
	Host              *string               `yaml:"host"`
	Port              *int                  `yaml:"port"`
	ConnectTimeout    *schemaDuration       `yaml:"connect_timeout"`
	ResponseTimeout   *schemaDuration       `yaml:"response_timeout"`
	HeartbeatInterval *schemaDuration       `yaml:"heartbeat_interval"`
	ProbeTimeout      *schemaDuration       `yaml:"probe_timeout"`
	ReconnectMinDelay *schemaDuration       `yaml:"reconnect_min_delay"`
	ReconnectMaxDelay *schemaDuration       `yaml:"reconnect_max_delay"`
	Brightness        *uint8                `yaml:"brightness"`
	Layout            schemaLayout          `yaml:"layout"`
	Background        *schemaBackgroundConfig `yaml:"background"`
}

type schemaLayout struct {
	Width             *int    `yaml:"width"`
	Height            *int    `yaml:"height"`
	Wiring            *string `yaml:"wiring"`
	OddRowDisplayFlip *bool   `yaml:"odd_row_display_flip"`
	Rotation          *int    `yaml:"rotation"`
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

	// Mutual exclusion: devices: and matrix: cannot coexist.
	if len(s.Devices) > 0 && s.Matrix != nil {
		return errors.New("cannot use both 'devices:' and 'matrix:' in config; use 'devices:' for multi-device")
	}

	if len(s.Devices) > 0 {
		// New multi-device format — replace the default devices map entirely.
		cfg.Devices = make(map[string]*DeviceConfig, len(s.Devices))
		for id, sd := range s.Devices {
			device := DefaultDeviceConfig()
			if sd != nil {
				sd.apply(device)
				// Per-device background comes from the device entry's "background:" sub-key.
				if sd.Background != nil {
					sd.Background.apply(&device.Background)
				}
			}
			cfg.Devices[id] = device
		}
	} else {
		// Legacy single-device format — patch the "default" device.
		if cfg.Devices == nil {
			cfg.Devices = map[string]*DeviceConfig{DefaultDeviceID: DefaultDeviceConfig()}
		}
		def := cfg.Devices[DefaultDeviceID]
		if def == nil {
			def = DefaultDeviceConfig()
			cfg.Devices[DefaultDeviceID] = def
		}
		if s.Matrix != nil {
			s.Matrix.apply(def)
		}
		if s.Background != nil {
			s.Background.apply(&def.Background)
		}
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
	if s.AnimationsFile != nil {
		cfg.AnimationsFile = *s.AnimationsFile
	}
	if s.RulesFile != nil {
		cfg.RulesFile = *s.RulesFile
	}

	return nil
}

// apply patches a DeviceConfig with the non-nil fields from the schema.
// Background is NOT applied here because it lives on a sub-key; callers handle it.
func (s *schemaDeviceConfig) apply(d *DeviceConfig) {
	if s == nil {
		return
	}
	if s.Host != nil {
		d.Host = *s.Host
	}
	if s.Port != nil {
		d.Port = *s.Port
	}
	if s.ConnectTimeout != nil {
		d.ConnectTimeout = s.ConnectTimeout.Duration
	}
	if s.ResponseTimeout != nil {
		d.ResponseTimeout = s.ResponseTimeout.Duration
	}
	if s.HeartbeatInterval != nil {
		d.HeartbeatInterval = s.HeartbeatInterval.Duration
	}
	if s.ProbeTimeout != nil {
		d.ProbeTimeout = s.ProbeTimeout.Duration
	}
	if s.ReconnectMinDelay != nil {
		d.ReconnectMinDelay = s.ReconnectMinDelay.Duration
	}
	if s.ReconnectMaxDelay != nil {
		d.ReconnectMaxDelay = s.ReconnectMaxDelay.Duration
	}
	if s.Brightness != nil {
		d.Brightness = *s.Brightness
	}
	if s.Layout.Width != nil {
		d.Layout.Width = *s.Layout.Width
	}
	if s.Layout.Height != nil {
		d.Layout.Height = *s.Layout.Height
	}
	if s.Layout.Wiring != nil {
		d.Layout.Wiring = *s.Layout.Wiring
	}
	if s.Layout.OddRowDisplayFlip != nil {
		d.Layout.OddRowDisplayFlip = *s.Layout.OddRowDisplayFlip
	}
	if s.Layout.Rotation != nil {
		d.Layout.Rotation = *s.Layout.Rotation
	}
}

func (s *schemaBackgroundConfig) apply(b *BackgroundConfig) {
	if s == nil {
		return
	}
	if s.Animation != nil {
		b.Animation = *s.Animation
	}
	if s.RestoreOnIdle != nil {
		b.RestoreOnIdle = *s.RestoreOnIdle
	}
}
