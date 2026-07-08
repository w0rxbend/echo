// Regenerate the OpenAPI 3.1 spec with:
//
//go:generate go run github.com/swaggo/swag/v2/cmd/swag@latest init --generalInfo cmd/matrix-proxy/main.go --dir ../../.. --output swaggerdocs --outputTypes json --parseInternal --v3.1
package httpapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
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
	specURL := fmt.Sprintf("%s://%s/openapi.json", scheme(r), r.Host)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, swaggerUITemplate, specURL)
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		return fwd
	}
	return "http"
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

const swaggerUITemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>LED Matrix Proxy — API Docs</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; }
    #swagger-ui .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({
      url: %q,
      dom_id: '#swagger-ui',
      presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
      layout: 'BaseLayout',
      deepLinking: true,
      tryItOutEnabled: true,
      persistAuthorization: true,
    });
  </script>
</body>
</html>`
