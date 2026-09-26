// Command worker runs the background jobs of the cinema booking API: it expires unpaid bookings whose hold
// has run out and makes their seats available again.
//
// Several workers may run at once, for example one per replica: they split the work between them through
// row locks in the database and never expire a booking twice.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // embed the time zone database, so CINEMA_TIMEZONE works on hosts without tzdata

	"github.com/onetodone/cinema-api/internal/app"
	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cinema-worker: %v\n", err)
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

	w, err := app.NewWorker(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer w.Close()

	return w.Run(ctx)
}
