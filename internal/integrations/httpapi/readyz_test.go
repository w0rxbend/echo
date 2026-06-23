package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/app"
)

func TestReadyzAndMetricsProjectGeneratedBackgroundKind(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	const backgroundID = "generated_background"
	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Background.Animation = backgroundID
	cfg.Background.RestoreOnIdle = true

	registry, err := animations.NewDefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterGenerated(backgroundID, "test_generated_background", animations.AnimationFunc(
		func(context.Context, animations.Params) ([]animations.Frame, error) {
			frame := animations.Frame{Delay: 20 * time.Millisecond}
			frame.Pixels[0] = animations.RGB{R: 255}
			return []animations.Frame{frame}, nil
		},
	)); err != nil {
		t.Fatal(err)
	}
	cfg.AnimationRegistry = registry

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := runAppWorkers(t, application)
	defer shutdownAppWorkers(t, application, done)

	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	ready := waitForReadyzBackground(t, httpServer.URL, backgroundID, "generated", "converged")
	if ready.Status != "ready" {
		t.Fatalf("/readyz status = %q, want ready: %#v", ready.Status, ready)
	}

	body := getReadyzBody(t, httpServer.URL)
	if !strings.Contains(body, `"kind":"generated"`) {
		t.Fatalf("/readyz response missing public generated background kind:\n%s", body)
	}
	if strings.Contains(body, `"kind":"renderable"`) {
		t.Fatalf("/readyz response leaked internal renderable background kind:\n%s", body)
	}

	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total", `kind="generated"`)
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_dirty", `kind="generated"`, " 0")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_converged", `kind="generated"`, " 1")
	waitForMetricLine(t, httpServer.URL, "matrix_proxy_background_state", `kind="generated"`, `state="converged"`, " 1")
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_restore_attempts_total", `kind="renderable"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_dirty", `kind="renderable"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_converged", `kind="renderable"`)
	assertNoMetricLine(t, httpServer.URL, "matrix_proxy_background_state", `kind="renderable"`)
}

func waitForReadyzBackground(t *testing.T, baseURL, wantID, wantKind, wantState string) readyDetails {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last readyDetails
	for time.Now().Before(deadline) {
		last = waitForReadyDetails(t, baseURL+"/readyz", http.StatusOK)
		if last.Background.ConfiguredID == wantID &&
			last.Background.Kind == wantKind &&
			last.Background.State == wantState {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/readyz background = %#v, want configured_id=%q kind=%q state=%q", last.Background, wantID, wantKind, wantState)
	return readyDetails{}
}

func getReadyzBody(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, body = %s", resp.StatusCode, data)
	}
	return string(data)
}
