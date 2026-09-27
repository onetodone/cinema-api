package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"uuid"
)

func TestNewBookingValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     NewBooking
		fields []string // invalid fields, in order; none means valid
	}{
		{name: "one seat", in: NewBooking{ShowtimeID: 1, SeatIDs: []int64{5}}},
		{name: "max seats", in: NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2, 3, 4}}},
		{name: "no showtime", in: NewBooking{SeatIDs: []int64{5}}, fields: []string{"showtime_id"}},
		{name: "negative showtime", in: NewBooking{ShowtimeID: -1, SeatIDs: []int64{5}}, fields: []string{"showtime_id"}},
		{name: "no seats", in: NewBooking{ShowtimeID: 1}, fields: []string{"seat_ids"}},
		{name: "too many seats", in: NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2, 3, 4, 5}}, fields: []string{"seat_ids"}},
		{name: "zero seat id", in: NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 0}}, fields: []string{"seat_ids"}},
		{name: "duplicate seat", in: NewBooking{ShowtimeID: 1, SeatIDs: []int64{7, 8, 7}}, fields: []string{"seat_ids"}},
		{
			name:   "everything wrong at once",
			in:     NewBooking{SeatIDs: []int64{1, 2, 3, 4, 4}},
			fields: []string{"showtime_id", "seat_ids", "seat_ids"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.in.Validate(4)
			if len(tt.fields) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() = %v, want a *ValidationError", err)
			}
			var got []string
			for _, f := range ve.Fields {
				got = append(got, f.Field)
			}
			if !slices.Equal(got, tt.fields) {
				t.Errorf("fields = %v, want %v (%v)", got, tt.fields, err)
			}
		})
	}
}

func TestNewBookingValidateNamesTheDuplicate(t *testing.T) {
	t.Parallel()

	err := NewBooking{ShowtimeID: 1, SeatIDs: []int64{7, 8, 7}}.Validate(10)
	if err == nil || !strings.Contains(err.Error(), "seat_ids must not contain seat 7 twice") {
		t.Errorf("Validate() = %v", err)
	}
}

func TestBookingTransitions(t *testing.T) {
	t.Parallel()

	allowed := map[[2]BookingStatus]bool{
		{BookingPending, BookingProcessing}:  true,
		{BookingPending, BookingCanceled}:    true,
		{BookingPending, BookingExpired}:     true,
		{BookingProcessing, BookingPaid}:     true,
		{BookingProcessing, BookingPending}:  true,
		{BookingProcessing, BookingExpired}:  true,
		{BookingProcessing, BookingCanceled}: false, // a payment in flight cannot be canceled
		{BookingPaid, BookingCanceled}:       false,
		{BookingExpired, BookingPending}:     false,
		{BookingCanceled, BookingPending}:    false,
		{BookingPending, BookingPaid}:        false, // paying always passes through processing
		{BookingPending, BookingPending}:     false,
	}
	for pair, want := range allowed {
		if got := pair[0].CanBecome(pair[1]); got != want {
			t.Errorf("%s -> %s allowed = %v, want %v", pair[0], pair[1], got, want)
		}
	}

	for s, active := range map[BookingStatus]bool{
		BookingPending: true, BookingProcessing: true, BookingPaid: false, BookingExpired: false, BookingCanceled: false,
	} {
		if s.Active() != active {
			t.Errorf("%s.Active() = %v, want %v", s, s.Active(), active)
		}
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	for _, s := range []BookingStatus{"", "held", "Pending", "pending "} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
}

func TestCompareSeatPositions(t *testing.T) {
	t.Parallel()

	type seat struct {
		row    string
		number int
	}
	seats := []seat{{"AA", 1}, {"B", 2}, {"A", 10}, {"B", 1}, {"A", 2}, {"Z", 1}}
	slices.SortFunc(seats, func(a, b seat) int { return CompareSeatPositions(a.row, a.number, b.row, b.number) })

	want := []seat{{"A", 2}, {"A", 10}, {"B", 1}, {"B", 2}, {"Z", 1}, {"AA", 1}}
	if !slices.Equal(seats, want) {
		t.Errorf("sorted = %v, want %v", seats, want)
	}
}

func TestSeatsUnavailableError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("create booking: %w", SeatsUnavailable([]int64{4, 9}))
	if !errors.Is(err, ErrConflict) {
		t.Error("not a conflict")
	}
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeSeatUnavailable {
		t.Errorf("errors.As(*Error) = %+v, want code %s", de, CodeSeatUnavailable)
	}
	var se *SeatsUnavailableError
	if !errors.As(err, &se) || !slices.Equal(se.SeatIDs, []int64{4, 9}) {
		t.Errorf("errors.As(*SeatsUnavailableError) = %+v", se)
	}
	if got := SeatsUnavailable([]int64{4, 9}).Error(); got != "seats 4, 9 are already held or sold" {
		t.Errorf("message = %q", got)
	}
	if got := SeatsUnavailable([]int64{4}).Error(); got != "seat 4 is already held or sold" {
		t.Errorf("message = %q", got)
	}
}

func TestActiveBookingExistsError(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("01920000-0000-7000-8000-0000000000b1")
	err := fmt.Errorf("create booking: %w", ActiveBookingExists(11, id))
	if !errors.Is(err, ErrConflict) {
		t.Error("not a conflict")
	}
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeActiveBookingExists {
		t.Errorf("errors.As(*Error) = %+v, want code %s", de, CodeActiveBookingExists)
	}
	var ae *ActiveBookingExistsError
	if !errors.As(err, &ae) || ae.BookingID != id {
		t.Errorf("errors.As(*ActiveBookingExistsError) = %+v", ae)
	}
	want := "you already have an unpaid booking for showtime 11; pay for it or cancel it first"
	if got := ActiveBookingExists(11, uuid.UUID{}).Error(); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestUnknownSeats(t *testing.T) {
	t.Parallel()

	err := UnknownSeats(3, []int64{8})
	if !errors.Is(err, ErrInvalid) || err.Error() != "seat 8 is not part of showtime 3" {
		t.Errorf("one seat: %v", err)
	}
	if err := UnknownSeats(3, []int64{8, 9}); err.Error() != "seats 8, 9 are not part of showtime 3" {
		t.Errorf("two seats: %v", err)
	}
}

func TestCheckBookable(t *testing.T) {
	t.Parallel()

	scheduled := ShowtimeRef{ID: 5, Status: ShowtimeScheduled}
	if err := CheckBookable(scheduled, false); err != nil {
		t.Errorf("future scheduled showtime: %v", err)
	}

	for name, tt := range map[string]struct {
		st      ShowtimeRef
		started bool
		msg     string
	}{
		"started":  {st: scheduled, started: true, msg: "showtime 5 has already started"},
		"canceled": {st: ShowtimeRef{ID: 5, Status: ShowtimeCanceled}, msg: "showtime 5 is canceled"},
	} {
		err := CheckBookable(tt.st, tt.started)
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeShowtimeNotBookable || !errors.Is(err, ErrConflict) || de.Message != tt.msg {
			t.Errorf("%s: %v", name, err)
		}
	}
}
