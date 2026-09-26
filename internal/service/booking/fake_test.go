package booking

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
)

type seatKey struct {
	showtimeID int64
	seatID     int64
}

type memSeat struct {
	seat      domain.ShowtimeSeat
	bookingID uuid.UUID
}

type memShowtime struct {
	ref     domain.ShowtimeRef
	started bool
}

// memState is the data of the fake database.
type memState struct {
	showtimes map[int64]memShowtime
	seats     map[seatKey]memSeat
	bookings  map[uuid.UUID]domain.Booking
}

func (s memState) clone() memState {
	return memState{
		showtimes: maps.Clone(s.showtimes),
		seats:     maps.Clone(s.seats),
		bookings:  maps.Clone(s.bookings),
	}
}

// memDB is an in-memory UnitOfWork and Reader. Do runs one transaction at a time on a copy of the state and
// keeps the copy only when fn succeeds, which behaves like a serializable database with rollback.
type memDB struct {
	mu    sync.Mutex
	state memState
	now   time.Time

	calls         []string // repository calls of the last transactions, for checking the lock order
	holdShortfall int64    // Hold reports this many fewer changed seats than it changed
	releaseExtra  int64    // Release reports this many more changed seats than it changed
	listLimit     int      // the limit the last ListBookings call received
}

func newMemDB(now time.Time) *memDB {
	return &memDB{
		now: now,
		state: memState{
			showtimes: map[int64]memShowtime{},
			seats:     map[seatKey]memSeat{},
			bookings:  map[uuid.UUID]domain.Booking{},
		},
	}
}

// addShowtime adds a showtime whose seats have ids first, first+1, ... at the given prices.
func (db *memDB) addShowtime(ref domain.ShowtimeRef, started bool, first int64, prices ...int64) {
	db.state.showtimes[ref.ID] = memShowtime{ref: ref, started: started}
	for i, price := range prices {
		id := first + int64(i)
		typ := domain.SeatStandard
		if price > 1000 {
			typ = domain.SeatVIP
		}
		db.state.seats[seatKey{ref.ID, id}] = memSeat{seat: domain.ShowtimeSeat{
			// Row "B" for even ids and "A" for odd ones, so seat id order differs from seat map order.
			SeatID: id, Row: map[bool]string{true: "B", false: "A"}[id%2 == 0], Number: int(id),
			Type: typ, PriceCents: price, Status: domain.SeatAvailable,
		}}
	}
}

func (db *memDB) seat(showtimeID, seatID int64) memSeat {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.state.seats[seatKey{showtimeID, seatID}]
}

func (db *memDB) booking(id uuid.UUID) (domain.Booking, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	b, ok := db.state.bookings[id]
	return b, ok
}

func (db *memDB) setBookingStatus(id uuid.UUID, status domain.BookingStatus) {
	db.mu.Lock()
	defer db.mu.Unlock()
	b := db.state.bookings[id]
	b.Status = status
	db.state.bookings[id] = b
}

func (db *memDB) Do(ctx context.Context, fn func(ctx context.Context, r TxRepos) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.calls = nil
	tx := &memTx{db: db, state: db.state.clone()}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	db.state = tx.state
	return nil
}

func (db *memDB) GetBooking(_ context.Context, id, userID uuid.UUID) (domain.Booking, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	b, ok := db.state.bookings[id]
	if !ok || b.UserID != userID {
		return domain.Booking{}, domain.BookingNotFound(id)
	}
	return b, nil
}

func (db *memDB) ListBookings(_ context.Context, userID, beforeID uuid.UUID, limit int) ([]domain.Booking, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.listLimit = limit
	var out []domain.Booking
	for _, b := range db.state.bookings {
		if b.UserID == userID && b.ID.Compare(beforeID) < 0 {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b domain.Booking) int { return b.ID.Compare(a.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// memTx implements TxRepos and all three repositories on a private copy of the state.
type memTx struct {
	db    *memDB
	state memState
}

func (tx *memTx) Showtimes() ShowtimeRepo { return tx }
func (tx *memTx) Seats() SeatRepo         { return tx }
func (tx *memTx) Bookings() Repo          { return tx }

func (tx *memTx) record(format string, args ...any) {
	tx.db.calls = append(tx.db.calls, fmt.Sprintf(format, args...))
}

func (tx *memTx) GetForBooking(_ context.Context, id int64) (domain.ShowtimeRef, bool, error) {
	st, ok := tx.state.showtimes[id]
	if !ok {
		return domain.ShowtimeRef{}, false, domain.NotFound(domain.CodeShowtimeNotFound, "showtime %d not found", id)
	}
	return st.ref, st.started, nil
}

func (tx *memTx) LockOrdered(_ context.Context, showtimeID int64, seatIDs []int64) ([]domain.ShowtimeSeat, error) {
	tx.record("lock seats %v", seatIDs)
	var out []domain.ShowtimeSeat
	for _, id := range seatIDs {
		if s, ok := tx.state.seats[seatKey{showtimeID, id}]; ok {
			out = append(out, s.seat)
		}
	}
	slices.SortFunc(out, func(a, b domain.ShowtimeSeat) int { return cmp.Compare(a.SeatID, b.SeatID) })
	return out, nil
}

func (tx *memTx) LockByBooking(_ context.Context, bookingID uuid.UUID) ([]domain.ShowtimeSeat, error) {
	tx.record("lock seats of booking")
	var out []domain.ShowtimeSeat
	for _, s := range tx.state.seats {
		if s.bookingID == bookingID {
			out = append(out, s.seat)
		}
	}
	return out, nil
}

func (tx *memTx) Hold(_ context.Context, showtimeID int64, seatIDs []int64, bookingID uuid.UUID) (int64, error) {
	tx.record("hold seats %v", seatIDs)
	var n int64
	for _, id := range seatIDs {
		k := seatKey{showtimeID, id}
		if s, ok := tx.state.seats[k]; ok && s.seat.Status == domain.SeatAvailable {
			s.seat.Status, s.bookingID = domain.SeatHeld, bookingID
			tx.state.seats[k] = s
			n++
		}
	}
	return n - tx.db.holdShortfall, nil
}

func (tx *memTx) Release(_ context.Context, bookingID uuid.UUID) (int64, error) {
	tx.record("release seats")
	var n int64
	for k, s := range tx.state.seats {
		if s.bookingID == bookingID && s.seat.Status == domain.SeatHeld {
			s.seat.Status, s.bookingID = domain.SeatAvailable, uuid.UUID{}
			tx.state.seats[k] = s
			n++
		}
	}
	return n + tx.db.releaseExtra, nil
}

func (tx *memTx) Create(_ context.Context, b domain.Booking, holdTTL time.Duration) (domain.Booking, error) {
	tx.record("insert booking")
	for _, other := range tx.state.bookings {
		if other.UserID == b.UserID && other.Showtime.ID == b.Showtime.ID && other.Status.Active() {
			return domain.Booking{}, domain.Conflict(domain.CodeActiveBookingExists, "one active booking per showtime")
		}
	}
	b.Status = domain.BookingPending
	b.ExpiresAt = tx.db.now.Add(holdTTL)
	b.CreatedAt, b.UpdatedAt = tx.db.now, tx.db.now
	tx.state.bookings[b.ID] = b
	return b, nil
}

func (tx *memTx) LockForUser(_ context.Context, id, userID uuid.UUID) (domain.Booking, error) {
	tx.record("lock booking")
	b, ok := tx.state.bookings[id]
	if !ok || b.UserID != userID {
		return domain.Booking{}, domain.BookingNotFound(id)
	}
	return domain.Booking{ID: b.ID, UserID: b.UserID, Showtime: domain.ShowtimeRef{ID: b.Showtime.ID}, Status: b.Status}, nil
}

func (tx *memTx) SetStatus(_ context.Context, id uuid.UUID, from, to domain.BookingStatus) error {
	tx.record("set status %s", to)
	b := tx.state.bookings[id]
	if b.Status != from {
		return fmt.Errorf("booking %s is no longer %s", id, from)
	}
	b.Status = to
	tx.state.bookings[id] = b
	return nil
}
