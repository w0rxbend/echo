package animations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRegistryCatalogProjectsInternalRenderableKindToGenerated(t *testing.T) {
	registry := NewRegistry()
	animation := AnimationFunc(func(context.Context, Params) ([]Frame, error) {
		return []Frame{{Delay: time.Millisecond}}, nil
	})

	registry.mu.Lock()
	registry.entries["idle_background"] = Entry{
		ID:        "idle_background",
		Kind:      EntryKind("renderable"),
		Animation: animation,
	}
	registry.mu.Unlock()

	catalog := registry.Catalog()
	if len(catalog) != 1 {
		t.Fatalf("Catalog() = %+v, want one entry", catalog)
	}
	entry := catalog[0]
	if entry.ID != "idle_background" || entry.Kind != PublicKindGenerated || !entry.Playable {
		t.Fatalf("Catalog()[0] = %+v, want playable generated idle_background", entry)
	}

	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "renderable") {
		t.Fatalf("Catalog() public JSON leaked internal kind: %s", data)
	}
}
