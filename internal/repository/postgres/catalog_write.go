package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

// CreateMovie inserts a movie with its genres in one transaction and returns it with its generated id. Genre
// ids that no genre has fail with a *domain.ValidationError.
func (c *Catalog) CreateMovie(ctx context.Context, nm domain.NewMovie) (domain.Movie, error) {
	var m domain.Movie
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `
INSERT INTO movies (title, description, duration_min, age_rating, poster_url)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`,
			nm.Title, nm.Description, nm.DurationMin, nullable(nm.AgeRating), nullable(nm.PosterURL)).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert movie: %w", err)
		}
		if err := setMovieGenres(ctx, tx, id, nm.GenreIDs); err != nil {
			return err
		}
		m, err = getMovie(ctx, tx, id)
		return err
	})
	if err != nil {
		return domain.Movie{}, fmt.Errorf("create movie %q: %w", nm.Title, err)
	}
	return m, nil
}

// CreateHall inserts a hall and generates its seats row by row, in one transaction, and returns the hall with
// its seats in seat map order. A name that another hall has fails with HALL_NAME_TAKEN.
func (c *Catalog) CreateHall(ctx context.Context, name string, rows []domain.HallRow) (domain.HallLayout, error) {
	var hall domain.HallLayout
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
			seatRows, err := tx.Query(ctx, `
INSERT INTO hall_seats (hall_id, row_label, seat_number, seat_type)
SELECT $1, $2, n, $4::seat_type FROM generate_series(1, $3::int) AS n
RETURNING id, row_label, seat_number, seat_type`,
				hall.ID, r.Label, r.Seats, string(r.Type))
			if err != nil {
				return fmt.Errorf("insert seats of row %q: %w", r.Label, err)
			}
			seats, err := pgx.CollectRows(seatRows, func(row pgx.CollectableRow) (domain.HallSeat, error) {
				var s domain.HallSeat
				err := row.Scan(&s.ID, &s.Row, &s.Number, &s.Type)
				return s, err
			})
			if err != nil {
				return fmt.Errorf("insert seats of row %q: %w", r.Label, err)
			}
			hall.Seats = append(hall.Seats, seats...)
		}
		return nil
	})
	if err != nil {
		return domain.HallLayout{}, fmt.Errorf("create hall %q: %w", name, err)
	}
	slices.SortFunc(hall.Seats, func(a, b domain.HallSeat) int {
		return domain.CompareSeatPositions(a.Row, a.Number, b.Row, b.Number)
	})
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
INSERT INTO showtimes (movie_id, hall_id, starts_at, ends_at, base_price_cents, audio_language, subtitle_language)
SELECT m.id, $2, $3::timestamptz, $3::timestamptz + make_interval(mins => m.duration_min + $5), $4, $6, $7
FROM movies m
WHERE m.id = $1
RETURNING id`,
			ns.MovieID, ns.HallID, ns.StartsAt, ns.BasePriceCents, int(domain.CleaningBuffer.Minutes()),
			ns.Language.Audio, nullable(ns.Language.Subtitles),
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
