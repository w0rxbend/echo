package app_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/worxbend/echo/internal/app"
)

const testCommandSetBrightness byte = 0x02

// The configured brightness only matters if it reaches the panel. It is parsed,
// defaulted and documented, so a regression here is invisible from the config
// file alone: the operator's setting would simply never be sent.
func TestDeviceBrightnessIsAppliedToMatrixAtStartup(t *testing.T) {
	matrixServer := newFakeESPServer(t)
	defer matrixServer.Close()

	cfg := newHTTPMatrixTestConfig(t, matrixServer.Addr())
	cfg.Devices["default"].Brightness = 7

	application, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runAppWorkers(t, application, ctx)
	defer func() {
		cancel()
		waitAppWorkers(t, done)
	}()

	frame := waitForMatrixCommand(t, matrixServer, testCommandSetBrightness)
	if len(frame.Payload) != 1 || frame.Payload[0] != 7 {
		t.Fatalf("set_brightness payload = %v, want [7]", frame.Payload)
	}
}
