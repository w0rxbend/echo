package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
)

func TestDefaultConfigValid(t *testing.T) {
	cfg := Default()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() error = %v", err)
	}
	if cfg.Matrix.HeartbeatInterval <= 0 {
		t.Fatalf("default matrix.heartbeat_interval = %s, want positive", cfg.Matrix.HeartbeatInterval)
	}
	if cfg.Matrix.ProbeTimeout <= 0 {
		t.Fatalf("default matrix.probe_timeout = %s, want positive", cfg.Matrix.ProbeTimeout)
	}
	if cfg.Queue.OverflowPolicy != "block" {
		t.Fatalf("default queue.overflow_policy = %q, want block", cfg.Queue.OverflowPolicy)
	}
	if cfg.Queue.DedupWindow != 0 {
		t.Fatalf("default queue.dedup_window = %s, want 0", cfg.Queue.DedupWindow)
	}
	if cfg.AnimationsFile != "" {
		t.Fatalf("default animations_file = %q, want empty", cfg.AnimationsFile)
	}
	if cfg.Background.Animation != "" || cfg.Background.RestoreOnIdle {
		t.Fatalf("default background = %+v, want disabled until configured", cfg.Background)
	}
}

func TestLoadParsesMatrixSchedulerDurations(t *testing.T) {
	rulesPath := writeFile(t, "rules.yaml", validRules("notification"))
	path := writeConfig(t, `
rules_file: "`+rulesPath+`"
matrix:
  heartbeat_interval: 750ms
  probe_timeout: 1500ms
queue:
  overflow_policy: block
  dedup_window: 0s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Matrix.HeartbeatInterval != 750*time.Millisecond {
		t.Fatalf("matrix.heartbeat_interval = %s, want 750ms", cfg.Matrix.HeartbeatInterval)
	}
	if cfg.Matrix.ProbeTimeout != 1500*time.Millisecond {
		t.Fatalf("matrix.probe_timeout = %s, want 1500ms", cfg.Matrix.ProbeTimeout)
	}
}

func TestValidateRejectsInvalidHeartbeatAndProbeDurations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "zero heartbeat",
			mutate: func(cfg *Config) {
				cfg.Matrix.HeartbeatInterval = 0
			},
			wantErr: "matrix.heartbeat_interval must be positive",
		},
		{
			name: "negative heartbeat",
			mutate: func(cfg *Config) {
				cfg.Matrix.HeartbeatInterval = -time.Second
			},
			wantErr: "matrix.heartbeat_interval must be positive",
		},
		{
			name: "zero probe",
			mutate: func(cfg *Config) {
				cfg.Matrix.ProbeTimeout = 0
			},
			wantErr: "matrix.probe_timeout must be positive",
		},
		{
			name: "negative probe",
			mutate: func(cfg *Config) {
				cfg.Matrix.ProbeTimeout = -time.Second
			},
			wantErr: "matrix.probe_timeout must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsInvalidReconnectBounds(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "nonpositive min",
			mutate: func(cfg *Config) {
				cfg.Matrix.ReconnectMinDelay = 0
			},
			wantErr: "matrix.reconnect_min_delay must be positive",
		},
		{
			name: "max less than min",
			mutate: func(cfg *Config) {
				cfg.Matrix.ReconnectMinDelay = time.Second
				cfg.Matrix.ReconnectMaxDelay = time.Second - time.Nanosecond
			},
			wantErr: "matrix.reconnect_max_delay must be greater than or equal to reconnect_min_delay",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsUnsupportedOverflowPolicies(t *testing.T) {
	for _, policy := range []string{"", "drop_oldest", "drop_low_priority"} {
		t.Run(policy, func(t *testing.T) {
			cfg := Default()
			cfg.Queue.OverflowPolicy = policy

			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "queue.overflow_policy must be block") {
				t.Fatalf("Validate() error = %v, want unsupported overflow policy error", err)
			}
		})
	}
}

func TestValidateRejectsNonzeroDedupWindow(t *testing.T) {
	for _, window := range []time.Duration{time.Nanosecond, time.Second, -time.Second} {
		t.Run(window.String(), func(t *testing.T) {
			cfg := Default()
			cfg.Queue.DedupWindow = window

			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "queue.dedup_window must be 0") {
				t.Fatalf("Validate() error = %v, want nonzero dedup window error", err)
			}
		})
	}
}

func TestLoadMergesConfiguredAnimationsWithBuiltins(t *testing.T) {
	animationsPath := writeFile(t, "animations.yaml", `
animations:
  alert_pulse:
    type: generated
    generator: notification
  matrix_rain_background:
    type: firmware_preset
    effect_id: 12
    interval: 90ms
    color: "#00FF55"
`)
	rulesPath := writeFile(t, "rules.yaml", `
rules:
  - id: alert
    when:
      source: http
      type: notify
    play:
      animation: alert_pulse
      duration: 2s
`)
	path := writeConfig(t, `
animations_file: "`+animationsPath+`"
rules_file: "`+rulesPath+`"
queue:
  overflow_policy: block
  dedup_window: 0s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.AnimationRegistry == nil {
		t.Fatal("Load() returned nil animation registry")
	}
	if _, ok := cfg.AnimationRegistry.Get(animations.NotificationAnimationID); !ok {
		t.Fatalf("merged registry missing built-in %q", animations.NotificationAnimationID)
	}
	if _, ok := cfg.AnimationRegistry.Get("alert_pulse"); !ok {
		t.Fatal("merged registry missing configured generated animation")
	}
	preset, ok := cfg.AnimationRegistry.FirmwarePreset("matrix_rain_background")
	if !ok {
		t.Fatal("merged registry missing configured firmware preset")
	}
	if preset.EffectID != 12 || preset.Interval != 90*time.Millisecond || preset.Color != (animations.RGB{G: 255, B: 85}) {
		t.Fatalf("firmware preset = %+v, want effect 12 interval 90ms color #00FF55", preset)
	}
	if !cfg.AnimationRegistry.Has("matrix_rain_background") {
		t.Fatal("merged registry does not report firmware preset ID")
	}
	if _, ok := cfg.AnimationRegistry.Get("matrix_rain_background"); ok {
		t.Fatal("firmware preset should not be returned as a renderable generated animation")
	}
}

func TestLoadRejectsUnknownAnimationType(t *testing.T) {
	err := loadWithAnimations(t, `
animations:
  wave:
    type: frames
`, validRules("notification"))
	if err == nil || !strings.Contains(err.Error(), `unknown animation type "frames"`) {
		t.Fatalf("Load() error = %v, want unknown animation type", err)
	}
}

func TestLoadRejectsUnknownGeneratedAnimationGenerator(t *testing.T) {
	err := loadWithAnimations(t, `
animations:
  alert:
    type: generated
    generator: sparkle
`, validRules("notification"))
	if err == nil || !strings.Contains(err.Error(), `unknown animation generator "sparkle"`) {
		t.Fatalf("Load() error = %v, want unknown generator", err)
	}
}

func TestLoadRejectsDuplicateAnimationID(t *testing.T) {
	err := loadWithAnimations(t, `
animations:
  notification:
    type: generated
    generator: notification
`, validRules("notification"))
	if err == nil || !strings.Contains(err.Error(), "animation already registered: notification") {
		t.Fatalf("Load() error = %v, want duplicate animation ID", err)
	}
}

func TestLoadRejectsInvalidFirmwarePresetBounds(t *testing.T) {
	tests := []struct {
		name      string
		animation string
		wantErr   string
	}{
		{
			name: "effect too large",
			animation: `
animations:
  rain:
    type: firmware_preset
    effect_id: 256
    interval: 90ms
`,
			wantErr: "effect_id must be between 0 and 255: 256",
		},
		{
			name: "negative interval",
			animation: `
animations:
  rain:
    type: firmware_preset
    effect_id: 12
    interval: -1ms
`,
			wantErr: "firmware preset interval cannot be negative",
		},
		{
			name: "interval too large",
			animation: `
animations:
  rain:
    type: firmware_preset
    effect_id: 12
    interval: 65536ms
`,
			wantErr: "firmware preset interval must be <= 65535ms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadWithAnimations(t, tt.animation, validRules("notification"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsUnknownRuleAnimationReference(t *testing.T) {
	err := loadWithAnimations(t, `
animations:
  alert:
    type: generated
    generator: notification
`, validRules("missing_animation"))
	if err == nil || !strings.Contains(err.Error(), `rule "alert" references unknown animation "missing_animation"`) {
		t.Fatalf("Load() error = %v, want unknown animation reference", err)
	}
}

func TestLoadRejectsNonRenderableRuleAnimationReference(t *testing.T) {
	err := loadWithAnimations(t, `
animations:
  matrix_rain_background:
    type: firmware_preset
    effect_id: 12
    interval: 90ms
    color: "#00FF55"
`, validRules("matrix_rain_background"))
	if err == nil {
		t.Fatal("Load() error = nil, want non-renderable animation reference")
	}
	for _, want := range []string{
		`rule "alert"`,
		`matrix_rain_background`,
		`non-renderable`,
		`renderable/playable`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load() error = %v, want containing %q", err, want)
		}
	}
}

func TestLoadRejectsUnknownBackgroundAnimationReference(t *testing.T) {
	animationsPath := writeFile(t, "animations.yaml", `
animations:
  alert:
    type: generated
    generator: notification
`)
	rulesPath := writeFile(t, "rules.yaml", validRules("notification"))
	path := writeConfig(t, `
animations_file: "`+animationsPath+`"
rules_file: "`+rulesPath+`"
background:
  animation: missing_background
  restore_on_idle: true
queue:
  overflow_policy: block
  dedup_window: 0s
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `background references unknown animation "missing_background"`) {
		t.Fatalf("Load() error = %v, want unknown background animation reference", err)
	}
}

func TestLoadAllowsFirmwarePresetBackgroundReference(t *testing.T) {
	animationsPath := writeFile(t, "animations.yaml", `
animations:
  matrix_rain_background:
    type: firmware_preset
    effect_id: 12
    interval: 90ms
    color: "#00FF55"
`)
	rulesPath := writeFile(t, "rules.yaml", validRules("notification"))
	path := writeConfig(t, `
animations_file: "`+animationsPath+`"
rules_file: "`+rulesPath+`"
background:
  animation: matrix_rain_background
  restore_on_idle: true
queue:
  overflow_policy: block
  dedup_window: 0s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Background.Animation != "matrix_rain_background" || !cfg.Background.RestoreOnIdle {
		t.Fatalf("background = %+v, want firmware preset restore enabled", cfg.Background)
	}
	if _, ok := cfg.AnimationRegistry.FirmwarePreset("matrix_rain_background"); !ok {
		t.Fatal("loaded registry missing firmware preset background")
	}
}

func loadWithAnimations(t *testing.T, animationsBody, rulesBody string) error {
	t.Helper()

	animationsPath := writeFile(t, "animations.yaml", animationsBody)
	rulesPath := writeFile(t, "rules.yaml", rulesBody)
	path := writeConfig(t, `
animations_file: "`+animationsPath+`"
rules_file: "`+rulesPath+`"
queue:
  overflow_policy: block
  dedup_window: 0s
`)
	_, err := Load(path)
	return err
}

func validRules(animationID string) string {
	return `
rules:
  - id: alert
    when:
      source: http
      type: notify
    play:
      animation: ` + animationID + `
      duration: 2s
`
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	return writeFile(t, "config.yaml", body)
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()

	if name == "" {
		name = "config.yaml"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
