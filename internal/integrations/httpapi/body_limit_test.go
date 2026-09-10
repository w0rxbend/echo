package httpapi_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// maxRequestBodyBytes mirrors the unexported cap in the httpapi package. The
// tests below only need to straddle it, so a local copy is enough.
const maxRequestBodyBytes = 1 << 20

// TestOversizedRequestBodyIsRejected covers the body cap on every JSON ingress
// route, including the two that are deliberately reachable without an admin
// token. Before the cap, an unauthenticated caller could make the process buffer
// a body of any size before a single field was validated.
func TestOversizedRequestBodyIsRejected(t *testing.T) {
	// Comfortably past the cap, and valid JSON all the way to the closing brace
	// so nothing but the size can be what rejects it.
	oversized := fmt.Sprintf(`{"message":%q}`, strings.Repeat("x", maxRequestBodyBytes+4096))

	tests := []struct {
		name string
		path string
	}{
		{name: "notify is unauthenticated ingress", path: "/api/v1/devices/default/notify"},
		{name: "events is unauthenticated ingress", path: "/api/v1/devices/default/events"},
		{name: "play is an admin route", path: "/api/v1/devices/default/play"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpServer := newAnimationAPITestServer(t)

			resp, err := http.Post(httpServer.URL+tt.path, "application/json", bytes.NewBufferString(oversized))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("POST %s status = %d, body = %s, want %d",
					tt.path, resp.StatusCode, data, http.StatusRequestEntityTooLarge)
			}
			if !strings.Contains(string(data), "request body exceeds") {
				t.Fatalf("POST %s body = %s, want to name the size limit", tt.path, data)
			}
		})
	}
}

// TestBodyAtTheLimitIsStillDecoded guards against the cap being set so low, or
// applied so eagerly, that ordinary requests start failing. A request just under
// the limit must still reach normal validation rather than being rejected on size.
func TestBodyAtTheLimitIsStillDecoded(t *testing.T) {
	httpServer := newAnimationAPITestServer(t)

	// Large, but under the cap once the surrounding JSON is counted.
	body := fmt.Sprintf(`{"message":%q,"duration":"50ms"}`, strings.Repeat("x", maxRequestBodyBytes-4096))

	resp, err := http.Post(httpServer.URL+"/api/v1/devices/default/notify", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /notify status = %d, body = %s, want the request to pass the size check",
			resp.StatusCode, data)
	}
}
