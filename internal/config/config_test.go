package config

import (
	"context"
	"encoding/json"
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

func TestLoadFrameAnimationFromConfig(t *testing.T) {
	animationsPath := writeFile(t, "animations.yaml", `
animations:
  pixel_badge:
    type: frames
    palette:
      ".": "#000000"
      R: "#FF0000"
      G:
        r: 0
        g: 255
        b: 0
      B: "#0000FF"
    frames:
      - delay: 125ms
        rows:
          - "R......."
          - ".G......"
          - "...B...."
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
`)
	rulesPath := writeFile(t, "rules.yaml", validRules("pixel_badge"))
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

	if !containsString(cfg.AnimationRegistry.RenderableIDs(), "pixel_badge") {
		t.Fatalf("RenderableIDs() = %v, want pixel_badge", cfg.AnimationRegistry.RenderableIDs())
	}
	animation, ok := cfg.AnimationRegistry.Get("pixel_badge")
	if !ok {
		t.Fatal("frame animation is not registered as renderable")
	}
	frames, err := animation.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("Render() returned %d frames, want 1", len(frames))
	}
	if frames[0].Delay != 125*time.Millisecond {
		t.Fatalf("frame delay = %s, want 125ms", frames[0].Delay)
	}
	assertConfigFramePixel(t, frames[0], 0, 0, animations.RGB{R: 255})
	assertConfigFramePixel(t, frames[0], 1, 1, animations.RGB{G: 255})
	assertConfigFramePixel(t, frames[0], 3, 2, animations.RGB{B: 255})
	assertConfigFramePixel(t, frames[0], 7, 7, animations.RGB{})

	var catalogEntry animations.CatalogEntry
	found := false
	for _, entry := range cfg.AnimationRegistry.Catalog() {
		if entry.ID == "pixel_badge" {
			catalogEntry = entry
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Catalog() = %+v, want pixel_badge", cfg.AnimationRegistry.Catalog())
	}
	if catalogEntry.Kind != animations.PublicKindGenerated || !catalogEntry.Playable {
		t.Fatalf("catalog entry = %+v, want playable generated", catalogEntry)
	}
	if catalogEntry.EffectID != nil || catalogEntry.Interval != nil || catalogEntry.Color != nil {
		t.Fatalf("frame animation catalog entry exposes firmware metadata: %+v", catalogEntry)
	}
	catalogJSON, err := json.Marshal(cfg.AnimationRegistry.Catalog())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(catalogJSON), "renderable") {
		t.Fatalf("frame animation catalog leaked internal kind: %s", catalogJSON)
	}
}

func TestLoadRejectsUnknownAnimationType(t *testing.T) {
	err := loadWithAnimations(t, `
animations:
  wave:
    type: sparkle
`, validRules("notification"))
	if err == nil || !strings.Contains(err.Error(), `unknown animation type "sparkle"`) {
		t.Fatalf("Load() error = %v, want unknown animation type", err)
	}
}

func TestLoadRejectsStrayAnimationTypeFields(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		animationType string
		field         string
	}{
		{name: "generated effect id", fixture: "animation_generated_with_effect_id.yaml", animationType: "generated", field: "effect_id"},
		{name: "generated interval", fixture: "animation_generated_with_interval.yaml", animationType: "generated", field: "interval"},
		{name: "generated color", fixture: "animation_generated_with_color.yaml", animationType: "generated", field: "color"},
		{name: "generated palette", fixture: "animation_generated_with_palette.yaml", animationType: "generated", field: "palette"},
		{name: "generated frames", fixture: "animation_generated_with_frames.yaml", animationType: "generated", field: "frames"},
		{name: "firmware preset generator", fixture: "animation_firmware_preset_with_generator.yaml", animationType: "firmware_preset", field: "generator"},
		{name: "firmware preset palette", fixture: "animation_firmware_preset_with_palette.yaml", animationType: "firmware_preset", field: "palette"},
		{name: "firmware preset frames", fixture: "animation_firmware_preset_with_frames.yaml", animationType: "firmware_preset", field: "frames"},
		{name: "frames generator", fixture: "animation_frames_with_generator.yaml", animationType: "frames", field: "generator"},
		{name: "frames effect id", fixture: "animation_frames_with_effect_id.yaml", animationType: "frames", field: "effect_id"},
		{name: "frames interval", fixture: "animation_frames_with_interval.yaml", animationType: "frames", field: "interval"},
		{name: "frames color", fixture: "animation_frames_with_color.yaml", animationType: "frames", field: "color"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadWithAnimationFixture(t, tt.fixture)
			if err == nil {
				t.Fatal("Load() error = nil, want stray field rejection")
			}
			for _, want := range []string{
				`field "` + tt.field + `"`,
				tt.animationType + " animation",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load() error = %v, want containing %q", err, want)
				}
			}
		})
	}
}

func TestLoadRejectsInvalidFrameAnimationConfig(t *testing.T) {
	tests := []struct {
		name      string
		animation string
		wantErr   string
	}{
		{
			name: "missing palette",
			animation: `
animations:
  pixels:
    type: frames
    frames:
      - delay: 100ms
        rows:
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
`,
			wantErr: "palette is required for frames animation",
		},
		{
			name: "empty frames",
			animation: `
animations:
  pixels:
    type: frames
    palette:
      ".": "#000000"
    frames: []
`,
			wantErr: "frame animation requires at least one frame",
		},
		{
			name: "missing delay",
			animation: `
animations:
  pixels:
    type: frames
    palette:
      ".": "#000000"
    frames:
      - rows:
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
`,
			wantErr: "frame 0 delay is required",
		},
		{
			name: "zero delay",
			animation: validFrameAnimationConfig("0s", []string{
				"........", "........", "........", "........",
				"........", "........", "........", "........",
			}),
			wantErr: "frame 0 delay must be positive",
		},
		{
			name: "negative delay",
			animation: validFrameAnimationConfig("-1ms", []string{
				"........", "........", "........", "........",
				"........", "........", "........", "........",
			}),
			wantErr: "frame 0 delay must be positive",
		},
		{
			name: "invalid delay",
			animation: validFrameAnimationConfig("soon", []string{
				"........", "........", "........", "........",
				"........", "........", "........", "........",
			}),
			wantErr: `parse duration "soon"`,
		},
		{
			name: "wrong row count",
			animation: validFrameAnimationConfig("100ms", []string{
				"........", "........", "........", "........",
				"........", "........", "........",
			}),
			wantErr: "frame 0 must have exactly 8 rows",
		},
		{
			name: "wrong row width",
			animation: validFrameAnimationConfig("100ms", []string{
				".......", "........", "........", "........",
				"........", "........", "........", "........",
			}),
			wantErr: "frame 0 row 0 must have exactly 8 symbols",
		},
		{
			name: "unknown symbol",
			animation: validFrameAnimationConfig("100ms", []string{
				"X.......", "........", "........", "........",
				"........", "........", "........", "........",
			}),
			wantErr: "uses unknown palette symbol",
		},
		{
			name: "duplicate palette symbol",
			animation: `
animations:
  pixels:
    type: frames
    palette:
      ".": "#000000"
      ".": "#111111"
    frames:
      - delay: 100ms
        rows:
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
          - "........"
`,
			wantErr: `duplicate palette symbol "."`,
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

func loadWithAnimationFixture(t *testing.T, fixture string) error {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	return loadWithAnimations(t, string(body), validRules("notification"))
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

func validFrameAnimationConfig(delay string, rows []string) string {
	var builder strings.Builder
	builder.WriteString(`
animations:
  pixels:
    type: frames
    palette:
      ".": "#000000"
    frames:
      - delay: `)
	builder.WriteString(delay)
	builder.WriteString(`
        rows:
`)
	for _, row := range rows {
		builder.WriteString(`          - "`)
		builder.WriteString(row)
		builder.WriteString(`"
`)
	}
	return builder.String()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertConfigFramePixel(t *testing.T, frame animations.Frame, x, y int, want animations.RGB) {
	t.Helper()

	got := frame.Pixels[y*animations.CanvasWidth+x]
	if got != want {
		t.Fatalf("display pixel (%d,%d) = %+v, want %+v", x, y, got, want)
	}
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
