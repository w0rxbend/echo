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
