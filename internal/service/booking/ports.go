package booking

import (
	"context"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
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
// methods are the only way to lock seats, and both follow that order. Payments are never locked themselves:
// every change to a payment happens under the lock of its booking.
type TxRepos interface {
	Showtimes() ShowtimeRepo
	Seats() SeatRepo
	Bookings() Repo
	Payments() PaymentRepo
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
	// LockByBookings locks the seats the given bookings hold or bought, in (showtime_id, seat_id) order across
	// all of them.
	LockByBookings(ctx context.Context, bookingIDs ...uuid.UUID) ([]domain.ShowtimeSeat, error)
	// Hold marks available seats as held by a booking and returns how many changed. Seats that are not
	// available are left alone.
	Hold(ctx context.Context, showtimeID int64, seatIDs []int64, bookingID uuid.UUID) (int64, error)
	// Release makes the seats the given bookings hold available again and returns how many changed. Sold seats
	// are left alone.
	Release(ctx context.Context, bookingIDs ...uuid.UUID) (int64, error)
	// Sell marks the seats a booking holds as sold and returns how many changed.
	Sell(ctx context.Context, bookingID uuid.UUID) (int64, error)
}

// Repo reads and changes bookings inside a transaction.
type Repo interface {
	// Create inserts b as a pending booking together with its seats. The hold expires holdTTL after the
	// transaction started, by the database clock. The returned booking carries the stored status and
	// timestamps. A second active booking of the same user for the same showtime fails with
	// ACTIVE_BOOKING_EXISTS.
	Create(ctx context.Context, b domain.Booking, holdTTL time.Duration) (domain.Booking, error)
	// LockForUser locks the booking row with this id if it belongs to userID, and reports whether the booking's
	// hold has run out by the database clock. It fails with BOOKING_NOT_FOUND otherwise, and with BOOKING_BUSY
	// when the lock is not granted within the lock timeout. Only the booking's own columns are loaded: Showtime
	// has just its ID, and Seats is empty.
	LockForUser(ctx context.Context, id, userID uuid.UUID) (b domain.Booking, holdOver bool, err error)
	// Lock is LockForUser for a booking of any user.
	Lock(ctx context.Context, id uuid.UUID) (b domain.Booking, holdOver bool, err error)
	// LockExpired locks up to limit pending bookings whose hold has run out by the database clock, earliest
	// deadline first, and returns them with their own columns only, like Lock. Bookings that another transaction
	// has locked are skipped instead of waited for, so concurrent callers take disjoint sets and never block each
	// other.
	LockExpired(ctx context.Context, limit int) ([]domain.Booking, error)
	// SetStatus changes the status of the given bookings from `from` to `to`, and stamps the time of payment by
	// the database clock when `to` is paid. It fails unless every one of them was in status `from`.
	SetStatus(ctx context.Context, from, to domain.BookingStatus, ids ...uuid.UUID) error
}

// PaymentRepo reads and changes payments inside a transaction. The caller must hold the lock of the payment's
// booking, which every change to a payment takes, so the payment cannot change under it.
type PaymentRepo interface {
	// Create inserts p as a pending payment and returns it with the stored timestamps. A booking has at most one
	// pending payment; a second one fails with PAYMENT_IN_PROGRESS.
	Create(ctx context.Context, p domain.Payment) (domain.Payment, error)
	// Get returns the payment with this id.
	Get(ctx context.Context, id uuid.UUID) (domain.Payment, error)
	// Finish ends the payment with this id as out says and returns it as stored. It fails unless the payment
	// was in status `from`.
	Finish(ctx context.Context, id uuid.UUID, from domain.PaymentStatus, out domain.PaymentOutcome) (domain.Payment, error)
}

// Reader reads bookings without a transaction. It is implemented by repository/postgres.Bookings.
type Reader interface {
	// GetBooking returns a booking of userID with its seats, or fails with BOOKING_NOT_FOUND.
	GetBooking(ctx context.Context, id, userID uuid.UUID) (domain.Booking, error)
	// ListBookings returns up to limit bookings of userID with an id below beforeID, newest first.
	ListBookings(ctx context.Context, userID, beforeID uuid.UUID, limit int) ([]domain.Booking, error)
	// ListStuckPayments returns up to limit pending payments that started at least age ago by the database
	// clock and have an id above afterID, in id order.
	ListStuckPayments(ctx context.Context, age time.Duration, afterID uuid.UUID, limit int) ([]domain.Payment, error)
}

// PaymentProviders finds the payment providers that payments go through. It is implemented by *payment.Registry.
type PaymentProviders interface {
	// Enabled returns the provider with this id if it takes new payments.
	Enabled(id string) (payment.Provider, bool)
	// Provider returns the provider with this id whether or not it takes new payments, so a payment that is
	// already in flight settles through the provider it started with.
	Provider(id string) (payment.Provider, bool)
}

// HoldGate is a fast pre-check in front of the seat row locks, implemented by repository/redis.HoldGate. Its
// claims name the booking that made them, by the booking id as token. It never decides who gets a seat: a request
// that passes it still has to win the row locks. It only turns away requests for seats that another booking holds
// or claims before they take a database connection and queue on a lock, which keeps the connection pool free
// during a rush on a few seats.
//
// Every method may fail. The service then carries on without the gate (fail open), and the database decides alone,
// as it always does.
type HoldGate interface {
	// Acquire claims the seats for token for ttl, all or nothing, and returns the seats that other tokens hold.
	Acquire(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID, ttl time.Duration) (conflicts []int64, err error)
	// ExtendUntil keeps the seats that token holds claimed until the given time.
	ExtendUntil(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID, until time.Time) error
	// Release frees the seats that token holds.
	Release(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID) error
}

// SeatMapCache is told after every committed change to the seats of showtimes, so that cached seat maps stop
// showing the old state at once instead of when they expire. It is implemented by repository/redis.CatalogCache.
// A failure leaves the cached seat maps to expire on their own.
type SeatMapCache interface {
	InvalidateSeatMaps(ctx context.Context, showtimeIDs ...int64) error
}
