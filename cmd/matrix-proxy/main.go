// Package main is the entry point for the LED Matrix Proxy server.
//
//	@title			LED Matrix Proxy
//	@version		1.0.0
//	@description	HTTP proxy for ESP8266 LED matrix controllers. Send events, rules pick animations, the scheduler owns the TCP connection.
//	@host			localhost:8080
//	@BasePath		/
//	@schemes		http https
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Required when server.addr is not a loopback address. Set MATRIX_PROXY_ADMIN_TOKEN.
//
//	@tag.name			health
//	@tag.description	Liveness and readiness probes
//	@tag.name			events
//	@tag.description	Event ingress — device routes require a device ID in the path
//	@tag.name			animations
//	@tag.description	Animation discovery (global, device-independent)
//	@tag.name			device
//	@tag.description	Device-specific playback, presets, and controls (admin)

//go:generate go run github.com/swaggo/swag/cmd/swag@v1.16.6 init --generalInfo cmd/matrix-proxy/main.go --dir . --output internal/integrations/httpapi/swaggerdocs --outputTypes json --parseInternal
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/worxbend/echo/internal/app"
	"github.com/worxbend/echo/internal/config"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "path to YAML configuration file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(*logLevel),
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err, "path", *configPath)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("initialize app", "error", err)
		os.Exit(1)
	}

	if err := application.Run(ctx); err != nil {
		logger.Error("run app", "error", err)
		os.Exit(1)
	}
}

func parseLogLevel(value string) slog.Leveler {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
