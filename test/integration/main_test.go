//go:build integration

// Package integration runs repository and HTTP tests against a real PostgreSQL in a throwaway container.
//
// One container is started per test run. Migrations are applied once to a template database, and every test
// gets its own database cloned from that template, so tests are isolated and can run in parallel.
package integration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/onetodone/cinema-api/internal/platform/dbmigrate"
)

const (
	postgresImage = "postgres:18.4-alpine"
	templateDB    = "cinema_template"
)

var (
	baseDSN   string        // DSN of the template database; other databases differ only in the path
	adminPool *pgxpool.Pool // connected to the "postgres" database, used to create and drop test databases
	dbCounter atomic.Int64
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase(templateDB),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
		// Detect deadlocks after 100 ms instead of 1 s. The deadlock test finishes sooner, and if a change ever
		// breaks the lock order, the concurrency tests fail within seconds instead of crawling to the timeout.
		testcontainers.WithCmdArgs("-c", "deadlock_timeout=100ms"),
	)
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
		}
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		return 1
	}

	baseDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}

	if err := migrateTemplate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "migrate template database: %v\n", err)
		return 1
	}

	adminPool, err = pgxpool.New(ctx, dsnFor("postgres"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect admin pool: %v\n", err)
		return 1
	}
	defer adminPool.Close()

	return m.Run()
}

// migrateTemplate applies all migrations to the template database and closes every connection to it, because
// PostgreSQL refuses to copy a template that has open connections.
func migrateTemplate(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	return dbmigrate.Up(ctx, pool)
}

// dsnFor returns the DSN of another database on the test server.
func dsnFor(db string) string {
	u, err := url.Parse(baseDSN)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + db
	return u.String()
}

// newDB creates a fresh, migrated database for one test and drops it when the test ends.
func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return createDB(t, "TEMPLATE "+templateDB)
}

// newEmptyDB creates a database without any schema, for migration tests.
func newEmptyDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return createDB(t, "")
}

func createDB(t *testing.T, clause string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	name := fmt.Sprintf("test_%d_%d", os.Getpid(), dbCounter.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+name+" "+clause); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}

	pool, err := pgxpool.New(ctx, dsnFor(name))
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	return pool
}
