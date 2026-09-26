package booking

import (
	"context"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
)

// UnitOfWork runs use cases in database transactions. It is implemented by repository/postgres.UnitOfWork.
type UnitOfWork interface {
	// Do runs fn in one READ COMMITTED transaction with a lock timeout. It commits when fn returns nil and
	// rolls back otherwise. When the database aborts the transaction because of a deadlock or a serialization
	// failure, Do runs fn again from the start, so fn must not have effects outside the transaction.
	Do(ctx context.Context, fn func(ctx context.Context, r TxRepos) error) error
}

// TxRepos gives access to repositories bound to one transaction.
//
// Every use case takes row locks in the same global order, which is what keeps concurrent bookings free of
// deadlocks: first the bookings row, then showtime seats in (showtime_id, seat_id) order. SeatRepo's Lock
// methods are the only way to lock seats, and both follow that order.
type TxRepos interface {
	Showtimes() ShowtimeRepo
	Seats() SeatRepo
	Bookings() Repo
}

// ShowtimeRepo reads showtimes inside a transaction.
type ShowtimeRepo interface {
	// GetForBooking returns a showtime and whether it has started, judged by the database clock. An unknown id
	// fails with SHOWTIME_NOT_FOUND.
	GetForBooking(ctx context.Context, id int64) (st domain.ShowtimeRef, started bool, err error)
}

// SeatRepo reads and changes the seat inventory of showtimes.
type SeatRepo interface {
	// LockOrdered locks the given seats of a showtime in seat id order and returns them with their current
	// status, in that order. Seats that are not part of the showtime are missing from the result. A lock that
	// is not granted within the lock timeout fails with SEAT_BUSY.
	LockOrdered(ctx context.Context, showtimeID int64, seatIDs []int64) ([]domain.ShowtimeSeat, error)
	// LockByBooking locks the seats a booking holds or bought, in (showtime_id, seat_id) order.
	LockByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.ShowtimeSeat, error)
	// Hold marks available seats as held by a booking and returns how many changed. Seats that are not
	// available are left alone.
	Hold(ctx context.Context, showtimeID int64, seatIDs []int64, bookingID uuid.UUID) (int64, error)
	// Release makes the seats a booking holds available again and returns how many changed. Sold seats are
	// left alone.
	Release(ctx context.Context, bookingID uuid.UUID) (int64, error)
}

// Repo reads and changes bookings inside a transaction.
type Repo interface {
	// Create inserts b as a pending booking together with its seats. The hold expires holdTTL after the
	// transaction started, by the database clock. The returned booking carries the stored status and
	// timestamps. A second active booking of the same user for the same showtime fails with
	// ACTIVE_BOOKING_EXISTS.
	Create(ctx context.Context, b domain.Booking, holdTTL time.Duration) (domain.Booking, error)
	// LockForUser locks the booking row with this id if it belongs to userID. It fails with BOOKING_NOT_FOUND
	// otherwise, and with BOOKING_BUSY when the lock is not granted within the lock timeout. Only the
	// booking's own columns are loaded: Showtime has just its ID, and Seats is empty.
	LockForUser(ctx context.Context, id, userID uuid.UUID) (domain.Booking, error)
	// SetStatus changes a booking's status from `from` to `to`. It fails if the booking is not in status `from`.
	SetStatus(ctx context.Context, id uuid.UUID, from, to domain.BookingStatus) error
}

// Reader reads bookings without a transaction. It is implemented by repository/postgres.Bookings.
type Reader interface {
	// GetBooking returns a booking of userID with its seats, or fails with BOOKING_NOT_FOUND.
	GetBooking(ctx context.Context, id, userID uuid.UUID) (domain.Booking, error)
	// ListBookings returns up to limit bookings of userID with an id below beforeID, newest first.
	ListBookings(ctx context.Context, userID, beforeID uuid.UUID, limit int) ([]domain.Booking, error)
}
