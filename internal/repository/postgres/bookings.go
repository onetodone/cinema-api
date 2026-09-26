package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Constraints whose violations map to domain errors.
const (
	activeBookingIndex = "bookings_one_active_uq" // one pending or processing booking per user and showtime
	bookingUserFK      = "bookings_user_id_fkey"
)

// Bookings reads bookings outside transactions. It implements booking.Reader.
type Bookings struct {
	pool *pgxpool.Pool
}

// NewBookings returns a Bookings repository backed by pool.
func NewBookings(pool *pgxpool.Pool) *Bookings {
	return &Bookings{pool: pool}
}

// bookingSelect joins a booking with the showtime, movie, and hall shown next to it.
const bookingSelect = `
SELECT b.id, b.user_id, b.status, b.total_cents, b.expires_at, b.paid_at, b.created_at, b.updated_at,
       s.id, s.starts_at, s.status,
       m.id, m.title, m.duration_min, m.age_rating,
       h.id, h.name
FROM bookings b
JOIN showtimes s ON s.id = b.showtime_id
JOIN movies    m ON m.id = s.movie_id
JOIN halls     h ON h.id = s.hall_id`

func scanBooking(row pgx.Row) (domain.Booking, error) {
	var (
		b         domain.Booking
		paidAt    *time.Time
		ageRating *string
	)
	err := row.Scan(
		&b.ID, &b.UserID, &b.Status, &b.TotalCents, &b.ExpiresAt, &paidAt, &b.CreatedAt, &b.UpdatedAt,
		&b.Showtime.ID, &b.Showtime.StartsAt, &b.Showtime.Status,
		&b.Showtime.Movie.ID, &b.Showtime.Movie.Title, &b.Showtime.Movie.DurationMin, &ageRating,
		&b.Showtime.Hall.ID, &b.Showtime.Hall.Name,
	)
	if paidAt != nil {
		b.PaidAt = *paidAt
	}
	b.Showtime.Movie.AgeRating = deref(ageRating)
	return b, err
}

// GetBooking returns a booking of userID with its seats. A booking of another user is reported as not found.
func (r *Bookings) GetBooking(ctx context.Context, id, userID uuid.UUID) (domain.Booking, error) {
	b, err := scanBooking(r.pool.QueryRow(ctx, bookingSelect+` WHERE b.id = $1 AND b.user_id = $2`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Booking{}, domain.BookingNotFound(id)
	}
	if err != nil {
		return domain.Booking{}, fmt.Errorf("get booking %s: %w", id, err)
	}

	seats, err := bookedSeats(ctx, r.pool, []uuid.UUID{id})
	if err != nil {
		return domain.Booking{}, err
	}
	b.Seats = seats[id]
	return b, nil
}

// ListBookings returns up to limit bookings of userID with an id below beforeID, newest first. UUIDv7 ids
// grow over time, so the id is a keyset pagination key.
func (r *Bookings) ListBookings(ctx context.Context, userID, beforeID uuid.UUID, limit int) ([]domain.Booking, error) {
	rows, err := r.pool.Query(ctx, bookingSelect+`
WHERE b.user_id = $1 AND b.id < $2
ORDER BY b.id DESC
LIMIT $3`, userID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list bookings of user %s: %w", userID, err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Booking, error) { return scanBooking(row) })
	if err != nil {
		return nil, fmt.Errorf("list bookings of user %s: %w", userID, err)
	}
	if len(list) == 0 {
		return list, nil
	}

	ids := make([]uuid.UUID, len(list))
	for i, b := range list {
		ids[i] = b.ID
	}
	seats, err := bookedSeats(ctx, r.pool, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Seats = seats[list[i].ID]
	}
	return list, nil
}

// bookedSeats returns the seats of the given bookings, in seat map order. The rows of booking_seats never
// change after the booking is created, so reading them outside the booking's query is consistent.
func bookedSeats(ctx context.Context, q querier, bookingIDs []uuid.UUID) (map[uuid.UUID][]domain.BookedSeat, error) {
	rows, err := q.Query(ctx, `
SELECT bs.booking_id, bs.seat_id, hs.row_label, hs.seat_number, hs.seat_type, bs.price_cents
FROM booking_seats bs
JOIN hall_seats hs ON hs.id = bs.seat_id
WHERE bs.booking_id = ANY($1::uuid[])
ORDER BY bs.booking_id, length(hs.row_label), hs.row_label, hs.seat_number`, bookingIDs)
	if err != nil {
		return nil, fmt.Errorf("list booked seats: %w", err)
	}

	seats := make(map[uuid.UUID][]domain.BookedSeat, len(bookingIDs))
	var (
		bookingID uuid.UUID
		seat      domain.BookedSeat
	)
	_, err = pgx.ForEachRow(rows,
		[]any{&bookingID, &seat.SeatID, &seat.Row, &seat.Number, &seat.Type, &seat.PriceCents},
		func() error {
			seats[bookingID] = append(seats[bookingID], seat)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list booked seats: %w", err)
	}
	return seats, nil
}

// showtimeStore reads showtimes inside a transaction. It implements booking.ShowtimeRepo.
type showtimeStore struct {
	q querier
}

// GetForBooking reads the showtime without locking it: showtimes are not changed while they are on sale.
func (s showtimeStore) GetForBooking(ctx context.Context, id int64) (domain.ShowtimeRef, bool, error) {
	var (
		st        domain.ShowtimeRef
		ageRating *string
		started   bool
	)
	err := s.q.QueryRow(ctx, `
SELECT s.id, s.starts_at, s.status, s.starts_at <= now(),
       m.id, m.title, m.duration_min, m.age_rating,
       h.id, h.name
FROM showtimes s
JOIN movies m ON m.id = s.movie_id
JOIN halls  h ON h.id = s.hall_id
WHERE s.id = $1`, id).Scan(
		&st.ID, &st.StartsAt, &st.Status, &started,
		&st.Movie.ID, &st.Movie.Title, &st.Movie.DurationMin, &ageRating,
		&st.Hall.ID, &st.Hall.Name,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ShowtimeRef{}, false, domain.NotFound(domain.CodeShowtimeNotFound, "showtime %d not found", id)
	}
	if err != nil {
		return domain.ShowtimeRef{}, false, fmt.Errorf("get showtime %d for booking: %w", id, err)
	}
	st.Movie.AgeRating = deref(ageRating)
	return st, started, nil
}

// bookingStore changes bookings inside a transaction. It implements booking.Repo.
type bookingStore struct {
	q querier
}

func (s bookingStore) Create(ctx context.Context, b domain.Booking, holdTTL time.Duration) (domain.Booking, error) {
	err := s.q.QueryRow(ctx, `
INSERT INTO bookings (id, user_id, showtime_id, status, total_cents, expires_at)
VALUES ($1, $2, $3, 'pending', $4, now() + $5::interval)
RETURNING status, expires_at, created_at, updated_at`,
		b.ID, b.UserID, b.Showtime.ID, b.TotalCents, holdTTL,
	).Scan(&b.Status, &b.ExpiresAt, &b.CreatedAt, &b.UpdatedAt)
	switch {
	case isUniqueViolation(err, activeBookingIndex):
		return domain.Booking{}, domain.Conflict(domain.CodeActiveBookingExists,
			"you already have an unpaid booking for showtime %d; pay for it or cancel it first", b.Showtime.ID)
	case pgErrorCode(err) == sqlstateLockNotAvailable:
		// The unique index makes this insert wait for another uncommitted booking of the same user and showtime.
		return domain.Booking{}, domain.Busy(domain.CodeBookingBusy,
			"another booking request of yours for showtime %d is in progress; try again in a moment", b.Showtime.ID)
	case isViolation(err, sqlstateForeignKeyViolation, bookingUserFK):
		return domain.Booking{}, domain.Unauthenticated(domain.CodeInvalidToken,
			"the account of this access token no longer exists")
	case err != nil:
		return domain.Booking{}, fmt.Errorf("insert booking %s: %w", b.ID, err)
	}

	seatIDs := make([]int64, len(b.Seats))
	prices := make([]int64, len(b.Seats))
	for i, seat := range b.Seats {
		seatIDs[i], prices[i] = seat.SeatID, seat.PriceCents
	}
	_, err = s.q.Exec(ctx, `
INSERT INTO booking_seats (booking_id, showtime_id, seat_id, price_cents)
SELECT $1, $2, seat.id, seat.price_cents
FROM unnest($3::bigint[], $4::bigint[]) AS seat (id, price_cents)`,
		b.ID, b.Showtime.ID, seatIDs, prices)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("insert seats of booking %s: %w", b.ID, err)
	}
	return b, nil
}

func (s bookingStore) LockForUser(ctx context.Context, id, userID uuid.UUID) (domain.Booking, error) {
	var (
		b      domain.Booking
		paidAt *time.Time
	)
	// FOR NO KEY UPDATE: the status UPDATE that follows changes no key column, and this lock does not block
	// the foreign key checks of rows that reference the booking.
	err := s.q.QueryRow(ctx, `
SELECT id, user_id, showtime_id, status, total_cents, expires_at, paid_at, created_at, updated_at
FROM bookings
WHERE id = $1 AND user_id = $2
FOR NO KEY UPDATE`, id, userID).Scan(
		&b.ID, &b.UserID, &b.Showtime.ID, &b.Status, &b.TotalCents, &b.ExpiresAt, &paidAt, &b.CreatedAt, &b.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Booking{}, domain.BookingNotFound(id)
	case pgErrorCode(err) == sqlstateLockNotAvailable:
		return domain.Booking{}, domain.Busy(domain.CodeBookingBusy,
			"booking %s is being changed by another request; try again in a moment", id)
	case err != nil:
		return domain.Booking{}, fmt.Errorf("lock booking %s: %w", id, err)
	}
	if paidAt != nil {
		b.PaidAt = *paidAt
	}
	return b, nil
}

// LockExpired claims due bookings for the expiry worker. Two details let several workers run side by side:
//   - SKIP LOCKED leaves out bookings that another transaction has locked, whether another worker or a user
//     canceling the booking, instead of waiting for them. Concurrent workers therefore take disjoint batches,
//     and the claim never waits for a lock, so it cannot take part in a deadlock.
//   - Under READ COMMITTED, a row that changed after the scan found it is checked against the WHERE clause
//     again once it is locked. A booking that was canceled in the meantime drops out of the batch.
//
// The status filter and the ORDER BY match the partial index bookings_expiry_idx, so a sweep reads only
// live bookings, earliest deadline first.
func (s bookingStore) LockExpired(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `
SELECT id
FROM bookings
WHERE status = 'pending' AND expires_at <= now()
ORDER BY expires_at
LIMIT $1
FOR NO KEY UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("lock expired bookings: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("lock expired bookings: %w", err)
	}
	return ids, nil
}

func (s bookingStore) SetStatus(ctx context.Context, from, to domain.BookingStatus, ids ...uuid.UUID) error {
	tag, err := s.q.Exec(ctx, `
UPDATE bookings
SET status = $3::booking_status, updated_at = now()
WHERE id = ANY($1::uuid[]) AND status = $2::booking_status`, ids, string(from), string(to))
	if err != nil {
		return fmt.Errorf("set status of %d bookings to %s: %w", len(ids), to, err)
	}
	if n := tag.RowsAffected(); n != int64(len(ids)) {
		return fmt.Errorf("set status of %d bookings to %s: only %d were still %s", len(ids), to, n, from)
	}
	return nil
}
