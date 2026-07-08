// Package main is the entry point for the LED Matrix Proxy server.
//
//	@title			LED Matrix Proxy
//	@version		1.0.0
//	@description	HTTP proxy for ESP8266 LED matrix controllers. Send events, rules pick animations, the scheduler owns the TCP connection.
//
//	@host		localhost:8080
//	@BasePath	/
//	@schemes	http https
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Required when server.addr is not a loopback address. Token value from the env var named by server.admin_token_env.
//
//	@tag.name			health
//	@tag.description	Liveness and readiness probes
//	@tag.name			events
//	@tag.description	Event ingress — device routes require a device ID in the path
//	@tag.name			animations
//	@tag.description	Animation discovery (global, device-independent)
//	@tag.name			device
//	@tag.description	Device-specific playback, presets, and controls (admin)

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

var logLevelParsers = map[string]slog.Leveler{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

func main() {
	if run() != nil {
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultPath, "path to YAML configuration file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(*logLevel),
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err, "path", *configPath)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("initialize app", "error", err)
		return err
	}

	if err := application.Run(ctx); err != nil {
		logger.Error("run app", "error", err)
		return err
	}
	return nil
}

func parseLogLevel(value string) slog.Leveler {
	if level, ok := logLevelParsers[value]; ok {
		return level
	}
	return slog.LevelInfo
}
