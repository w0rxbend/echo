package httpapi_test

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/integrations/httpapi"
	"github.com/worxbend/echo/internal/matrix"
)

func newDocsResponse(t *testing.T) *http.Response {
	t.Helper()

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(httpapi.Options{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Bus:        newTestBus(t, 4),
		Schedulers: map[string]*matrix.Scheduler{"default": nil},
		Registry:   registry,
		ServerAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	api.HandleDocs(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	return rec.Result()
}

func docsBody(t *testing.T) string {
	t.Helper()
	resp := newDocsResponse(t)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A CDN tag without an integrity hash lets whoever serves it run arbitrary
// JavaScript on this origin, and a floating version range means that is a
// routine release rather than a break-in.
func TestDocsPinsEveryCDNAssetWithIntegrity(t *testing.T) {
	body := docsBody(t)

	tag := regexp.MustCompile(`<(?:script|link)\b[^>]*\b(?:src|href)="(https://[^"]+)"[^>]*>`)
	matches := tag.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("no remote assets found; the docs page is expected to load Swagger UI from a CDN")
	}
	for _, m := range matches {
		element, url := m[0], m[1]
		if strings.Contains(url, "@5/") || !regexp.MustCompile(`@\d+\.\d+\.\d+/`).MatchString(url) {
			t.Errorf("%s is not pinned to an exact version", url)
		}
		if !strings.Contains(element, `integrity="sha384-`) {
			t.Errorf("%s has no subresource integrity hash", url)
		}
		if !strings.Contains(element, `crossorigin="anonymous"`) {
			// Without it the response is opaque and the hash is never checked.
			t.Errorf("%s has an integrity hash but no crossorigin attribute", url)
		}
	}
}

// A content-security-policy hash covers the element's content byte for byte,
// so it is the template that can break this, not the script: inserting the
// script with so much as a newline either side of it leaves the policy hashing
// bytes the browser never sees, and the page silently renders blank. Editing
// the script itself is safe, because the hash is derived from it at startup --
// which this also confirms, since a hash hardcoded as a literal would not
// survive here either.
func TestDocsCSPHashMatchesTheInlineScript(t *testing.T) {
	resp := newDocsResponse(t)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	inline := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(body), -1)
	if len(inline) != 1 {
		t.Fatalf("found %d inline scripts, want exactly 1", len(inline))
	}
	sum := sha256.Sum256([]byte(inline[0][1]))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, want) {
		t.Errorf("Content-Security-Policy does not cover the inline script\n got: %s\nwant it to contain: %s", csp, want)
	}
}

func TestDocsSetsSecurityHeaders(t *testing.T) {
	resp := newDocsResponse(t)
	defer resp.Body.Close()

	for header, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'none'",
		"script-src https://unpkg.com ",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("Content-Security-Policy is missing %q\ngot: %s", directive, csp)
		}
	}
}

// SwaggerUIStandalonePreset lives in a file this page does not load, so naming
// it only put undefined into the presets array.
func TestDocsReferencesNoUnloadedGlobals(t *testing.T) {
	body := docsBody(t)

	if strings.Contains(body, "SwaggerUIStandalonePreset") && !strings.Contains(body, "swagger-ui-standalone-preset.js") {
		t.Error("the page uses SwaggerUIStandalonePreset without loading swagger-ui-standalone-preset.js")
	}
}

// The spec URL is relative on purpose: it used to be built from r.Host and
// X-Forwarded-Proto, both attacker-controlled, and %q does not escape
// "</script>".
func TestDocsSpecURLStaysRelative(t *testing.T) {
	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(httpapi.Options{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Bus:        newTestBus(t, 4),
		Schedulers: map[string]*matrix.Scheduler{"default": nil},
		Registry:   registry,
		ServerAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	req.Host = `evil"</script><script>alert(1)</script>`
	req.Header.Set("X-Forwarded-Proto", `javascript:`)

	rec := httptest.NewRecorder()
	api.HandleDocs(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, "evil") || strings.Contains(body, "alert(1)") {
		t.Errorf("request-controlled input reached the docs page:\n%s", body)
	}
	if !strings.Contains(body, `url: "/openapi.json"`) {
		t.Error("the docs page no longer points at the same-origin spec URL")
	}
}
