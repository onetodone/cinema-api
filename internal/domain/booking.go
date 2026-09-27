package domain

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"
)

// BookingStatus is the lifecycle state of a booking.
type BookingStatus string

// Booking statuses. A pending or processing booking holds its seats; the other statuses are final.
const (
	BookingPending    BookingStatus = "pending"    // seats held, waiting for payment
	BookingProcessing BookingStatus = "processing" // a payment is in flight; the expiry worker leaves it alone
	BookingPaid       BookingStatus = "paid"
	BookingExpired    BookingStatus = "expired"
	BookingCanceled   BookingStatus = "canceled"
)

// bookingTransitions lists the allowed status changes. The SQL that performs a change repeats the current
// status in its WHERE clause, so a change that raced with another one updates nothing.
var bookingTransitions = map[BookingStatus][]BookingStatus{
	BookingPending:    {BookingProcessing, BookingCanceled, BookingExpired},
	BookingProcessing: {BookingPaid, BookingPending, BookingExpired},
}

// Valid reports whether s is a known booking status.
func (s BookingStatus) Valid() bool {
	switch s {
	case BookingPending, BookingProcessing, BookingPaid, BookingExpired, BookingCanceled:
		return true
	}
	return false
}

// CanBecome reports whether a booking in status s may change to next.
func (s BookingStatus) CanBecome(next BookingStatus) bool {
	return slices.Contains(bookingTransitions[s], next)
}

// Active reports whether a booking in status s still holds its seats.
func (s BookingStatus) Active() bool {
	return s == BookingPending || s == BookingProcessing
}

// ShowtimeRef is the part of a showtime shown with a booking.
type ShowtimeRef struct {
	ID       int64
	Movie    MovieSummary
	Hall     Hall
	StartsAt time.Time
	Language LanguageVersion
	Status   ShowtimeStatus
}

// BookedSeat is one seat of a booking, at the price it was booked for.
type BookedSeat struct {
	SeatID     int64
	Row        string
	Number     int
	Type       SeatType
	PriceCents int64
}

// Booking is a customer's hold on, or purchase of, seats of one showtime.
type Booking struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Showtime   ShowtimeRef
	Status     BookingStatus
	Seats      []BookedSeat // ordered as on the seat map
	TotalCents int64
	ExpiresAt  time.Time // hold deadline, computed with the database clock
	PaidAt     time.Time // zero until paid
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewBooking is a request to hold seats of one showtime.
type NewBooking struct {
	ShowtimeID int64
	SeatIDs    []int64
}

// Validate reports every invalid field of b as a *ValidationError, or returns nil. A booking holds between
// 1 and maxSeats distinct seats.
func (b NewBooking) Validate(maxSeats int) error {
	var v Violations
	if b.ShowtimeID <= 0 {
		v.Add("showtime_id", "must be a positive integer")
	}
	switch n := len(b.SeatIDs); {
	case n == 0:
		v.Add("seat_ids", "must contain at least 1 seat")
	case n > maxSeats:
		v.Add("seat_ids", "must contain at most %d seats", maxSeats)
	}
	seen := make(map[int64]bool, len(b.SeatIDs))
	for _, id := range b.SeatIDs {
		if id <= 0 {
			v.Add("seat_ids", "must contain positive integers only")
			break
		}
		if seen[id] {
			v.Add("seat_ids", "must not contain seat %d twice", id)
			break
		}
		seen[id] = true
	}
	return v.Err()
}

// CompareSeatPositions orders seats the way a seat map lists them: by row, then by number. Rows sort by label
// length first, so "AA" follows "Z".
func CompareSeatPositions(rowA string, numberA int, rowB string, numberB int) int {
	return cmp.Or(cmp.Compare(len(rowA), len(rowB)), cmp.Compare(rowA, rowB), cmp.Compare(numberA, numberB))
}

// SeatsUnavailableError reports requested seats that another booking already holds or bought. It is a conflict
// with code CodeSeatUnavailable: errors.Is(err, ErrConflict) holds, and errors.As finds the *Error.
type SeatsUnavailableError struct {
	SeatIDs []int64
	err     *Error
}

// SeatsUnavailable returns a *SeatsUnavailableError for the given seats.
func SeatsUnavailable(seatIDs []int64) error {
	msg := "seat " + joinIDs(seatIDs) + " is already held or sold"
	if len(seatIDs) > 1 {
		msg = "seats " + joinIDs(seatIDs) + " are already held or sold"
	}
	return &SeatsUnavailableError{
		SeatIDs: seatIDs,
		err:     &Error{Kind: ErrConflict, Code: CodeSeatUnavailable, Message: msg},
	}
}

func (e *SeatsUnavailableError) Error() string { return e.err.Message }

// Unwrap exposes the underlying *Error, so the code and kind are visible to errors.As and errors.Is.
func (e *SeatsUnavailableError) Unwrap() error { return e.err }

// ActiveBookingExistsError reports that the user already has an unpaid booking for the showtime, which blocks a
// second one. It is a conflict with code CodeActiveBookingExists: errors.Is(err, ErrConflict) holds, and
// errors.As finds the *Error.
type ActiveBookingExistsError struct {
	// BookingID names the blocking booking; the zero UUID when it is not known, or ended in the meantime.
	BookingID uuid.UUID
	err       *Error
}

// ActiveBookingExists returns an *ActiveBookingExistsError for the showtime, naming the blocking booking if
// bookingID is not the zero UUID.
func ActiveBookingExists(showtimeID int64, bookingID uuid.UUID) error {
	return &ActiveBookingExistsError{
		BookingID: bookingID,
		err: &Error{Kind: ErrConflict, Code: CodeActiveBookingExists, Message: fmt.Sprintf(
			"you already have an unpaid booking for showtime %d; pay for it or cancel it first", showtimeID)},
	}
}

func (e *ActiveBookingExistsError) Error() string { return e.err.Message }

// Unwrap exposes the underlying *Error, so the code and kind are visible to errors.As and errors.Is.
func (e *ActiveBookingExistsError) Unwrap() error { return e.err }

// UnknownSeats returns the UNKNOWN_SEAT error for seats that are not part of a showtime.
func UnknownSeats(showtimeID int64, seatIDs []int64) error {
	noun := "seat " + joinIDs(seatIDs) + " is"
	if len(seatIDs) > 1 {
		noun = "seats " + joinIDs(seatIDs) + " are"
	}
	return Invalid(CodeUnknownSeat, "%s not part of showtime %d", noun, showtimeID)
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

// BookingNotFound returns the error for a booking that does not exist or belongs to another user. The two cases
// look the same, so nobody can probe for the booking ids of other users.
func BookingNotFound(id uuid.UUID) error {
	return NotFound(CodeBookingNotFound, "booking %s not found", id)
}

// CheckBookable reports why a showtime cannot be booked, or returns nil. started tells whether the showtime has
// begun, judged by the database clock.
func CheckBookable(st ShowtimeRef, started bool) error {
	switch {
	case st.Status != ShowtimeScheduled:
		return Conflict(CodeShowtimeNotBookable, "showtime %d is %s", st.ID, st.Status)
	case started:
		return Conflict(CodeShowtimeNotBookable, "showtime %d has already started", st.ID)
	}
	return nil
}
