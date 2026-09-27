package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fieldsOf returns the fields a *ValidationError names, or nil for any other error.
func fieldsOf(err error) []string {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	fields := make([]string, len(ve.Fields))
	for i, f := range ve.Fields {
		fields[i] = f.Field
	}
	return fields
}

func TestNewGenreValidate(t *testing.T) {
	t.Parallel()

	for _, g := range []NewGenre{
		{Slug: "science_fiction", Name: "Science fiction"},
		{Slug: "a", Name: "A"},
		{Slug: "film_noir_1940s", Name: "Film noir"},
		{Slug: strings.Repeat("x", MaxGenreSlugLength), Name: strings.Repeat("ж", MaxGenreNameLength)},
	} {
		if err := g.Validate(); err != nil {
			t.Errorf("%+v: %v", g, err)
		}
	}

	for _, tt := range []struct {
		genre  NewGenre
		fields []string
	}{
		{genre: NewGenre{}, fields: []string{"slug", "name"}},
		{genre: NewGenre{Slug: "Drama", Name: "Drama"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: "sci-fi", Name: "Sci-fi"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: "science__fiction", Name: "x"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: "_drama", Name: "x"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: "drama_", Name: "x"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: "драма", Name: "x"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: strings.Repeat("x", MaxGenreSlugLength+1), Name: "x"}, fields: []string{"slug"}},
		{genre: NewGenre{Slug: "drama", Name: strings.Repeat("ж", MaxGenreNameLength+1)}, fields: []string{"name"}},
	} {
		if got := fieldsOf(tt.genre.Validate()); !slices.Equal(got, tt.fields) {
			t.Errorf("%+v: fields %v, want %v", tt.genre, got, tt.fields)
		}
	}
}

func TestCheckGenreIDs(t *testing.T) {
	t.Parallel()

	for _, ids := range [][]int64{nil, {}, {1}, {5, 4, 3, 2, 1}} {
		if err := CheckGenreIDs(ids); err != nil {
			t.Errorf("%v: %v", ids, err)
		}
	}
	for _, tt := range []struct {
		ids    []int64
		fields []string
	}{
		{ids: []int64{1, 2, 3, 4, 5, 6}, fields: []string{"genre_ids"}},
		{ids: []int64{3, 0, -1}, fields: []string{"genre_ids[1]", "genre_ids[2]"}},
		{ids: []int64{3, 4, 3, 3}, fields: []string{"genre_ids[2]", "genre_ids[3]"}},
		{ids: []int64{1, 1, 1, 1, 1, 1}, fields: []string{"genre_ids", "genre_ids[1]", "genre_ids[2]", "genre_ids[3]", "genre_ids[4]", "genre_ids[5]"}},
	} {
		if got := fieldsOf(CheckGenreIDs(tt.ids)); !slices.Equal(got, tt.fields) {
			t.Errorf("%v: fields %v, want %v", tt.ids, got, tt.fields)
		}
	}
}

func TestUnknownGenres(t *testing.T) {
	t.Parallel()

	known := map[int64]bool{1: true, 3: true}
	if err := UnknownGenres([]int64{3, 1}, known); err != nil {
		t.Errorf("known ids: %v", err)
	}
	if err := UnknownGenres(nil, known); err != nil {
		t.Errorf("no ids: %v", err)
	}
	err := UnknownGenres([]int64{3, 2, 1, 9}, known)
	if got := fieldsOf(err); !slices.Equal(got, []string{"genre_ids[1]", "genre_ids[3]"}) {
		t.Errorf("fields = %v", got)
	}
	if !strings.Contains(err.Error(), "no genre has the id 9") {
		t.Errorf("message = %v", err)
	}
}
