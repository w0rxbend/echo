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
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
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
	logLevel := flag.String("log-level", "info", "log level: "+strings.Join(logLevelNames(), ", "))
	flag.Parse()

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		// There is no logger yet, and the whole point is that this must not pass
		// quietly: a typo used to start the process at info, so an operator
		// reaching for debug during an incident got no extra lines and no hint
		// why.
		fmt.Fprintln(os.Stderr, err)
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
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

func parseLogLevel(value string) (slog.Leveler, error) {
	if level, ok := logLevelParsers[value]; ok {
		return level, nil
	}
	return nil, fmt.Errorf("invalid -log-level %q: want one of %s", value, strings.Join(logLevelNames(), ", "))
}

func logLevelNames() []string {
	names := make([]string, 0, len(logLevelParsers))
	for name := range logLevelParsers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return logLevelParsers[names[i]].Level() < logLevelParsers[names[j]].Level()
	})
	return names
}
