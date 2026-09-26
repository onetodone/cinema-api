// Package dbmigrate applies the embedded schema migrations with goose.
package dbmigrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/onetodone/cinema-api/migrations"
)

// Migrator runs schema migrations against one database.
type Migrator struct {
	provider *goose.Provider
}

// New builds a Migrator on top of an existing pool. A PostgreSQL advisory lock serializes migrations, so several
// processes can call Up at the same time safely. Call Close when done; it does not close the pool.
func New(pool *pgxpool.Pool) (*Migrator, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("create migration lock: %w", err)
	}

	db := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create migration provider: %w", err), db.Close())
	}
	return &Migrator{provider: provider}, nil
}

// Up applies all pending migrations and returns the ones it applied.
func (m *Migrator) Up(ctx context.Context) ([]*goose.MigrationResult, error) {
	results, err := m.provider.Up(ctx)
	if err != nil {
		return results, fmt.Errorf("migrate up: %w", err)
	}
	return results, nil
}

// Down rolls back the most recent migration.
func (m *Migrator) Down(ctx context.Context) (*goose.MigrationResult, error) {
	result, err := m.provider.Down(ctx)
	if err != nil {
		return result, fmt.Errorf("migrate down: %w", err)
	}
	return result, nil
}

// DownTo rolls back migrations until the schema is at version (0 removes everything).
func (m *Migrator) DownTo(ctx context.Context, version int64) ([]*goose.MigrationResult, error) {
	results, err := m.provider.DownTo(ctx, version)
	if err != nil {
		return results, fmt.Errorf("migrate down to %d: %w", version, err)
	}
	return results, nil
}

// Status reports every known migration and whether it is applied.
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	status, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration status: %w", err)
	}
	return status, nil
}

// Close releases the database/sql handle that wraps the pool.
func (m *Migrator) Close() error {
	return m.provider.Close()
}

// Up is a shortcut that applies all pending migrations on pool.
func Up(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := New(pool)
	if err != nil {
		return err
	}
	_, upErr := m.Up(ctx)
	return errors.Join(upErr, m.Close())
}
