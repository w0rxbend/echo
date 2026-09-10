package animations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrAnimationNotFound = errors.New("animation not found")
	ErrAnimationExists   = errors.New("animation already registered")
)

type AnimationFunc func(context.Context, Params) ([]Frame, error)

func (f AnimationFunc) Render(ctx context.Context, params Params) ([]Frame, error) {
	return f(ctx, params)
}

type EntryKind string

const (
	EntryGenerated      EntryKind = "generated"
	EntryFirmwarePreset EntryKind = "firmware_preset"
	EntryStaticColor    EntryKind = "static_color"
)

type PublicKind string

const (
	PublicKindGenerated      PublicKind = "generated"
	PublicKindFirmwarePreset PublicKind = "firmware_preset"
	PublicKindStaticColor    PublicKind = "static_color"
)

var publicKindMap = map[string]PublicKind{
	string(EntryGenerated):      PublicKindGenerated,
	"renderable":                PublicKindGenerated,
	string(EntryFirmwarePreset): PublicKindFirmwarePreset,
	string(EntryStaticColor):    PublicKindStaticColor,
}

func ProjectPublicKind(kind string) (PublicKind, bool) {
	projected, ok := publicKindMap[kind]
	return projected, ok
}

type Entry struct {
	ID             string
	Kind           EntryKind
	GeneratorID    string
	Animation      Animation
	FirmwarePreset *FirmwarePreset
	StaticColor    *RGB
}

type CatalogEntry struct {
	ID       string
	Kind     PublicKind
	Playable bool
	EffectID *byte
	Interval *time.Duration
	Color    *RGB
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]Entry)}
}

func (r *Registry) Register(id string, animation Animation) error {
	return r.RegisterGenerated(id, id, animation)
}

func (r *Registry) RegisterGenerated(id, generatorID string, animation Animation) error {
	if id == "" {
		return errors.New("animation id is required")
	}
	if generatorID == "" {
		return errors.New("animation generator is required")
	}
	if animation == nil {
		return errors.New("animation is required")
	}
	return r.register(Entry{
		ID:          id,
		Kind:        EntryGenerated,
		GeneratorID: generatorID,
		Animation:   animation,
	})
}

func (r *Registry) RegisterFirmwarePreset(id string, preset FirmwarePreset) error {
	if id == "" {
		return errors.New("animation id is required")
	}
	if err := ValidateFirmwarePreset(preset); err != nil {
		return err
	}
	copied := preset
	return r.register(Entry{
		ID:             id,
		Kind:           EntryFirmwarePreset,
		FirmwarePreset: &copied,
	})
}

// RegisterStaticColor registers a fixed-colour entry backed by the firmware's
// static-colour command (0x07). Unlike a fill, the firmware keeps re-asserting the
// colour, which is what makes it usable as an idle background.
func (r *Registry) RegisterStaticColor(id string, color RGB) error {
	if id == "" {
		return errors.New("animation id is required")
	}
	copied := color
	return r.register(Entry{
		ID:          id,
		Kind:        EntryStaticColor,
		StaticColor: &copied,
	})
}

// StaticColor returns the colour registered for a static_color entry.
func (r *Registry) StaticColor(id string) (RGB, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[id]
	if !ok || entry.StaticColor == nil {
		return RGB{}, false
	}
	return *entry.StaticColor, true
}

func (r *Registry) register(entry Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]Entry)
	}
	if _, exists := r.entries[entry.ID]; exists {
		return fmt.Errorf("%w: %s", ErrAnimationExists, entry.ID)
	}
	r.entries[entry.ID] = entry
	return nil
}

func (r *Registry) Get(id string) (Animation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[id]
	if !ok || entry.Animation == nil {
		return nil, false
	}
	return entry.Animation, true
}

func (r *Registry) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[id]
	return ok
}

func (r *Registry) IsRenderable(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[id]
	return ok && entry.Animation != nil
}

func (r *Registry) Entry(id string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[id]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(entry), true
}

func (r *Registry) FirmwarePreset(id string) (FirmwarePreset, bool) {
	entry, ok := r.Entry(id)
	if !ok || entry.FirmwarePreset == nil {
		return FirmwarePreset{}, false
	}
	return *entry.FirmwarePreset, true
}

func (r *Registry) GetByID(id string) (Animation, error) {
	animation, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAnimationNotFound, id)
	}
	return animation, nil
}

func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Registry) RenderableIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.entries))
	for id, entry := range r.entries {
		if entry.Animation != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (r *Registry) Catalog() []CatalogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	catalog := make([]CatalogEntry, 0, len(r.entries))
	for id, entry := range r.entries {
		kind, ok := ProjectPublicKind(string(entry.Kind))
		if !ok {
			continue
		}
		item := CatalogEntry{
			ID:       id,
			Kind:     kind,
			Playable: entry.Animation != nil,
		}
		if entry.FirmwarePreset != nil {
			effectID := entry.FirmwarePreset.EffectID
			interval := entry.FirmwarePreset.Interval
			color := entry.FirmwarePreset.Color
			item.EffectID = &effectID
			item.Interval = &interval
			item.Color = &color
		}
		if entry.StaticColor != nil {
			color := *entry.StaticColor
			item.Color = &color
		}
		catalog = append(catalog, item)
	}
	sort.Slice(catalog, func(i, j int) bool {
		return catalog[i].ID < catalog[j].ID
	})
	return catalog
}

func cloneEntry(entry Entry) Entry {
	if entry.FirmwarePreset != nil {
		preset := *entry.FirmwarePreset
		entry.FirmwarePreset = &preset
	}
	if entry.StaticColor != nil {
		color := *entry.StaticColor
		entry.StaticColor = &color
	}
	return entry
}

// StopEffectID is the firmware's "stop effect" sentinel. It halts the running
// effect without touching the frame buffer or calling render(), so the panel keeps
// whatever the last effect tick happened to draw. That makes it unusable as a
// restorable display state: replaying it reproduces no image.
const StopEffectID byte = 0

// colourIgnoringEffectIDs are the firmware effects that accept an RGB triple on the
// wire and discard it, because they compute their own colours:
// 7 rainbow, 11 fire, 17 plasma, 22 confetti. Verified against the renderers in
// led-matrix-controller/src/TcpMatrixServer.cpp.
//
// This matters beyond documentation: the scheduler remembers EffectID, Interval and
// Color for convergence matching, so without this, two visually identical
// backgrounds could fail to match on a byte the panel never used.
var colourIgnoringEffectIDs = map[byte]struct{}{
	7:  {},
	11: {},
	17: {},
	22: {},
}

// FirmwareEffectIgnoresColor reports whether the effect computes its own colours
// and discards the caller's.
func FirmwareEffectIgnoresColor(effectID byte) bool {
	_, ok := colourIgnoringEffectIDs[effectID]
	return ok
}

// MaxFirmwareEffectID is the highest effect id the firmware implements.
// TcpMatrixServer::applyCommand rejects anything above this with
// Status::kInvalidLength, so reject it here instead of surfacing a 502 later.
// Effect id 0 is the documented "stop effect" sentinel.
const MaxFirmwareEffectID = 22

func ValidateFirmwarePreset(preset FirmwarePreset) error {
	if preset.EffectID > MaxFirmwareEffectID {
		return fmt.Errorf("firmware preset effect_id must be between 0 and %d: %d", MaxFirmwareEffectID, preset.EffectID)
	}
	if preset.Interval < 0 {
		return fmt.Errorf("firmware preset interval cannot be negative: %s", preset.Interval)
	}
	if preset.Interval.Milliseconds() > 65535 {
		return fmt.Errorf("firmware preset interval must be <= 65535ms: %s", preset.Interval)
	}
	return nil
}
