package httpapi

import (
	_ "embed"
	"fmt"
	"net/http"
)

// openapiSpec is the hand-written OpenAPI 3.0 spec, served at /openapi.json.
//
//go:embed openapi.json
var openapiSpec []byte

// swaggerSpec is the Swagger 2.0 spec auto-generated from handler annotations.
// Regenerate with: go generate ./cmd/matrix-proxy
//
//go:embed swaggerdocs/swagger.json
var swaggerSpec []byte

// HandleOpenAPI serves the OpenAPI 3.0 spec (hand-maintained, full detail).
func (s *Server) HandleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiSpec)
}

// HandleSwagger serves the auto-generated Swagger 2.0 spec.
func (s *Server) HandleSwagger(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(swaggerSpec)
}

// HandleDocs serves Swagger UI loaded from the auto-generated spec.
// The spec is fetched from /swagger.json on the same origin.
func (s *Server) HandleDocs(w http.ResponseWriter, r *http.Request) {
	specURL := fmt.Sprintf("%s://%s/swagger.json", scheme(r), r.Host)
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
