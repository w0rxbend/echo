// Regenerate the OpenAPI 3.1 spec with:
//
// The swag version is pinned rather than @latest: CI fails the build on any
// diff in swaggerdocs/, so a floating generator would turn unrelated pull
// requests red the day swag changes its output.
//
//go:generate go run github.com/swaggo/swag/v2/cmd/swag@v2.0.0-rc6 init --generalInfo cmd/matrix-proxy/main.go --dir ../../.. --output swaggerdocs --outputTypes json --parseInternal --v3.1
package httpapi

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

//go:embed swaggerdocs/swagger.json
var rawSpec []byte

// openAPISpec holds embedded OpenAPI data with startup parse status.
type openAPISpec struct {
	doc map[string]any
	err error
}

func loadOpenAPISpec() openAPISpec {
	var doc map[string]any
	if err := json.Unmarshal(rawSpec, &doc); err != nil {
		return openAPISpec{err: fmt.Errorf("httpapi: failed to parse embedded OpenAPI spec: %w", err)}
	}
	// Remove pre-generated servers block — HandleOpenAPI injects it per-request.
	delete(doc, "servers")
	return openAPISpec{doc: doc}
}

func (o openAPISpec) resolve() (map[string]any, error) {
	if o.doc == nil {
		if o.err != nil {
			return nil, o.err
		}
		return nil, fmt.Errorf("httpapi: embedded OpenAPI spec is unavailable")
	}
	return o.doc, nil
}

func logOpenAPISpecError(logger *slog.Logger, err error) {
	if logger == nil {
		return
	}
	logger.Error("httpapi: failed to initialize embedded OpenAPI spec", "error", err)
}

// HandleOpenAPI serves the OpenAPI 3.1 spec with the server URL reflecting the
// actual request origin (scheme + host).
func (s *Server) HandleOpenAPI(w http.ResponseWriter, r *http.Request) {
	docBase, err := s.openapiSpec.resolve()
	if err != nil {
		logOpenAPISpecError(s.logger, err)
		writeError(w, http.StatusInternalServerError, "openapi specification is unavailable")
		return
	}
	doc := cloneTopLevel(docBase)
	doc["servers"] = []any{
		map[string]any{"url": fmt.Sprintf("%s://%s", scheme(r), r.Host)},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// HandleSwagger serves the same OpenAPI 3.1 spec at /swagger.json for
// backward-compatibility and Swagger UI consumption.
func (s *Server) HandleSwagger(w http.ResponseWriter, r *http.Request) {
	s.HandleOpenAPI(w, r)
}

// HandleDocs serves Swagger UI backed by the spec at /openapi.json.
func (s *Server) HandleDocs(w http.ResponseWriter, r *http.Request) {
	// A same-origin relative URL, not one built from the request. The template
	// interpolates into a JS string with %q, which escapes quotes but not
	// "</script>", and both r.Host and X-Forwarded-Proto are attacker-controlled
	// -- so building the URL from them put a reflected XSS in the docs page.
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", swaggerUICSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	_, _ = io.WriteString(w, swaggerUI)
}

// scheme reports the scheme to advertise in the spec's "servers" entry.
//
// X-Forwarded-Proto is set by whatever spoke to us last, so only the two values
// that mean anything here are honoured; anything else is treated as absent.
func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	switch r.Header.Get("X-Forwarded-Proto") {
	case "https":
		return "https"
	case "http":
		return "http"
	default:
		return "http"
	}
}

// cloneTopLevel returns a shallow copy of the top-level map so per-request
// mutations (injecting "servers") don't race with concurrent requests.
func cloneTopLevel(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Swagger UI is served from a CDN, pinned to an exact version and checked
// against a Subresource Integrity hash. A floating "@5" range means whatever
// unpkg decides to serve executes as first-party JavaScript on this origin,
// with no way for the browser to tell a genuine 5.x release from a tampered
// one -- which would hand back the cross-site scripting that HandleDocs above
// exists to prevent. With the hash, altered bytes are a hard failure instead.
//
// Bumping the version means recomputing both hashes:
//
//	curl -sS -L https://unpkg.com/swagger-ui-dist@<ver>/<file> |
//	  openssl dgst -sha384 -binary | openssl base64 -A
const (
	swaggerUIVersion      = "5.33.0"
	swaggerUICSSIntegrity = "sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW"
	swaggerUIJSIntegrity  = "sha384-YDALVcy8kj8yltLBVi1vBiBAUqdxvus673gM8XKwiy6aDUJFXivF/KCufekjYbVf"
)

// swaggerUIScript is the page's only inline script, kept separate so its
// content-security-policy hash is derived from the exact bytes that ship.
// Editing it cannot leave the policy behind.
//
// presets holds only SwaggerUIBundle.presets.apis. It used to also list
// SwaggerUIBundle.SwaggerUIStandalonePreset, which is undefined: that preset
// lives in swagger-ui-standalone-preset.js, a file this page never loaded, and
// the bundle does not re-export it. Loading it would only add the topbar that
// the stylesheet below hides, and BaseLayout does not use it either.
const swaggerUIScript = `
    SwaggerUIBundle({
      url: "/openapi.json",
      dom_id: '#swagger-ui',
      presets: [SwaggerUIBundle.presets.apis],
      layout: 'BaseLayout',
      deepLinking: true,
      tryItOutEnabled: true,
      persistAuthorization: true,
    });
  `

var swaggerUI = fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>LED Matrix Proxy — API Docs</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@%[1]s/swagger-ui.css" integrity="%[2]s" crossorigin="anonymous">
  <style>
    body { margin: 0; }
    #swagger-ui .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@%[1]s/swagger-ui-bundle.js" integrity="%[3]s" crossorigin="anonymous"></script>
  <script>%[4]s</script>
</body>
</html>`, swaggerUIVersion, swaggerUICSSIntegrity, swaggerUIJSIntegrity, swaggerUIScript)

// swaggerUICSP locks the docs page down to the one CDN it needs. Integrity
// hashes only help for the tags that carry them; without a policy, script
// injected by any other route still runs.
//
// style-src keeps 'unsafe-inline' because Swagger UI is React and sets style
// attributes on the elements it renders, which no hash can cover.
var swaggerUICSP = buildSwaggerUICSP()

func buildSwaggerUICSP() string {
	sum := sha256.Sum256([]byte(swaggerUIScript))
	inline := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	return "default-src 'none'; " +
		"script-src https://unpkg.com " + inline + "; " +
		"style-src https://unpkg.com 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"font-src 'self' data:; " +
		"connect-src 'self'; " +
		"base-uri 'none'; " +
		"form-action 'none'; " +
		"frame-ancestors 'none'"
}
