package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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
		{err: Busy(CodeSeatBusy, "locked"), kind: ErrBusy},
		{err: SeatsUnavailable([]int64{3}), kind: ErrConflict},
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

// invalidFields returns the fields a validation error names, or fails the test if err is not one.
func invalidFields(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate() = %v, want a validation error", err)
	}
	got := make([]string, 0, len(ve.Fields))
	for _, f := range ve.Fields {
		got = append(got, f.Field)
	}
	return got
}

func TestNewHallValidate(t *testing.T) {
	t.Parallel()

	valid := NewHall{Name: "Hall 4", Rows: []HallRow{
		{Label: "A", Seats: 10, Type: SeatAccessible},
		{Label: "B", Seats: MaxRowSeats, Type: SeatStandard},
		{Label: "AA", Seats: 1, Type: SeatVIP},
		{Label: "10", Seats: 1, Type: SeatStandard},
	}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid hall: %v", err)
	}

	rowsOf := func(n, seats int) []HallRow {
		rows := make([]HallRow, n)
		for i := range rows {
			rows[i] = HallRow{Label: fmt.Sprint(i + 1), Seats: seats, Type: SeatStandard}
		}
		return rows
	}
	tests := []struct {
		name   string
		edit   func(*NewHall)
		fields []string
	}{
		{name: "missing name", edit: func(h *NewHall) { h.Name = "" }, fields: []string{"name"}},
		{name: "long name", edit: func(h *NewHall) { h.Name = strings.Repeat("é", MaxHallNameLength+1) }, fields: []string{"name"}},
		{name: "no rows", edit: func(h *NewHall) { h.Rows = nil }, fields: []string{"rows"}},
		{name: "too many rows", edit: func(h *NewHall) { h.Rows = rowsOf(MaxHallRows+1, 1) }, fields: []string{"rows"}},
		{name: "too many seats", edit: func(h *NewHall) { h.Rows = rowsOf(11, 100) }, fields: []string{"rows"}},
		{name: "lowercase label", edit: func(h *NewHall) { h.Rows[1].Label = "b" }, fields: []string{"rows[1].label"}},
		{name: "empty label", edit: func(h *NewHall) { h.Rows[0].Label = "" }, fields: []string{"rows[0].label"}},
		{name: "long label", edit: func(h *NewHall) { h.Rows[0].Label = "ABCD" }, fields: []string{"rows[0].label"}},
		{name: "duplicate label", edit: func(h *NewHall) { h.Rows[2].Label = "A" }, fields: []string{"rows[2].label"}},
		{name: "empty row", edit: func(h *NewHall) { h.Rows[0].Seats = 0 }, fields: []string{"rows[0].seats"}},
		{name: "long row", edit: func(h *NewHall) { h.Rows[0].Seats = MaxRowSeats + 1 }, fields: []string{"rows[0].seats"}},
		{name: "unknown type", edit: func(h *NewHall) { h.Rows[3].Type = "balcony" }, fields: []string{"rows[3].type"}},
		{name: "missing type", edit: func(h *NewHall) { h.Rows[3].Type = "" }, fields: []string{"rows[3].type"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := NewHall{Name: valid.Name, Rows: slices.Clone(valid.Rows)}
			tt.edit(&h)
			if got := invalidFields(t, h.Validate()); !slices.Equal(got, tt.fields) {
				t.Errorf("invalid fields = %v, want %v", got, tt.fields)
			}
		})
	}
}

func TestNewShowtimeValidate(t *testing.T) {
	t.Parallel()

	now := time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)
	valid := NewShowtime{MovieID: 1, HallID: 2, StartsAt: now.Add(time.Minute), BasePriceCents: 900}
	if err := valid.Validate(now); err != nil {
		t.Fatalf("valid showtime: %v", err)
	}
	free := valid
	free.BasePriceCents, free.StartsAt = 0, now.Add(MaxScheduleAhead)
	if err := free.Validate(now); err != nil {
		t.Errorf("a free showtime a year ahead: %v", err)
	}

	tests := []struct {
		name   string
		edit   func(*NewShowtime)
		fields []string
	}{
		{name: "no movie", edit: func(s *NewShowtime) { s.MovieID = 0 }, fields: []string{"movie_id"}},
		{name: "no hall", edit: func(s *NewShowtime) { s.HallID = -1 }, fields: []string{"hall_id"}},
		{name: "no start", edit: func(s *NewShowtime) { s.StartsAt = time.Time{} }, fields: []string{"starts_at"}},
		{name: "starts now", edit: func(s *NewShowtime) { s.StartsAt = now }, fields: []string{"starts_at"}},
		{name: "too far ahead", edit: func(s *NewShowtime) { s.StartsAt = now.Add(MaxScheduleAhead + time.Second) }, fields: []string{"starts_at"}},
		{name: "negative price", edit: func(s *NewShowtime) { s.BasePriceCents = -1 }, fields: []string{"base_price_cents"}},
		{name: "huge price", edit: func(s *NewShowtime) { s.BasePriceCents = MaxBasePriceCents + 1 }, fields: []string{"base_price_cents"}},
		{name: "everything wrong", edit: func(s *NewShowtime) { *s = NewShowtime{BasePriceCents: -5} },
			fields: []string{"movie_id", "hall_id", "starts_at", "base_price_cents"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := valid
			tt.edit(&s)
			if got := invalidFields(t, s.Validate(now)); !slices.Equal(got, tt.fields) {
				t.Errorf("invalid fields = %v, want %v", got, tt.fields)
			}
		})
	}
}
