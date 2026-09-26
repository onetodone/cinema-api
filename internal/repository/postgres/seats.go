package postgres

import (
	"context"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

// seatStore changes the seat inventory inside a transaction. It implements booking.SeatRepo.
type seatStore struct {
	q querier
}

// lockSeatsSelect reads showtime seats and locks them. Two details make it the backbone of the lock order:
//   - PostgreSQL sorts the rows before it locks them, so ORDER BY decides the order in which the locks are
//     taken. Two transactions that want overlapping seats therefore queue on the lowest shared seat instead
//     of each holding a seat the other one waits for.
//   - FOR NO KEY UPDATE is the lock that the later UPDATE of non-key columns takes anyway. Unlike FOR UPDATE,
//     it does not block foreign key checks (FOR KEY SHARE) from booking_seats rows that reference the seat.
//
// OF ss locks only the inventory rows; the hall_seats rows are just read.
const lockSeatsSelect = `
SELECT ss.seat_id, hs.row_label, hs.seat_number, hs.seat_type, ss.price_cents, ss.status
FROM showtime_seats ss
JOIN hall_seats hs ON hs.id = ss.seat_id
`

const lockSeatsOrder = `
ORDER BY ss.showtime_id, ss.seat_id
FOR NO KEY UPDATE OF ss`

func (s seatStore) LockOrdered(ctx context.Context, showtimeID int64, seatIDs []int64) ([]domain.ShowtimeSeat, error) {
	seats, err := s.lock(ctx, lockSeatsSelect+`WHERE ss.showtime_id = $1 AND ss.seat_id = ANY($2::bigint[])`+lockSeatsOrder,
		showtimeID, seatIDs)
	if err != nil {
		return nil, fmt.Errorf("lock seats %v of showtime %d: %w", seatIDs, showtimeID, err)
	}
	return seats, nil
}

func (s seatStore) LockByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.ShowtimeSeat, error) {
	seats, err := s.lock(ctx, lockSeatsSelect+`WHERE ss.booking_id = $1`+lockSeatsOrder, bookingID)
	if err != nil {
		return nil, fmt.Errorf("lock seats of booking %s: %w", bookingID, err)
	}
	return seats, nil
}

func (s seatStore) lock(ctx context.Context, sql string, args ...any) ([]domain.ShowtimeSeat, error) {
	var seats []domain.ShowtimeSeat
	rows, err := s.q.Query(ctx, sql, args...)
	if err == nil {
		seats, err = pgx.CollectRows(rows, scanShowtimeSeat)
	}
	if pgErrorCode(err) == sqlstateLockNotAvailable {
		return nil, domain.Busy(domain.CodeSeatBusy,
			"the seats are locked by another booking in progress; try again in a moment")
	}
	return seats, err
}

// Hold flips available seats to held. Repeating status = 'available' makes the UPDATE a compare-and-set on its
// own: even without the preceding lock, a seat that is already held could not be taken over.
func (s seatStore) Hold(ctx context.Context, showtimeID int64, seatIDs []int64, bookingID uuid.UUID) (int64, error) {
	tag, err := s.q.Exec(ctx, `
UPDATE showtime_seats
SET status = 'held', booking_id = $3, updated_at = now()
WHERE showtime_id = $1 AND seat_id = ANY($2::bigint[]) AND status = 'available'`,
		showtimeID, seatIDs, bookingID)
	if err != nil {
		return 0, fmt.Errorf("hold seats %v of showtime %d: %w", seatIDs, showtimeID, err)
	}
	return tag.RowsAffected(), nil
}

func (s seatStore) Release(ctx context.Context, bookingID uuid.UUID) (int64, error) {
	tag, err := s.q.Exec(ctx, `
UPDATE showtime_seats
SET status = 'available', booking_id = NULL, updated_at = now()
WHERE booking_id = $1 AND status = 'held'`, bookingID)
	if err != nil {
		return 0, fmt.Errorf("release seats of booking %s: %w", bookingID, err)
	}
	return tag.RowsAffected(), nil
}

func scanShowtimeSeat(row pgx.CollectableRow) (domain.ShowtimeSeat, error) {
	var s domain.ShowtimeSeat
	err := row.Scan(&s.SeatID, &s.Row, &s.Number, &s.Type, &s.PriceCents, &s.Status)
	return s, err
}
