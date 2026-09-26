//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
)

// fixture is a small catalog: two movies and two halls.
type fixture struct {
	pool    *pgxpool.Pool
	catalog *postgres.Catalog
	dune    domain.Movie // 155 min
	short   domain.Movie // 90 min
	hall    domain.Hall  // rows A (2 standard), B (2 vip), AA (1 accessible)
	hall2   domain.Hall  // row A (3 standard)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	pool := newDB(t)
	f := &fixture{pool: pool, catalog: postgres.NewCatalog(pool)}

	f.dune = must(f.catalog.CreateMovie(ctx, domain.NewMovie{Title: "Dune", DurationMin: 155, AgeRating: "PG-13"}))(t)
	f.short = must(f.catalog.CreateMovie(ctx, domain.NewMovie{Title: "Short", DurationMin: 90}))(t)
	// Rows are created in a deliberately unsorted order to exercise the seat-map ordering.
	f.hall = must(f.catalog.CreateHall(ctx, "Hall 1", []domain.HallRow{
		{Label: "AA", Seats: 1, Type: domain.SeatAccessible},
		{Label: "B", Seats: 2, Type: domain.SeatVIP},
		{Label: "A", Seats: 2, Type: domain.SeatStandard},
	}))(t)
	f.hall2 = must(f.catalog.CreateHall(ctx, "Hall 2", []domain.HallRow{
		{Label: "A", Seats: 3, Type: domain.SeatStandard},
	}))(t)
	return f
}

// showtime schedules movie m in hall h at start, failing the test on error.
func (f *fixture) showtime(t *testing.T, m domain.Movie, h domain.Hall, start time.Time) domain.Showtime {
	t.Helper()
	st, err := f.catalog.CreateShowtime(t.Context(), domain.NewShowtime{
		MovieID: m.ID, HallID: h.ID, StartsAt: start, BasePriceCents: 1000,
	})
	if err != nil {
		t.Fatalf("create showtime: %v", err)
	}
	return st
}

// setSeatStatus marks one seat of a showtime as held or sold by a new booking, the way the booking flow will.
func (f *fixture) setSeatStatus(t *testing.T, showtimeID, seatID int64, status domain.SeatStatus) {
	t.Helper()
	ctx := t.Context()
	userID, bookingID := uuid.NewV7(), uuid.NewV7()

	exec(t, f.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, userID.String()+"@example.com")
	exec(t, f.pool, `
INSERT INTO bookings (id, user_id, showtime_id, total_cents, expires_at)
VALUES ($1, $2, $3, 0, now() + interval '15 minutes')`, bookingID, userID, showtimeID)
	tag, err := f.pool.Exec(ctx, `
UPDATE showtime_seats SET status = $1, booking_id = $2 WHERE showtime_id = $3 AND seat_id = $4`,
		string(status), bookingID, showtimeID, seatID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("set seat %d of showtime %d to %s: %v (rows %d)", seatID, showtimeID, status, err, tag.RowsAffected())
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// must unwraps a (value, error) pair; the returned function fails the test on error.
// Usage: v := must(repo.Create(ctx, x))(t).
func must[T any](v T, err error) func(t *testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
}

// domainCode returns the code of a domain error, or "" if err is not one.
func domainCode(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}
