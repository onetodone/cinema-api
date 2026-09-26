package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorKindsSurviveWrapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		kind error
	}{
		{err: NotFound(CodeMovieNotFound, "movie %d not found", 7), kind: ErrNotFound},
		{err: Conflict(CodeHallOverlap, "overlap"), kind: ErrConflict},
		{err: Invalid("SOME_CODE", "bad"), kind: ErrInvalid},
		{err: Unauthenticated(CodeInvalidToken, "bad token"), kind: ErrUnauthenticated},
		{err: Forbidden(CodeForbidden, "no"), kind: ErrForbidden},
	}

	for _, tt := range tests {
		wrapped := fmt.Errorf("service: %w", tt.err)
		if !errors.Is(wrapped, tt.kind) {
			t.Errorf("errors.Is(%v, %v) = false", wrapped, tt.kind)
		}
		var de *Error
		if !errors.As(wrapped, &de) {
			t.Fatalf("errors.As(%v, *Error) = false", wrapped)
		}
		if de.Error() != tt.err.Error() {
			t.Errorf("message = %q, want %q", de.Error(), tt.err.Error())
		}
	}
}

func TestNotFoundFormatsMessage(t *testing.T) {
	t.Parallel()

	err := NotFound(CodeShowtimeNotFound, "showtime %d not found", 42)
	var de *Error
	if !errors.As(err, &de) {
		t.Fatal("not a domain error")
	}
	if de.Code != CodeShowtimeNotFound || de.Message != "showtime 42 not found" {
		t.Errorf("got %+v", de)
	}
}

func TestSeatPrice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		seat SeatType
		base int64
		want int64
	}{
		{seat: SeatStandard, base: 1000, want: 1000},
		{seat: SeatAccessible, base: 1000, want: 1000},
		{seat: SeatVIP, base: 1000, want: 1500},
		{seat: SeatVIP, base: 999, want: 1498}, // integer cents, rounded down like the SQL rule
	}
	for _, tt := range tests {
		if got := SeatPrice(tt.base, tt.seat); got != tt.want {
			t.Errorf("SeatPrice(%d, %s) = %d, want %d", tt.base, tt.seat, got, tt.want)
		}
	}
}

func TestSeatTypeValid(t *testing.T) {
	t.Parallel()

	for _, st := range []SeatType{SeatStandard, SeatVIP, SeatAccessible} {
		if !st.Valid() {
			t.Errorf("%q should be valid", st)
		}
	}
	if SeatType("balcony").Valid() {
		t.Error(`"balcony" should be invalid`)
	}
}

func TestViolations(t *testing.T) {
	t.Parallel()

	var empty Violations
	if err := empty.Err(); err != nil {
		t.Errorf("Err() without violations = %v, want nil", err)
	}

	var v Violations
	v.Add("email", "is required")
	v.Check("password", nil)
	v.Check("password", errors.New("must be at least 8 characters"))
	v.Add("limit", "must be between %d and %d", 1, 100)

	err := fmt.Errorf("register: %w", v.Err())
	if !errors.Is(err, ErrInvalid) {
		t.Error("a validation error should be of kind ErrInvalid")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As(%v, *ValidationError) = false", err)
	}
	want := []FieldError{
		{Field: "email", Message: "is required"},
		{Field: "password", Message: "must be at least 8 characters"},
		{Field: "limit", Message: "must be between 1 and 100"},
	}
	if fmt.Sprint(ve.Fields) != fmt.Sprint(want) {
		t.Errorf("fields = %v, want %v", ve.Fields, want)
	}
	if got := ve.Error(); got != "invalid input: email is required; password must be at least 8 characters; "+
		"limit must be between 1 and 100" {
		t.Errorf("message = %q", got)
	}
}

func TestNewMovieValidate(t *testing.T) {
	t.Parallel()

	valid := NewMovie{Title: "Dune", DurationMin: 155, AgeRating: "PG-13", PosterURL: "https://img.example/dune.jpg"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid movie: %v", err)
	}
	if err := (NewMovie{Title: "Short", DurationMin: 1}).Validate(); err != nil {
		t.Errorf("a movie with only the required fields: %v", err)
	}

	tests := []struct {
		name   string
		edit   func(*NewMovie)
		fields []string
	}{
		{name: "missing title", edit: func(m *NewMovie) { m.Title = "" }, fields: []string{"title"}},
		{name: "long title", edit: func(m *NewMovie) { m.Title = strings.Repeat("é", MaxTitleLength+1) }, fields: []string{"title"}},
		{name: "long description", edit: func(m *NewMovie) {
			m.Description = strings.Repeat("x", MaxDescriptionLength+1)
		}, fields: []string{"description"}},
		{name: "zero duration", edit: func(m *NewMovie) { m.DurationMin = 0 }, fields: []string{"duration_min"}},
		{name: "long duration", edit: func(m *NewMovie) { m.DurationMin = MaxDurationMin + 1 }, fields: []string{"duration_min"}},
		{name: "long age rating", edit: func(m *NewMovie) { m.AgeRating = strings.Repeat("R", 17) }, fields: []string{"age_rating"}},
		{name: "relative poster", edit: func(m *NewMovie) { m.PosterURL = "/dune.jpg" }, fields: []string{"poster_url"}},
		{name: "script poster", edit: func(m *NewMovie) { m.PosterURL = "javascript:alert(1)" }, fields: []string{"poster_url"}},
		{name: "long poster", edit: func(m *NewMovie) {
			m.PosterURL = "https://img.example/" + strings.Repeat("a", MaxPosterURLLength)
		}, fields: []string{"poster_url"}},
		{name: "everything wrong", edit: func(m *NewMovie) {
			*m = NewMovie{PosterURL: "ftp://x/y"}
		}, fields: []string{"title", "duration_min", "poster_url"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := valid
			tt.edit(&m)
			var ve *ValidationError
			if !errors.As(m.Validate(), &ve) {
				t.Fatalf("Validate() = %v, want a validation error", m.Validate())
			}
			got := make([]string, 0, len(ve.Fields))
			for _, f := range ve.Fields {
				got = append(got, f.Field)
			}
			if strings.Join(got, ",") != strings.Join(tt.fields, ",") {
				t.Errorf("invalid fields = %v, want %v", got, tt.fields)
			}
		})
	}
}
