// Command migrate applies or rolls back the embedded database schema migrations.
//
// Usage: migrate up | down | status
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/dbmigrate"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
)

const usage = `usage: migrate <command>

commands:
  up      apply all pending migrations
  down    roll back the most recent migration
  status  list migrations and whether they are applied`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return errors.New("expected exactly one command")
	}
	command := flag.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := pgpool.New(ctx, cfg.DB, "cinema-migrate")
	if err != nil {
		return err
	}
	defer pool.Close()

	m, err := dbmigrate.New(pool)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	switch command {
	case "up":
		results, err := m.Up(ctx)
		for _, r := range results {
			fmt.Println(r)
		}
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("no pending migrations")
		}
	case "down":
		result, err := m.Down(ctx)
		if result != nil {
			fmt.Println(result)
		}
		return err
	case "status":
		status, err := m.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range status {
			applied := "pending"
			if !s.AppliedAt.IsZero() {
				applied = "applied " + s.AppliedAt.UTC().Format(time.DateTime) + " UTC"
			}
			fmt.Printf("%05d  %-28s %s\n", s.Source.Version, s.Source.Path, applied)
		}
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
	return nil
}
