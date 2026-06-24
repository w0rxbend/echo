package config

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

const DefaultPath = "configs/config.yaml"

// DefaultDeviceID is the device key created by Default() and by the backward-compat
// YAML loader when only the old "matrix:" + "background:" top-level keys are present.
const DefaultDeviceID = "default"

type Config struct {
	Server         ServerConfig
	Devices        map[string]*DeviceConfig
	Queue          QueueConfig
	AnimationsFile string
	RulesFile      string

	AnimationRegistry *animations.Registry `yaml:"-"`
}

type ServerConfig struct {
	Addr          string `yaml:"addr"`
	AdminTokenEnv string `yaml:"admin_token_env"`
}

// DeviceConfig holds all per-device settings: TCP connection, layout, and background.
type DeviceConfig struct {
	Host              string        `yaml:"host"`
	Port              int           `yaml:"port"`
	ConnectTimeout    time.Duration `yaml:"connect_timeout"`
	ResponseTimeout   time.Duration `yaml:"response_timeout"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	// ProbeTimeout bounds idle heartbeat probes. Probes run on the scheduler
	// selection path, so newly queued matrix work can wait up to this duration
	// behind an in-progress probe.
	ProbeTimeout      time.Duration    `yaml:"probe_timeout"`
	ReconnectMinDelay time.Duration    `yaml:"reconnect_min_delay"`
	ReconnectMaxDelay time.Duration    `yaml:"reconnect_max_delay"`
	Brightness        uint8            `yaml:"brightness"`
	Layout            LayoutConfig     `yaml:"layout"`
	Background        BackgroundConfig `yaml:"background"`
}

type LayoutConfig struct {
	Width             int    `yaml:"width"`
	Height            int    `yaml:"height"`
	Wiring            string `yaml:"wiring"`
	OddRowDisplayFlip bool   `yaml:"odd_row_display_flip"`
	// Rotation is the clockwise degrees to rotate all frame content before
	// physical LED mapping. Use 0 (default) for the standard mounting
	// orientation. Valid values: -90, 0, 90, 180.
	Rotation          int    `yaml:"rotation"`
}

type QueueConfig struct {
	EventsBuffer   int           `yaml:"events_buffer"`
	PlayBuffer     int           `yaml:"play_buffer"`
	OverflowPolicy string        `yaml:"overflow_policy"`
	DedupWindow    time.Duration `yaml:"dedup_window"`
}

type BackgroundConfig struct {
	Animation     string `yaml:"animation"`
	RestoreOnIdle bool   `yaml:"restore_on_idle"`
}

func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:          ":8080",
			AdminTokenEnv: "MATRIX_PROXY_ADMIN_TOKEN",
		},
		Devices: map[string]*DeviceConfig{
			DefaultDeviceID: DefaultDeviceConfig(),
		},
		Queue: QueueConfig{
			EventsBuffer:   512,
			PlayBuffer:     128,
			OverflowPolicy: "block",
			DedupWindow:    0,
		},
		AnimationsFile: "",
		RulesFile:      "configs/rules.example.yaml",
	}
}

func DefaultDeviceConfig() *DeviceConfig {
	return &DeviceConfig{
		Host:              "192.168.1.127",
		Port:              7777,
		ConnectTimeout:    5 * time.Second,
		ResponseTimeout:   2 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		ProbeTimeout:      2 * time.Second,
		ReconnectMinDelay: 500 * time.Millisecond,
		ReconnectMaxDelay: 10 * time.Second,
		Brightness:        30,
		Layout: LayoutConfig{
			Width:             8,
			Height:            8,
			Wiring:            "h-tl",
			OddRowDisplayFlip: true,
		},
	}
}

func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		return fmt.Errorf("server.addr must be host:port or :port: %w", err)
	}
	if c.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if len(c.Devices) == 0 {
		return errors.New("at least one device is required under devices:")
	}
	for id, device := range c.Devices {
		if err := validateDeviceID(id); err != nil {
			return err
		}
		if device == nil {
			return fmt.Errorf("device %q: nil config", id)
		}
		if err := device.Validate(id); err != nil {
			return err
		}
	}
	if c.Queue.EventsBuffer <= 0 {
		return errors.New("queue.events_buffer must be positive")
	}
	if c.Queue.PlayBuffer <= 0 {
		return errors.New("queue.play_buffer must be positive")
	}
	if c.Queue.OverflowPolicy != "block" {
		return fmt.Errorf("queue.overflow_policy must be block until non-blocking overflow is implemented: %q", c.Queue.OverflowPolicy)
	}
	if c.Queue.DedupWindow != 0 {
		return errors.New("queue.dedup_window must be 0 until deduplication is implemented")
	}
	if c.RulesFile == "" {
		return errors.New("rules_file is required")
	}
	return nil
}

func (d DeviceConfig) Validate(id string) error {
	prefix := fmt.Sprintf("device %q", id)
	if d.Host == "" {
		return fmt.Errorf("%s: host is required", prefix)
	}
	if d.Port <= 0 || d.Port > 65535 {
		return fmt.Errorf("%s: port must be between 1 and 65535: %d", prefix, d.Port)
	}
	if d.ConnectTimeout <= 0 {
		return fmt.Errorf("%s: connect_timeout must be positive", prefix)
	}
	if d.ResponseTimeout <= 0 {
		return fmt.Errorf("%s: response_timeout must be positive", prefix)
	}
	if d.HeartbeatInterval <= 0 {
		return fmt.Errorf("%s: heartbeat_interval must be positive", prefix)
	}
	if d.ProbeTimeout <= 0 {
		return fmt.Errorf("%s: probe_timeout must be positive", prefix)
	}
	if d.ReconnectMinDelay <= 0 {
		return fmt.Errorf("%s: reconnect_min_delay must be positive", prefix)
	}
	if d.ReconnectMaxDelay < d.ReconnectMinDelay {
		return fmt.Errorf("%s: reconnect_max_delay must be >= reconnect_min_delay", prefix)
	}
	if d.Layout.Width != 8 || d.Layout.Height != 8 {
		return fmt.Errorf("%s: layout width and height must be 8 for the current firmware", prefix)
	}
	if d.Layout.Wiring == "" {
		return fmt.Errorf("%s: layout.wiring is required", prefix)
	}
	validRotations := map[int]bool{-90: true, 0: true, 90: true, 180: true}
	if !validRotations[d.Layout.Rotation] {
		return fmt.Errorf("%s: layout.rotation must be one of -90, 0, 90, 180: got %d", prefix, d.Layout.Rotation)
	}
	return nil
}

func validateDeviceID(id string) error {
	if id == "" {
		return errors.New("device id cannot be empty")
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return fmt.Errorf("device id %q contains invalid character %q (only alphanumeric, hyphens, underscores allowed)", id, string(r))
		}
	}
	return nil
}
