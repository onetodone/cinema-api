// Command seed fills the database with demo movies, halls, and showtimes.
//
// It refuses to run on a database that already has movies, unless -reset is given. -reset truncates the catalog
// and every booking and payment that depends on it; users are kept.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed the time zone database, so CINEMA_TIMEZONE works on hosts without tzdata

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/seed"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	reset := flag.Bool("reset", false, "delete the existing catalog, bookings, and payments before seeding")
	days := flag.Int("days", 7, "number of days to schedule, starting today")
	flag.Parse()
	if *days < 1 {
		return fmt.Errorf("-days must be at least 1, got %d", *days)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := pgpool.New(ctx, cfg.DB, "cinema-seed")
	if err != nil {
		return err
	}
	defer pool.Close()

	if *reset {
		if err := resetCatalog(ctx, pool); err != nil {
			return err
		}
		fmt.Println("existing catalog deleted")
	} else {
		var seeded bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM movies)`).Scan(&seeded); err != nil {
			return fmt.Errorf("check existing data (did you run the migrations?): %w", err)
		}
		if seeded {
			fmt.Println("database already has movies; nothing to do (use -reset to start over)")
			return nil
		}
	}

	start := time.Now()
	stats, err := seed.Run(ctx, postgres.NewCatalog(pool), seed.Options{
		Days:     *days,
		FirstDay: time.Now().In(cfg.Cinema.Location),
		Location: cfg.Cinema.Location,
	})
	if err != nil {
		return err
	}

	fmt.Printf("seeded %d movies, %d halls (%d seats), %d showtimes over %d days in %s (%s)\n",
		stats.Movies, stats.Halls, stats.Seats, stats.Showtimes, *days, cfg.Cinema.Location,
		time.Since(start).Round(time.Millisecond))
	return nil
}

func resetCatalog(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
TRUNCATE payments, booking_seats, showtime_seats, bookings, showtimes, hall_seats, halls, movies
RESTART IDENTITY`)
	if err != nil {
		return fmt.Errorf("reset catalog: %w", err)
	}
	return nil
}
