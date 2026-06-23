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
)

type Entry struct {
	ID             string
	Kind           EntryKind
	GeneratorID    string
	Animation      Animation
	FirmwarePreset *FirmwarePreset
}

type CatalogEntry struct {
	ID       string         `json:"id"`
	Kind     EntryKind      `json:"kind"`
	Playable bool           `json:"playable"`
	EffectID *byte          `json:"effect_id,omitempty"`
	Interval *time.Duration `json:"interval,omitempty"`
	Color    *RGB           `json:"color,omitempty"`
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

func (r *Registry) MustGet(id string) (Animation, error) {
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
		item := CatalogEntry{
			ID:       id,
			Kind:     entry.Kind,
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
	return entry
}

func ValidateFirmwarePreset(preset FirmwarePreset) error {
	if preset.Interval < 0 {
		return fmt.Errorf("firmware preset interval cannot be negative: %s", preset.Interval)
	}
	if preset.Interval.Milliseconds() > 65535 {
		return fmt.Errorf("firmware preset interval must be <= 65535ms: %s", preset.Interval)
	}
	return nil
}
