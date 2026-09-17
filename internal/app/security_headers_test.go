package app_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/worxbend/echo/internal/app"
)

// TestEveryResponseCarriesTheSecurityHeaders checks the headers on the routes
// nobody thinks of as attack surface.
//
// /docs has carried them since it started serving Swagger UI, because /docs is
// obviously HTML. The rest -- readiness JSON, the Prometheus text exposition,
// the OpenAPI document -- went without, on the unstated assumption that only
// programs fetch them. They get opened in browsers by hand all the time, and
// nosniff is precisely the header that decides what a browser does when it
// distrusts the Content-Type it was given.
//
// The list deliberately includes routes that answer with an error. A header set
// only on the success path is a header missing from every response an attacker
// can actually provoke, so the 404 and the unauthorised API call are the
// interesting rows here, not the healthy ones.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}

	paths := []string{
		"/healthz",
		"/readyz",
		"/metrics",
		"/openapi.json",
		"/swagger.json",
		"/docs",
		"/api/v1/devices",
		"/api/v1/devices/nonexistent/play",
		"/no/such/route",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(httpServer.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			for header, value := range want {
				if got := resp.Header.Get(header); got != value {
					t.Errorf("%s (status %d): %s = %q, want %q", path, resp.StatusCode, header, got, value)
				}
			}
		})
	}
}

// TestDocsKeepsItsOwnContentSecurityPolicy guards the interaction between the
// blanket headers and the one handler that needs stricter ones. The middleware
// sets its values before the handler runs, so the handler's own Set replaces
// them rather than conflicting -- but only as long as nothing starts using Add,
// which would leave the docs page advertising two policies and let a browser
// enforce either.
func TestDocsKeepsItsOwnContentSecurityPolicy(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	application, err := app.New(newHTTPMatrixTestConfig(t, matrixServer.Addr()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL + "/docs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Values("Content-Security-Policy"); len(got) != 1 {
		t.Fatalf("/docs sent %d content-security-policy headers (%q), want exactly 1", len(got), got)
	}
	for _, header := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if got := resp.Header.Values(header); len(got) != 1 {
			t.Errorf("/docs sent %d %s headers (%q), want exactly 1", len(got), header, got)
		}
	}
}
