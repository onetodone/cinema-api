package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Catalog reads and writes movies, halls, showtimes, and showtime seat inventory.
type Catalog struct {
	pool *pgxpool.Pool
}

// NewCatalog returns a Catalog repository backed by pool.
func NewCatalog(pool *pgxpool.Pool) *Catalog {
	return &Catalog{pool: pool}
}

const movieColumns = `id, title, description, duration_min, age_rating, poster_url, created_at`

func scanMovie(row pgx.Row) (domain.Movie, error) {
	var (
		m                    domain.Movie
		ageRating, posterURL *string
	)
	err := row.Scan(&m.ID, &m.Title, &m.Description, &m.DurationMin, &ageRating, &posterURL, &m.CreatedAt)
	m.AgeRating, m.PosterURL = deref(ageRating), deref(posterURL)
	return m, err
}

// ListMovies returns up to limit movies with an id greater than afterID, ordered by id (keyset pagination).
func (c *Catalog) ListMovies(ctx context.Context, afterID int64, limit int) ([]domain.Movie, error) {
	rows, err := c.pool.Query(ctx,
		`SELECT `+movieColumns+` FROM movies WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list movies: %w", err)
	}
	movies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Movie, error) { return scanMovie(row) })
	if err != nil {
		return nil, fmt.Errorf("list movies: %w", err)
	}
	return movies, nil
}

// GetMovie returns one movie or a MOVIE_NOT_FOUND error.
func (c *Catalog) GetMovie(ctx context.Context, id int64) (domain.Movie, error) {
	m, err := scanMovie(c.pool.QueryRow(ctx, `SELECT `+movieColumns+` FROM movies WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Movie{}, domain.NotFound(domain.CodeMovieNotFound, "movie %d not found", id)
	}
	if err != nil {
		return domain.Movie{}, fmt.Errorf("get movie %d: %w", id, err)
	}
	return m, nil
}

// showtimeSelect joins a showtime with its movie, hall, and live seat counts.
const showtimeSelect = `
SELECT s.id, s.starts_at, s.ends_at, s.base_price_cents, s.status,
       m.id, m.title, m.duration_min, m.age_rating,
       h.id, h.name,
       inv.available, inv.total
FROM showtimes s
JOIN movies m ON m.id = s.movie_id
JOIN halls  h ON h.id = s.hall_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE ss.status = 'available') AS available,
           count(*)                                        AS total
    FROM showtime_seats ss
    WHERE ss.showtime_id = s.id
) inv`

func scanShowtime(row pgx.Row) (domain.Showtime, error) {
	var (
		s         domain.Showtime
		ageRating *string
	)
	err := row.Scan(
		&s.ID, &s.StartsAt, &s.EndsAt, &s.BasePriceCents, &s.Status,
		&s.Movie.ID, &s.Movie.Title, &s.Movie.DurationMin, &ageRating,
		&s.Hall.ID, &s.Hall.Name,
		&s.SeatsAvailable, &s.SeatsTotal,
	)
	s.Movie.AgeRating = deref(ageRating)
	return s, err
}

// ListShowtimes returns scheduled showtimes that start in [f.From, f.To), ordered by start time.
func (c *Catalog) ListShowtimes(ctx context.Context, f domain.ShowtimeFilter) ([]domain.Showtime, error) {
	var limit *int // NULL means no limit
	if f.Limit > 0 {
		limit = &f.Limit
	}

	rows, err := c.pool.Query(ctx, showtimeSelect+`
WHERE s.status = 'scheduled'
  AND s.starts_at >= $1 AND s.starts_at < $2
  AND ($3::bigint = 0 OR s.movie_id = $3)
ORDER BY s.starts_at, h.name, s.id
LIMIT $4::int`, f.From, f.To, f.MovieID, limit)
	if err != nil {
		return nil, fmt.Errorf("list showtimes: %w", err)
	}
	showtimes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Showtime, error) {
		return scanShowtime(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list showtimes: %w", err)
	}
	return showtimes, nil
}

// GetShowtime returns one showtime in any status, or a SHOWTIME_NOT_FOUND error.
func (c *Catalog) GetShowtime(ctx context.Context, id int64) (domain.Showtime, error) {
	return getShowtime(ctx, c.pool, id)
}

func getShowtime(ctx context.Context, q querier, id int64) (domain.Showtime, error) {
	s, err := scanShowtime(q.QueryRow(ctx, showtimeSelect+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Showtime{}, domain.NotFound(domain.CodeShowtimeNotFound, "showtime %d not found", id)
	}
	if err != nil {
		return domain.Showtime{}, fmt.Errorf("get showtime %d: %w", id, err)
	}
	return s, nil
}

// ListShowtimeSeats returns the seat map of a showtime, ordered by row and seat number.
// Rows sort by label length first, so "AA" comes after "Z".
func (c *Catalog) ListShowtimeSeats(ctx context.Context, showtimeID int64) ([]domain.ShowtimeSeat, error) {
	rows, err := c.pool.Query(ctx, `
SELECT hs.id, hs.row_label, hs.seat_number, hs.seat_type, ss.price_cents, ss.status
FROM showtime_seats ss
JOIN hall_seats hs ON hs.id = ss.seat_id
WHERE ss.showtime_id = $1
ORDER BY length(hs.row_label), hs.row_label, hs.seat_number`, showtimeID)
	if err != nil {
		return nil, fmt.Errorf("list seats of showtime %d: %w", showtimeID, err)
	}
	seats, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ShowtimeSeat, error) {
		var s domain.ShowtimeSeat
		err := row.Scan(&s.SeatID, &s.Row, &s.Number, &s.Type, &s.PriceCents, &s.Status)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("list seats of showtime %d: %w", showtimeID, err)
	}
	return seats, nil
}
