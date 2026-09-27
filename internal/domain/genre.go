package domain

import (
	"fmt"
	"slices"
	"strings"
)

// Genre classifies a movie. Genres are stable lowercase slugs that clients translate and filter on.
type Genre string

// Genres. The PostgreSQL enum movie_genre has the same values.
const (
	GenreAction         Genre = "action"
	GenreAdventure      Genre = "adventure"
	GenreAnimation      Genre = "animation"
	GenreComedy         Genre = "comedy"
	GenreCrime          Genre = "crime"
	GenreDocumentary    Genre = "documentary"
	GenreDrama          Genre = "drama"
	GenreFamily         Genre = "family"
	GenreFantasy        Genre = "fantasy"
	GenreHistory        Genre = "history"
	GenreHorror         Genre = "horror"
	GenreMusic          Genre = "music"
	GenreMystery        Genre = "mystery"
	GenreRomance        Genre = "romance"
	GenreScienceFiction Genre = "science_fiction"
	GenreThriller       Genre = "thriller"
	GenreWar            Genre = "war"
	GenreWestern        Genre = "western"
)

var genres = []Genre{
	GenreAction, GenreAdventure, GenreAnimation, GenreComedy, GenreCrime, GenreDocumentary, GenreDrama,
	GenreFamily, GenreFantasy, GenreHistory, GenreHorror, GenreMusic, GenreMystery, GenreRomance,
	GenreScienceFiction, GenreThriller, GenreWar, GenreWestern,
}

// Genres returns every genre in alphabetical order.
func Genres() []Genre {
	return slices.Clone(genres)
}

// Valid reports whether g is a known genre.
func (g Genre) Valid() bool {
	return slices.Contains(genres, g)
}

// MaxMovieGenres is how many genres a movie may have.
const MaxMovieGenres = 5

// checkGenres reports unknown and repeated genres, each under its own field such as genres[1], and a list that is
// too long under genres.
func checkGenres(v *Violations, list []Genre) {
	if len(list) > MaxMovieGenres {
		v.Add("genres", "must contain at most %d genres", MaxMovieGenres)
	}
	seen := make(map[Genre]bool, len(list))
	for i, g := range list {
		field := fmt.Sprintf("genres[%d]", i)
		switch {
		case !g.Valid():
			v.Add(field, "must be one of %s", genreNames())
		case seen[g]:
			v.Add(field, "must be unique, but %s appears more than once", g)
		}
		seen[g] = true
	}
}

func genreNames() string {
	names := make([]string, len(genres))
	for i, g := range genres {
		names[i] = string(g)
	}
	return strings.Join(names, ", ")
}
