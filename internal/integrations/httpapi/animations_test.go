package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/worxbend/echo/internal/animations"
)

func TestAnimationCatalogFreezesFrameAndFirmwarePresetContract(t *testing.T) {
	const (
		generatedAnimationID = animations.NotificationAnimationID
		frameAnimationID     = "pixel_badge"
		firmwarePresetID     = "matrix_rain_background"
	)
	httpServer := newConfigAuthoredFrameAnimationAPITestServer(t)

	resp, err := http.Get(httpServer.URL + "/api/v1/animations/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /animations/catalog status = %d, body = %s, want %d", resp.StatusCode, data, http.StatusOK)
	}

	var body struct {
		Animations []map[string]any `json:"animations"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}

	allowedFields := map[string]bool{
		"id":        true,
		"kind":      true,
		"playable":  true,
		"effect_id": true,
		"interval":  true,
		"color":     true,
	}
	entries := make(map[string]map[string]any, len(body.Animations))
	for i, entry := range body.Animations {
		for field := range entry {
			if !allowedFields[field] {
				t.Fatalf("GET /animations/catalog entry %d id=%v leaked non-DTO field %q: %+v", i, entry["id"], field, entry)
			}
		}
		id, ok := entry["id"].(string)
		if !ok || id == "" {
			t.Fatalf("GET /animations/catalog entry %d missing string id: %+v", i, entry)
		}
		if _, ok := entry["kind"].(string); !ok {
			t.Fatalf("GET /animations/catalog entry %d id=%s missing string kind: %+v", i, id, entry)
		}
		if _, ok := entry["playable"].(bool); !ok {
			t.Fatalf("GET /animations/catalog entry %d id=%s missing bool playable: %+v", i, id, entry)
		}
		if kind := entry["kind"]; kind == "renderable" {
			t.Fatalf("GET /animations/catalog entry %d id=%s leaked internal kind %q", i, id, kind)
		}
		entries[id] = entry
	}

	generatedEntry, ok := entries[generatedAnimationID]
	if !ok {
		t.Fatalf("GET /animations/catalog missing generated animation %q: %+v", generatedAnimationID, body.Animations)
	}
	if got := generatedEntry["kind"]; got != string(animations.PublicKindGenerated) {
		t.Fatalf("GET /animations/catalog generated animation kind = %v, want %q", got, animations.PublicKindGenerated)
	}
	if got := generatedEntry["playable"]; got != true {
		t.Fatalf("GET /animations/catalog generated animation playable = %v, want true", got)
	}
	for _, field := range []string{"effect_id", "interval", "color"} {
		if _, ok := generatedEntry[field]; ok {
			t.Fatalf("GET /animations/catalog generated animation leaked firmware metadata field %q: %+v", field, generatedEntry)
		}
	}

	frameEntry, ok := entries[frameAnimationID]
	if !ok {
		t.Fatalf("GET /animations/catalog missing frame animation %q: %+v", frameAnimationID, body.Animations)
	}
	if got := frameEntry["kind"]; got != string(animations.PublicKindGenerated) {
		t.Fatalf("GET /animations/catalog frame animation kind = %v, want %q", got, animations.PublicKindGenerated)
	}
	if got := frameEntry["playable"]; got != true {
		t.Fatalf("GET /animations/catalog frame animation playable = %v, want true", got)
	}
	for _, field := range []string{"effect_id", "interval", "color"} {
		if _, ok := frameEntry[field]; ok {
			t.Fatalf("GET /animations/catalog frame animation leaked firmware metadata field %q: %+v", field, frameEntry)
		}
	}

	firmwareEntry, ok := entries[firmwarePresetID]
	if !ok {
		t.Fatalf("GET /animations/catalog missing firmware preset %q: %+v", firmwarePresetID, body.Animations)
	}
	if got := firmwareEntry["kind"]; got != string(animations.PublicKindFirmwarePreset) {
		t.Fatalf("GET /animations/catalog firmware preset kind = %v, want %q", got, animations.PublicKindFirmwarePreset)
	}
	if got := firmwareEntry["playable"]; got != false {
		t.Fatalf("GET /animations/catalog firmware preset playable = %v, want false", got)
	}
	effectID, ok := firmwareEntry["effect_id"].(json.Number)
	if !ok || effectID.String() != "12" {
		t.Fatalf("GET /animations/catalog firmware preset effect_id = %v (%T), want JSON number 12", firmwareEntry["effect_id"], firmwareEntry["effect_id"])
	}
	interval, ok := firmwareEntry["interval"].(string)
	if !ok || interval != "90ms" {
		t.Fatalf("GET /animations/catalog firmware preset interval = %v (%T), want JSON string %q", firmwareEntry["interval"], firmwareEntry["interval"], "90ms")
	}
	color, ok := firmwareEntry["color"].(map[string]any)
	if !ok {
		t.Fatalf("GET /animations/catalog firmware preset color = %v (%T), want JSON object", firmwareEntry["color"], firmwareEntry["color"])
	}
	for channel, want := range map[string]string{"r": "0", "g": "255", "b": "85"} {
		got, ok := color[channel].(json.Number)
		if !ok || got.String() != want {
			t.Fatalf("GET /animations/catalog firmware preset color.%s = %v (%T), want JSON number %s", channel, color[channel], color[channel], want)
		}
	}
}
