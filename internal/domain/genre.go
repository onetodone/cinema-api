package domain

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// Genre classifies a movie. Admins manage the genres; clients refer to one by ID.
type Genre struct {
	ID   int64
	Slug string // unique, such as science_fiction; for URLs and filters
	Name string // unique regardless of case, such as Science fiction
}

// NewGenre holds the fields of a genre to create, or to replace those of an existing one.
type NewGenre struct {
	Slug string
	Name string
}

// Genre field limits.
const (
	MaxGenreSlugLength = 50
	MaxGenreNameLength = 100
)

// genreSlug is lowercase letters and digits in groups joined by single underscores. The genres table has the
// same CHECK.
var genreSlug = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// Validate reports every invalid field of g as a *ValidationError, or returns nil.
func (g NewGenre) Validate() error {
	var v Violations
	switch {
	case g.Slug == "":
		v.Add("slug", "is required")
	case len(g.Slug) > MaxGenreSlugLength:
		v.Add("slug", "must be at most %d characters", MaxGenreSlugLength)
	case !genreSlug.MatchString(g.Slug):
		v.Add("slug", "must be lowercase letters and digits, in groups joined by single underscores, such as science_fiction")
	}
	switch n := utf8.RuneCountInString(g.Name); {
	case n == 0:
		v.Add("name", "is required")
	case n > MaxGenreNameLength:
		v.Add("name", "must be at most %d characters", MaxGenreNameLength)
	}
	return v.Err()
}

// MaxMovieGenres is how many genres a movie may have.
const MaxMovieGenres = 5

// CheckGenreIDs reports a list of a movie's genre ids that is too long under genre_ids, and ids that are not
// positive or repeated, each under its own field such as genre_ids[1]. Whether the genres exist is for the store
// to tell (see UnknownGenres).
func CheckGenreIDs(ids []int64) error {
	var v Violations
	checkGenreIDs(&v, ids)
	return v.Err()
}

func checkGenreIDs(v *Violations, ids []int64) {
	if len(ids) > MaxMovieGenres {
		v.Add("genre_ids", "must contain at most %d genres", MaxMovieGenres)
	}
	seen := make(map[int64]bool, len(ids))
	for i, id := range ids {
		field := fmt.Sprintf("genre_ids[%d]", i)
		switch {
		case id < 1:
			v.Add(field, "must be a genre id, a positive integer")
		case seen[id]:
			v.Add(field, "must be unique, but %d appears more than once", id)
		}
		seen[id] = true
	}
}

// UnknownGenres reports the ids in ids that known does not contain, each under its own field such as
// genre_ids[1], as a *ValidationError; it returns nil when every id is known.
func UnknownGenres(ids []int64, known map[int64]bool) error {
	var v Violations
	for i, id := range ids {
		if !known[id] {
			v.Add(fmt.Sprintf("genre_ids[%d]", i), "no genre has the id %d", id)
		}
	}
	return v.Err()
}
