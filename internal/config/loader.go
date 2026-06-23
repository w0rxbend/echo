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
	Type      string              `yaml:"type"`
	Generator *string             `yaml:"generator"`
	EffectID  *int                `yaml:"effect_id"`
	Interval  *schemaDuration     `yaml:"interval"`
	Color     *schemaRGB          `yaml:"color"`
	Palette   *schemaFramePalette `yaml:"palette"`
	Frames    *[]schemaFrame      `yaml:"frames"`

	presentFields map[string]struct{}
}

type schemaRGB struct {
	animations.RGB
}

type schemaFramePalette []animations.FramePaletteEntry

type schemaFrame struct {
	Delay *schemaDuration `yaml:"delay"`
	Rows  []string        `yaml:"rows"`
}

func (a *schemaAnimation) UnmarshalYAML(node *yaml.Node) error {
	type rawSchemaAnimation schemaAnimation
	var raw rawSchemaAnimation
	if err := node.Decode(&raw); err != nil {
		return err
	}

	*a = schemaAnimation(raw)
	a.presentFields = make(map[string]struct{}, len(node.Content)/2)
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind == yaml.ScalarNode {
				a.presentFields[key.Value] = struct{}{}
			}
		}
	}
	return nil
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
	if err := parseAnimationsFile(data, &file); err != nil {
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

func parseAnimationsFile(data []byte, file *schemaAnimationsFile) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if err := validateAnimationsFileSchema(&root); err != nil {
		return err
	}
	return root.Decode(file)
}

func validateAnimationsFileSchema(root *yaml.Node) error {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return errors.New("animations file must be a YAML document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return errors.New("animations file must be a mapping")
	}
	if err := rejectDuplicateTopLevelFields(doc); err != nil {
		return err
	}

	for i := 0; i < len(doc.Content); i += 2 {
		key := doc.Content[i]
		value := doc.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return errors.New("top-level field must be a scalar")
		}
		if key.Value != "animations" {
			return fmt.Errorf("unknown top-level field %q at %s", key.Value, key.Value)
		}
		if err := validateAnimationsMapSchema(value); err != nil {
			return err
		}
	}
	return nil
}

func validateAnimationsMapSchema(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("animations must be a mapping")
	}
	if err := rejectDuplicateAnimationIDs(node); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return errors.New("animation id must be a scalar")
		}
		id := key.Value
		if value.Kind != yaml.MappingNode {
			return fmt.Errorf("animation %q: entry at animation %s must be a mapping", id, id)
		}
		if err := validateAnimationEntrySchema(id, value); err != nil {
			return err
		}
	}
	return nil
}

func validateAnimationEntrySchema(id string, node *yaml.Node) error {
	if err := rejectDuplicateAnimationFields(id, node, "field", ""); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("animation %q: field at animation %s must be a scalar", id, id)
		}
		path := animationFieldPath(id, key.Value)
		switch key.Value {
		case "type", "generator", "effect_id", "interval":
		case "color":
			if err := validateAnimationColorSchema(id, value, path); err != nil {
				return err
			}
		case "palette":
			if err := validateAnimationPaletteSchema(id, value, path); err != nil {
				return err
			}
		case "frames":
			if err := validateAnimationFramesSchema(id, value, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("animation %q: unknown field %q at %s", id, key.Value, path)
		}
	}
	return nil
}

func validateAnimationFramesSchema(id string, node *yaml.Node, path string) error {
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	for i, frame := range node.Content {
		framePath := fmt.Sprintf("%s[%d]", path, i)
		if frame.Kind != yaml.MappingNode {
			return fmt.Errorf("animation %q: frame at %s must be a mapping", id, framePath)
		}
		if err := rejectDuplicateAnimationFields(id, frame, "field", framePath); err != nil {
			return err
		}
		for j := 0; j < len(frame.Content); j += 2 {
			key := frame.Content[j]
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("animation %q: frame field at %s must be a scalar", id, framePath)
			}
			switch key.Value {
			case "delay", "rows":
			default:
				return fmt.Errorf("animation %q: unknown field %q at %s.%s", id, key.Value, framePath, key.Value)
			}
		}
	}
	return nil
}

func validateAnimationPaletteSchema(id string, node *yaml.Node, path string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	if err := rejectDuplicatePaletteSymbols(id, node, path); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("animation %q: palette symbol at %s must be a scalar", id, path)
		}
		if err := validateAnimationColorSchema(id, value, path+"."+key.Value); err != nil {
			return err
		}
	}
	return nil
}

func validateAnimationColorSchema(id string, node *yaml.Node, path string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	if err := rejectDuplicateAnimationFields(id, node, "field", path); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("animation %q: color field at %s must be a scalar", id, path)
		}
		switch key.Value {
		case "r", "g", "b":
		default:
			return fmt.Errorf("animation %q: unknown field %q at %s.%s", id, key.Value, path, key.Value)
		}
	}
	return nil
}

func rejectDuplicateTopLevelFields(node *yaml.Node) error {
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("duplicate top-level field %q at %s", key.Value, key.Value)
		}
		seen[key.Value] = struct{}{}
	}
	return nil
}

func rejectDuplicateAnimationIDs(node *yaml.Node) error {
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("duplicate animation id %q at animations.%s", key.Value, key.Value)
		}
		seen[key.Value] = struct{}{}
	}
	return nil
}

func rejectDuplicateAnimationFields(id string, node *yaml.Node, duplicateKind, path string) error {
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		fieldPath := path
		if fieldPath == "" {
			fieldPath = animationFieldPath(id, key.Value)
		} else {
			fieldPath += "." + key.Value
		}
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("animation %q: duplicate %s %q at %s", id, duplicateKind, key.Value, fieldPath)
		}
		seen[key.Value] = struct{}{}
	}
	return nil
}

func rejectDuplicatePaletteSymbols(id string, node *yaml.Node, path string) error {
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		symbolPath := path + "." + key.Value
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("animation %q: duplicate palette symbol %q at %s", id, key.Value, symbolPath)
		}
		seen[key.Value] = struct{}{}
	}
	return nil
}

func animationFieldPath(id, field string) string {
	return "animation " + id + "." + field
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
		if err := rejectAnimationFields(entry.Type, entry, "effect_id", "interval", "color", "palette", "frames"); err != nil {
			return err
		}
		if entry.Generator == nil || *entry.Generator == "" {
			return errors.New("generator is required for generated animation")
		}
		animation, err := animations.NewGeneratedAnimation(*entry.Generator)
		if err != nil {
			return err
		}
		return registry.RegisterGenerated(id, *entry.Generator, animation)
	case string(animations.EntryFirmwarePreset):
		if err := rejectAnimationFields(entry.Type, entry, "generator", "palette", "frames"); err != nil {
			return err
		}
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
		if err := rejectAnimationFields(entry.Type, entry, "generator", "effect_id", "interval", "color"); err != nil {
			return err
		}
		return registerConfiguredFrameAnimation(registry, id, entry)
	case "":
		return errors.New("type is required")
	default:
		return fmt.Errorf("unknown animation type %q", entry.Type)
	}
}

func rejectAnimationFields(animationType string, entry schemaAnimation, fields ...string) error {
	for _, field := range fields {
		if !animationFieldPresent(entry, field) {
			continue
		}
		return fmt.Errorf("field %q is not allowed for %s animation", field, animationType)
	}
	return nil
}

func animationFieldPresent(entry schemaAnimation, field string) bool {
	if _, ok := entry.presentFields[field]; ok {
		return true
	}
	switch field {
	case "generator":
		return entry.Generator != nil
	case "effect_id":
		return entry.EffectID != nil
	case "interval":
		return entry.Interval != nil
	case "color":
		return entry.Color != nil
	case "palette":
		return entry.Palette != nil
	case "frames":
		return entry.Frames != nil
	default:
		return false
	}
}

func registerConfiguredFrameAnimation(registry *animations.Registry, id string, entry schemaAnimation) error {
	if entry.Palette == nil {
		return errors.New("palette is required for frames animation")
	}

	frames := []schemaFrame(nil)
	if entry.Frames != nil {
		frames = *entry.Frames
	}
	specs := make([]animations.FrameSpec, 0, len(frames))
	for i, frame := range frames {
		if frame.Delay == nil {
			return fmt.Errorf("frame %d delay is required", i)
		}
		specs = append(specs, animations.FrameSpec{
			Delay: frame.Delay.Duration,
			Rows:  frame.Rows,
		})
	}

	animation, err := animations.NewFrameAnimation(specs, []animations.FramePaletteEntry(*entry.Palette))
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
