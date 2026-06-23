package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/rules"
	"gopkg.in/yaml.v3"
)

func Load(path string) (Config, error) {
	if envPath := os.Getenv("MATRIX_PROXY_CONFIG"); envPath != "" {
		path = envPath
	}
	if path == "" {
		path = DefaultPath
	}

	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && path == DefaultPath {
			return cfg, cfg.Validate()
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var schema schemaConfig
	if err := yaml.Unmarshal(data, &schema); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	if err := schema.apply(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}

	animationsFile := resolveReferencedPath(path, cfg.AnimationsFile)
	animationRegistry, err := loadAnimationRegistry(animationsFile)
	if err != nil {
		return Config{}, fmt.Errorf("load animations: %w", err)
	}
	cfg.AnimationsFile = animationsFile
	cfg.AnimationRegistry = animationRegistry
	if err := validateBackgroundAnimationReference(cfg, animationRegistry); err != nil {
		return Config{}, fmt.Errorf("validate background animation: %w", err)
	}
	rulesFile := cfg.RulesFile
	if rulesFile != Default().RulesFile {
		rulesFile = resolveReferencedPath(path, rulesFile)
	}
	if err := validateRuleAnimationReferences(rulesFile, animationRegistry); err != nil {
		return Config{}, fmt.Errorf("validate animation references: %w", err)
	}
	cfg.RulesFile = rulesFile

	return cfg, nil
}

type schemaAnimationsFile struct {
	Animations map[string]schemaAnimation `yaml:"animations"`
}

type schemaAnimation struct {
	Type      string             `yaml:"type"`
	Generator string             `yaml:"generator"`
	EffectID  *int               `yaml:"effect_id"`
	Interval  *schemaDuration    `yaml:"interval"`
	Color     *schemaRGB         `yaml:"color"`
	Palette   schemaFramePalette `yaml:"palette"`
	Frames    []schemaFrame      `yaml:"frames"`
}

type schemaRGB struct {
	animations.RGB
}

type schemaFramePalette []animations.FramePaletteEntry

type schemaFrame struct {
	Delay *schemaDuration `yaml:"delay"`
	Rows  []string        `yaml:"rows"`
}

func (c *schemaRGB) UnmarshalYAML(unmarshal func(any) error) error {
	var text string
	if err := unmarshal(&text); err == nil {
		rgb, err := parseHexRGB(text)
		if err != nil {
			return err
		}
		c.RGB = rgb
		return nil
	}

	var rgb animations.RGB
	if err := unmarshal(&rgb); err != nil {
		return err
	}
	c.RGB = rgb
	return nil
}

func (p *schemaFramePalette) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("palette must be a mapping of one-character symbols to colors")
	}

	seen := make(map[string]struct{}, len(node.Content)/2)
	entries := make([]animations.FramePaletteEntry, 0, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("palette entry %d symbol must be a scalar", i/2)
		}
		symbol := key.Value
		if _, exists := seen[symbol]; exists {
			return fmt.Errorf("duplicate palette symbol %q", symbol)
		}
		seen[symbol] = struct{}{}

		var color schemaRGB
		if err := value.Decode(&color); err != nil {
			return fmt.Errorf("palette symbol %q color: %w", symbol, err)
		}
		entries = append(entries, animations.FramePaletteEntry{
			Symbol: symbol,
			Color:  color.RGB,
		})
	}
	*p = entries
	return nil
}

func loadAnimationRegistry(path string) (*animations.Registry, error) {
	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return registry, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read animations file: %w", err)
	}

	var file schemaAnimationsFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse animations file: %w", err)
	}

	ids := make([]string, 0, len(file.Animations))
	for id := range file.Animations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := registerConfiguredAnimation(registry, id, file.Animations[id]); err != nil {
			return nil, fmt.Errorf("animation %q: %w", id, err)
		}
	}

	return registry, nil
}

func resolveReferencedPath(configPath, referencedPath string) string {
	if referencedPath == "" || filepath.IsAbs(referencedPath) {
		return referencedPath
	}
	if _, err := os.Stat(referencedPath); err == nil {
		return referencedPath
	}
	configDir := filepath.Dir(configPath)
	if configDir == "." || configDir == "" {
		return referencedPath
	}
	return filepath.Join(configDir, referencedPath)
}

func registerConfiguredAnimation(registry *animations.Registry, id string, entry schemaAnimation) error {
	switch entry.Type {
	case string(animations.EntryGenerated):
		if entry.Generator == "" {
			return errors.New("generator is required for generated animation")
		}
		animation, err := animations.NewGeneratedAnimation(entry.Generator)
		if err != nil {
			return err
		}
		return registry.RegisterGenerated(id, entry.Generator, animation)
	case string(animations.EntryFirmwarePreset):
		if entry.EffectID == nil {
			return errors.New("effect_id is required for firmware_preset animation")
		}
		if *entry.EffectID < 0 || *entry.EffectID > 255 {
			return fmt.Errorf("effect_id must be between 0 and 255: %d", *entry.EffectID)
		}
		if entry.Interval == nil {
			return errors.New("interval is required for firmware_preset animation")
		}
		preset := animations.FirmwarePreset{
			EffectID: byte(*entry.EffectID),
			Interval: entry.Interval.Duration,
		}
		if entry.Color != nil {
			preset.Color = entry.Color.RGB
		}
		return registry.RegisterFirmwarePreset(id, preset)
	case "frames":
		return registerConfiguredFrameAnimation(registry, id, entry)
	case "":
		return errors.New("type is required")
	default:
		return fmt.Errorf("unknown animation type %q", entry.Type)
	}
}

func registerConfiguredFrameAnimation(registry *animations.Registry, id string, entry schemaAnimation) error {
	if entry.Palette == nil {
		return errors.New("palette is required for frames animation")
	}

	specs := make([]animations.FrameSpec, 0, len(entry.Frames))
	for i, frame := range entry.Frames {
		if frame.Delay == nil {
			return fmt.Errorf("frame %d delay is required", i)
		}
		specs = append(specs, animations.FrameSpec{
			Delay: frame.Delay.Duration,
			Rows:  frame.Rows,
		})
	}

	animation, err := animations.NewFrameAnimation(specs, []animations.FramePaletteEntry(entry.Palette))
	if err != nil {
		return err
	}
	return registry.RegisterGenerated(id, "frames", animation)
}

func validateRuleAnimationReferences(path string, registry *animations.Registry) error {
	engine, err := rules.LoadFile(path)
	if err != nil {
		if path != Default().RulesFile || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		engine, err = rules.New(nil)
		if err != nil {
			return err
		}
	}
	for _, rule := range engine.Rules() {
		if !registry.Has(rule.Play.Animation) {
			return fmt.Errorf("rule %q references unknown animation %q", rule.ID, rule.Play.Animation)
		}
		if !registry.IsRenderable(rule.Play.Animation) {
			return fmt.Errorf("rule %q references non-renderable animation %q; play.animation must reference a renderable/playable animation", rule.ID, rule.Play.Animation)
		}
	}
	return nil
}

func validateBackgroundAnimationReference(cfg Config, registry *animations.Registry) error {
	if cfg.Background.Animation == "" || !cfg.Background.RestoreOnIdle {
		return nil
	}
	if !registry.Has(cfg.Background.Animation) {
		return fmt.Errorf("background references unknown animation %q", cfg.Background.Animation)
	}
	return nil
}

func parseHexRGB(text string) (animations.RGB, error) {
	text = strings.TrimSpace(text)
	if len(text) != 7 || text[0] != '#' {
		return animations.RGB{}, fmt.Errorf("color must be #RRGGBB: %q", text)
	}
	var values [3]byte
	for i := 0; i < 3; i++ {
		offset := 1 + i*2
		value, ok := parseHexByte(text[offset : offset+2])
		if !ok {
			return animations.RGB{}, fmt.Errorf("color must be #RRGGBB: %q", text)
		}
		values[i] = value
	}
	return animations.RGB{R: values[0], G: values[1], B: values[2]}, nil
}

func parseHexByte(text string) (byte, bool) {
	high, ok := hexNibble(text[0])
	if !ok {
		return 0, false
	}
	low, ok := hexNibble(text[1])
	if !ok {
		return 0, false
	}
	return high<<4 | low, true
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}
