package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

// movieGenres selects the genres of the movie aliased m as a JSON array of genreList, most characteristic first.
const movieGenres = `(
    SELECT coalesce(json_agg(json_build_object('id', g.id, 'slug', g.slug, 'name', g.name) ORDER BY mg.position), '[]')
    FROM movie_genres mg
    JOIN genres g ON g.id = mg.genre_id
    WHERE mg.movie_id = m.id)`

// genreList is the JSON that movieGenres selects.
type genreList []struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// domain returns the genres. The result is never nil.
func (l genreList) domain() []domain.Genre {
	genres := make([]domain.Genre, len(l))
	for i, g := range l {
		genres[i] = domain.Genre{ID: g.ID, Slug: g.Slug, Name: g.Name}
	}
	return genres
}

const genreColumns = `id, slug, name`

func scanGenre(row pgx.Row) (domain.Genre, error) {
	var g domain.Genre
	err := row.Scan(&g.ID, &g.Slug, &g.Name)
	return g, err
}

// ListGenres returns every genre, ordered by name regardless of case, then by id.
func (c *Catalog) ListGenres(ctx context.Context) ([]domain.Genre, error) {
	rows, err := c.pool.Query(ctx, `SELECT `+genreColumns+` FROM genres ORDER BY lower(name), id`)
	if err != nil {
		return nil, fmt.Errorf("list genres: %w", err)
	}
	genres, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Genre, error) { return scanGenre(row) })
	if err != nil {
		return nil, fmt.Errorf("list genres: %w", err)
	}
	return genres, nil
}

// GetGenre returns one genre or a GENRE_NOT_FOUND error.
func (c *Catalog) GetGenre(ctx context.Context, id int64) (domain.Genre, error) {
	g, err := scanGenre(c.pool.QueryRow(ctx, `SELECT `+genreColumns+` FROM genres WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Genre{}, genreNotFound(id)
	}
	if err != nil {
		return domain.Genre{}, fmt.Errorf("get genre %d: %w", id, err)
	}
	return g, nil
}

// CreateGenre inserts a genre and returns it with its generated id. A slug or name (regardless of case) that
// another genre has fails with GENRE_SLUG_TAKEN or GENRE_NAME_TAKEN.
func (c *Catalog) CreateGenre(ctx context.Context, ng domain.NewGenre) (domain.Genre, error) {
	g, err := scanGenre(c.pool.QueryRow(ctx,
		`INSERT INTO genres (slug, name) VALUES ($1, $2) RETURNING `+genreColumns, ng.Slug, ng.Name))
	if err != nil {
		return domain.Genre{}, genreWriteError(err, ng, "create genre")
	}
	return g, nil
}

// UpdateGenre replaces the slug and name of a genre. It fails with GENRE_NOT_FOUND, or as CreateGenre does.
func (c *Catalog) UpdateGenre(ctx context.Context, id int64, ng domain.NewGenre) (domain.Genre, error) {
	g, err := scanGenre(c.pool.QueryRow(ctx,
		`UPDATE genres SET slug = $2, name = $3 WHERE id = $1 RETURNING `+genreColumns, id, ng.Slug, ng.Name))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Genre{}, genreNotFound(id)
	}
	if err != nil {
		return domain.Genre{}, genreWriteError(err, ng, fmt.Sprintf("update genre %d", id))
	}
	return g, nil
}

// DeleteGenre deletes a genre. It fails with GENRE_NOT_FOUND, or with GENRE_IN_USE while a movie has the genre:
// the foreign key of movie_genres refuses the delete.
func (c *Catalog) DeleteGenre(ctx context.Context, id int64) error {
	tag, err := c.pool.Exec(ctx, `DELETE FROM genres WHERE id = $1`, id)
	switch {
	case pgErrorCode(err) == sqlstateForeignKeyViolation:
		return domain.Conflict(domain.CodeGenreInUse, "genre %d is used by movies; remove it from them first", id)
	case err != nil:
		return fmt.Errorf("delete genre %d: %w", id, err)
	case tag.RowsAffected() == 0:
		return genreNotFound(id)
	}
	return nil
}

// SetMovieGenres replaces the genres of a movie with genreIDs, most characteristic first, and returns the movie.
// It fails with MOVIE_NOT_FOUND, or with a *domain.ValidationError naming the ids that no genre has.
//
// The movie row is locked first, so two replacements for one movie run one after the other instead of colliding
// on the unique positions.
func (c *Catalog) SetMovieGenres(ctx context.Context, movieID int64, genreIDs []int64) (domain.Movie, error) {
	var m domain.Movie
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		// NO KEY UPDATE: new showtimes of the movie, whose foreign key takes a KEY SHARE lock, do not wait.
		err := tx.QueryRow(ctx, `SELECT id FROM movies WHERE id = $1 FOR NO KEY UPDATE`, movieID).Scan(&movieID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound(domain.CodeMovieNotFound, "movie %d not found", movieID)
		}
		if err != nil {
			return fmt.Errorf("lock movie: %w", err)
		}
		if err := setMovieGenres(ctx, tx, movieID, genreIDs); err != nil {
			return err
		}
		m, err = getMovie(ctx, tx, movieID)
		return err
	})
	if err != nil {
		return domain.Movie{}, fmt.Errorf("set genres of movie %d: %w", movieID, err)
	}
	return m, nil
}

// setMovieGenres replaces the genres of a movie inside tx. The genres are locked FOR KEY SHARE while they are
// checked, so a concurrent delete of one of them either waits for tx and then fails with GENRE_IN_USE, or
// commits first and the id is reported unknown here.
func setMovieGenres(ctx context.Context, tx pgx.Tx, movieID int64, genreIDs []int64) error {
	ids := genreIDs
	if ids == nil {
		ids = []int64{} // nil would be NULL
	}
	rows, err := tx.Query(ctx, `SELECT id FROM genres WHERE id = ANY ($1) ORDER BY id FOR KEY SHARE`, ids)
	if err != nil {
		return fmt.Errorf("lock genres: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("lock genres: %w", err)
	}
	known := make(map[int64]bool, len(found))
	for _, id := range found {
		known[id] = true
	}
	if err := domain.UnknownGenres(ids, known); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM movie_genres WHERE movie_id = $1`, movieID); err != nil {
		return fmt.Errorf("clear genres: %w", err)
	}
	_, err = tx.Exec(ctx, `
INSERT INTO movie_genres (movie_id, genre_id, position)
SELECT $1, u.id, u.position FROM unnest($2::bigint[]) WITH ORDINALITY AS u (id, position)`, movieID, ids)
	if err != nil {
		return fmt.Errorf("insert genres: %w", err)
	}
	return nil
}

// genreWriteError maps a unique violation of an insert or update of ng to a domain conflict.
func genreWriteError(err error, ng domain.NewGenre, op string) error {
	switch {
	case isViolation(err, sqlstateUniqueViolation, "genres_slug_uq"):
		return domain.Conflict(domain.CodeGenreSlugTaken, "another genre has the slug %q", ng.Slug)
	case isViolation(err, sqlstateUniqueViolation, "genres_name_uq"):
		return domain.Conflict(domain.CodeGenreNameTaken, "another genre has the name %q", ng.Name)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func genreNotFound(id int64) error {
	return domain.NotFound(domain.CodeGenreNotFound, "genre %d not found", id)
}
