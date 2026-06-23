package config

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

const DefaultPath = "configs/config.yaml"

type Config struct {
	Server         ServerConfig     `yaml:"server"`
	Matrix         MatrixConfig     `yaml:"matrix"`
	Queue          QueueConfig      `yaml:"queue"`
	Background     BackgroundConfig `yaml:"background"`
	AnimationsFile string           `yaml:"animations_file"`
	RulesFile      string           `yaml:"rules_file"`

	AnimationRegistry *animations.Registry `yaml:"-"`
}

type ServerConfig struct {
	Addr          string `yaml:"addr"`
	AdminTokenEnv string `yaml:"admin_token_env"`
}

type MatrixConfig struct {
	Host              string        `yaml:"host"`
	Port              int           `yaml:"port"`
	ConnectTimeout    time.Duration `yaml:"connect_timeout"`
	ResponseTimeout   time.Duration `yaml:"response_timeout"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	// ProbeTimeout bounds idle heartbeat probes. Probes run on the scheduler
	// selection path, so newly queued matrix work can wait up to this duration
	// behind an in-progress probe.
	ProbeTimeout      time.Duration `yaml:"probe_timeout"`
	ReconnectMinDelay time.Duration `yaml:"reconnect_min_delay"`
	ReconnectMaxDelay time.Duration `yaml:"reconnect_max_delay"`
	Brightness        uint8         `yaml:"brightness"`
	Layout            LayoutConfig  `yaml:"layout"`
}

type LayoutConfig struct {
	Width             int    `yaml:"width"`
	Height            int    `yaml:"height"`
	Wiring            string `yaml:"wiring"`
	OddRowDisplayFlip bool   `yaml:"odd_row_display_flip"`
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
		Matrix: MatrixConfig{
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
		},
		Queue: QueueConfig{
			EventsBuffer:   512,
			PlayBuffer:     128,
			OverflowPolicy: "block",
			DedupWindow:    0,
		},
		Background:     BackgroundConfig{},
		AnimationsFile: "",
		RulesFile:      "configs/rules.example.yaml",
	}
}

func (c Config) Validate() error {
	if c.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		return fmt.Errorf("server.addr must be host:port or :port: %w", err)
	}
	if c.Matrix.Host == "" {
		return errors.New("matrix.host is required")
	}
	if c.Matrix.Port <= 0 || c.Matrix.Port > 65535 {
		return fmt.Errorf("matrix.port must be between 1 and 65535: %d", c.Matrix.Port)
	}
	if c.Matrix.ConnectTimeout <= 0 {
		return errors.New("matrix.connect_timeout must be positive")
	}
	if c.Matrix.ResponseTimeout <= 0 {
		return errors.New("matrix.response_timeout must be positive")
	}
	if c.Matrix.HeartbeatInterval <= 0 {
		return errors.New("matrix.heartbeat_interval must be positive")
	}
	if c.Matrix.ProbeTimeout <= 0 {
		return errors.New("matrix.probe_timeout must be positive")
	}
	if c.Matrix.ReconnectMinDelay <= 0 {
		return errors.New("matrix.reconnect_min_delay must be positive")
	}
	if c.Matrix.ReconnectMaxDelay < c.Matrix.ReconnectMinDelay {
		return errors.New("matrix.reconnect_max_delay must be greater than or equal to reconnect_min_delay")
	}
	if c.Matrix.Layout.Width != 8 || c.Matrix.Layout.Height != 8 {
		return errors.New("matrix.layout width and height must be 8 for the current firmware")
	}
	if c.Matrix.Layout.Wiring == "" {
		return errors.New("matrix.layout.wiring is required")
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
