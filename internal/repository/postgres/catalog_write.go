package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

// CreateMovie inserts a movie and returns it with its generated id.
func (c *Catalog) CreateMovie(ctx context.Context, nm domain.NewMovie) (domain.Movie, error) {
	m, err := scanMovie(c.pool.QueryRow(ctx, `
INSERT INTO movies (title, description, duration_min, age_rating, poster_url)
VALUES ($1, $2, $3, $4, $5)
RETURNING `+movieColumns,
		nm.Title, nm.Description, nm.DurationMin, nullable(nm.AgeRating), nullable(nm.PosterURL)))
	if err != nil {
		return domain.Movie{}, fmt.Errorf("create movie %q: %w", nm.Title, err)
	}
	return m, nil
}

// CreateHall inserts a hall and generates its seats row by row, in one transaction.
func (c *Catalog) CreateHall(ctx context.Context, name string, rows []domain.HallRow) (domain.Hall, error) {
	var hall domain.Hall
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO halls (name) VALUES ($1) RETURNING id, name`, name).
			Scan(&hall.ID, &hall.Name)
		if pgErrorCode(err) == sqlstateUniqueViolation {
			return domain.Conflict(domain.CodeHallNameTaken, "hall %q already exists", name)
		}
		if err != nil {
			return fmt.Errorf("insert hall: %w", err)
		}

		for _, r := range rows {
			_, err := tx.Exec(ctx, `
INSERT INTO hall_seats (hall_id, row_label, seat_number, seat_type)
SELECT $1, $2, n, $4::seat_type FROM generate_series(1, $3::int) AS n`,
				hall.ID, r.Label, r.Seats, string(r.Type))
			if err != nil {
				return fmt.Errorf("insert seats of row %q: %w", r.Label, err)
			}
		}
		return nil
	})
	if err != nil {
		return domain.Hall{}, fmt.Errorf("create hall %q: %w", name, err)
	}
	return hall, nil
}

// CreateShowtime schedules a showtime and materializes its seat inventory in one transaction.
// The end time is the start plus the movie duration plus domain.CleaningBuffer. Seat prices follow
// domain.SeatPrice. Overlapping another scheduled showtime in the same hall fails with HALL_OVERLAP.
func (c *Catalog) CreateShowtime(ctx context.Context, ns domain.NewShowtime) (domain.Showtime, error) {
	var created domain.Showtime
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `
INSERT INTO showtimes (movie_id, hall_id, starts_at, ends_at, base_price_cents)
SELECT m.id, $2, $3::timestamptz, $3::timestamptz + make_interval(mins => m.duration_min + $5), $4
FROM movies m
WHERE m.id = $1
RETURNING id`,
			ns.MovieID, ns.HallID, ns.StartsAt, ns.BasePriceCents, int(domain.CleaningBuffer.Minutes()),
		).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return domain.NotFound(domain.CodeMovieNotFound, "movie %d not found", ns.MovieID)
		case pgErrorCode(err) == sqlstateForeignKeyViolation:
			return domain.NotFound(domain.CodeHallNotFound, "hall %d not found", ns.HallID)
		case pgErrorCode(err) == sqlstateExclusionViolation:
			return domain.Conflict(domain.CodeHallOverlap,
				"hall %d already has a showtime overlapping %s", ns.HallID, ns.StartsAt.Format("2006-01-02 15:04 MST"))
		case err != nil:
			return fmt.Errorf("insert showtime: %w", err)
		}

		_, err = tx.Exec(ctx, `
INSERT INTO showtime_seats (showtime_id, seat_id, price_cents)
SELECT $1, hs.id, CASE WHEN hs.seat_type = 'vip' THEN $2 * (100 + $3) / 100 ELSE $2 END
FROM hall_seats hs
WHERE hs.hall_id = $4`,
			id, ns.BasePriceCents, domain.VIPSurchargePercent, ns.HallID)
		if err != nil {
			return fmt.Errorf("materialize seats of showtime %d: %w", id, err)
		}

		created, err = getShowtime(ctx, tx, id)
		return err
	})
	if err != nil {
		return domain.Showtime{}, fmt.Errorf("create showtime: %w", err)
	}
	return created, nil
}
