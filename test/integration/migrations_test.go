//go:build integration

package integration

import (
	"testing"

	"github.com/onetodone/cinema-api/internal/platform/dbmigrate"
)

// TestMigrationsRoundTrip applies every migration, rolls all of them back, and applies them again. It proves
// that each Down section really undoes its Up section.
func TestMigrationsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newEmptyDB(t)

	m, err := dbmigrate.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	tables := func() int {
		var n int
		err := pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.tables
WHERE table_schema = 'public' AND table_name <> 'goose_db_version'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	const wantTables = 10
	if got := tables(); got != wantTables {
		t.Fatalf("after up: %d tables, want %d", got, wantTables)
	}

	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range status {
		if s.AppliedAt.IsZero() {
			t.Errorf("migration %d is not applied", s.Source.Version)
		}
	}

	if _, err := m.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if got := tables(); got != 0 {
		t.Fatalf("after down: %d tables left, want 0", got)
	}
	var types int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = 'public' AND t.typtype = 'e'`).Scan(&types); err != nil {
		t.Fatal(err)
	}
	if types != 0 {
		t.Errorf("after down: %d enum types left, want 0", types)
	}

	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := tables(); got != wantTables {
		t.Fatalf("after second up: %d tables, want %d", got, wantTables)
	}
}
