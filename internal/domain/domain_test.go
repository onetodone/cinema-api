package domain

import (
	"errors"
	"fmt"
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
