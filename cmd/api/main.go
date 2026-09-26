// Command api runs the cinema booking HTTP API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/onetodone/cinema-api/internal/app"
	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cinema-api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second Ctrl+C kills the process immediately.
	go func() {
		<-ctx.Done()
		stop()
	}()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger, err := logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	api, err := app.NewAPI(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer api.Close()

	return api.Run(ctx)
}
