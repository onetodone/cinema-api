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
	payments  map[uuid.UUID]domain.Payment
}

func (s memState) clone() memState {
	return memState{
		showtimes: maps.Clone(s.showtimes),
		seats:     maps.Clone(s.seats),
		bookings:  maps.Clone(s.bookings),
		payments:  maps.Clone(s.payments),
	}
}

// memDB is an in-memory UnitOfWork and Reader. Do runs one transaction at a time on a copy of the state and
// keeps the copy only when fn succeeds, which behaves like a serializable database with rollback.
type memDB struct {
	mu    sync.Mutex
	state memState
	now   time.Time

	calls         []string // repository calls of the last transaction, for checking the lock order
	holdShortfall int64    // Hold reports this many fewer changed seats than it changed
	releaseExtra  int64    // Release reports this many more changed seats than it changed
	sellShortfall int64    // Sell reports this many fewer changed seats than it changed
	finishErr     error    // Finish fails with this error
	listLimit     int      // the limit the last ListBookings call received
}

func newMemDB(now time.Time) *memDB {
	return &memDB{
		now: now,
		state: memState{
			showtimes: map[int64]memShowtime{},
			seats:     map[seatKey]memSeat{},
			bookings:  map[uuid.UUID]domain.Booking{},
			payments:  map[uuid.UUID]domain.Payment{},
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

func (db *memDB) payment(id uuid.UUID) domain.Payment {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.state.payments[id]
}

// paymentsOf returns the payments of a booking, oldest first.
func (db *memDB) paymentsOf(bookingID uuid.UUID) []domain.Payment {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []domain.Payment
	for _, p := range db.state.payments {
		if p.BookingID == bookingID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b domain.Payment) int { return a.ID.Compare(b.ID) })
	return out
}

// lastCalls returns the repository calls of the last transaction.
func (db *memDB) lastCalls() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	return slices.Clone(db.calls)
}

func (db *memDB) setFinishErr(err error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.finishErr = err
}

// advance moves the fake database clock forward.
func (db *memDB) advance(d time.Duration) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.now = db.now.Add(d)
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

func (db *memDB) ListStuckPayments(_ context.Context, age time.Duration, afterID uuid.UUID, limit int) ([]domain.Payment, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []domain.Payment
	for _, p := range db.state.payments {
		if p.Status == domain.PaymentPending && !p.CreatedAt.After(db.now.Add(-age)) && p.ID.Compare(afterID) > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b domain.Payment) int { return a.ID.Compare(b.ID) })
	return out[:min(limit, len(out))], nil
}

// memTx implements TxRepos and the showtime, seat, and booking repositories on a private copy of the state.
type memTx struct {
	db    *memDB
	state memState
}

func (tx *memTx) Showtimes() ShowtimeRepo { return tx }
func (tx *memTx) Seats() SeatRepo         { return tx }
func (tx *memTx) Bookings() Repo          { return tx }
func (tx *memTx) Payments() PaymentRepo   { return memPayments{tx} }

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

func (tx *memTx) LockByBookings(_ context.Context, bookingIDs ...uuid.UUID) ([]domain.ShowtimeSeat, error) {
	tx.record("lock seats of %d bookings", len(bookingIDs))
	var out []domain.ShowtimeSeat
	for _, s := range tx.state.seats {
		if slices.Contains(bookingIDs, s.bookingID) {
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

func (tx *memTx) Release(_ context.Context, bookingIDs ...uuid.UUID) (int64, error) {
	tx.record("release seats")
	var n int64
	for k, s := range tx.state.seats {
		if slices.Contains(bookingIDs, s.bookingID) && s.seat.Status == domain.SeatHeld {
			s.seat.Status, s.bookingID = domain.SeatAvailable, uuid.UUID{}
			tx.state.seats[k] = s
			n++
		}
	}
	return n + tx.db.releaseExtra, nil
}

func (tx *memTx) Sell(_ context.Context, bookingID uuid.UUID) (int64, error) {
	tx.record("sell seats")
	var n int64
	for k, s := range tx.state.seats {
		if s.bookingID == bookingID && s.seat.Status == domain.SeatHeld {
			s.seat.Status = domain.SeatSold
			tx.state.seats[k] = s
			n++
		}
	}
	return n - tx.db.sellShortfall, nil
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

func (tx *memTx) LockForUser(_ context.Context, id, userID uuid.UUID) (domain.Booking, bool, error) {
	tx.record("lock booking")
	b, ok := tx.state.bookings[id]
	if !ok || b.UserID != userID {
		return domain.Booking{}, false, domain.BookingNotFound(id)
	}
	return tx.ownColumns(b), !b.ExpiresAt.After(tx.db.now), nil
}

func (tx *memTx) Lock(_ context.Context, id uuid.UUID) (domain.Booking, bool, error) {
	tx.record("lock booking")
	b, ok := tx.state.bookings[id]
	if !ok {
		return domain.Booking{}, false, domain.BookingNotFound(id)
	}
	return tx.ownColumns(b), !b.ExpiresAt.After(tx.db.now), nil
}

// ownColumns keeps what a lock loads from the bookings row alone.
func (*memTx) ownColumns(b domain.Booking) domain.Booking {
	return domain.Booking{
		ID: b.ID, UserID: b.UserID, Showtime: domain.ShowtimeRef{ID: b.Showtime.ID}, Status: b.Status,
		TotalCents: b.TotalCents, ExpiresAt: b.ExpiresAt, PaidAt: b.PaidAt, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

// LockExpired returns the pending bookings whose hold ended at or before the fake clock. The fake runs one
// transaction at a time, so there is nothing to skip.
func (tx *memTx) LockExpired(_ context.Context, limit int) ([]uuid.UUID, error) {
	tx.record("lock expired bookings")
	var due []domain.Booking
	for _, b := range tx.state.bookings {
		if b.Status == domain.BookingPending && !b.ExpiresAt.After(tx.db.now) {
			due = append(due, b)
		}
	}
	slices.SortFunc(due, func(a, b domain.Booking) int { return a.ExpiresAt.Compare(b.ExpiresAt) })
	ids := make([]uuid.UUID, 0, min(limit, len(due)))
	for _, b := range due[:min(limit, len(due))] {
		ids = append(ids, b.ID)
	}
	return ids, nil
}

func (tx *memTx) SetStatus(_ context.Context, from, to domain.BookingStatus, ids ...uuid.UUID) error {
	tx.record("set status %s", to)
	for _, id := range ids {
		b := tx.state.bookings[id]
		if b.Status != from {
			return fmt.Errorf("booking %s is no longer %s", id, from)
		}
		b.Status = to
		if to == domain.BookingPaid {
			b.PaidAt = tx.db.now
		}
		tx.state.bookings[id] = b
	}
	return nil
}

// memPayments implements PaymentRepo on a transaction of the fake.
type memPayments struct {
	tx *memTx
}

func (m memPayments) Create(_ context.Context, p domain.Payment) (domain.Payment, error) {
	m.tx.record("insert payment")
	for _, other := range m.tx.state.payments {
		if other.BookingID == p.BookingID && other.Status == domain.PaymentPending {
			return domain.Payment{}, domain.Conflict(domain.CodePaymentInProgress, "one pending payment per booking")
		}
	}
	p.Status = domain.PaymentPending
	p.CreatedAt, p.UpdatedAt = m.tx.db.now, m.tx.db.now
	m.tx.state.payments[p.ID] = p
	return p, nil
}

func (m memPayments) Get(_ context.Context, id uuid.UUID) (domain.Payment, error) {
	m.tx.record("get payment")
	p, ok := m.tx.state.payments[id]
	if !ok {
		return domain.Payment{}, fmt.Errorf("payment %s not found", id)
	}
	return p, nil
}

func (m memPayments) Finish(_ context.Context, id uuid.UUID, from domain.PaymentStatus, out domain.PaymentOutcome) (domain.Payment, error) {
	m.tx.record("finish payment %s", out.Status)
	if m.tx.db.finishErr != nil {
		return domain.Payment{}, m.tx.db.finishErr
	}
	p := m.tx.state.payments[id]
	if p.Status != from {
		return domain.Payment{}, fmt.Errorf("payment %s is no longer %s", id, from)
	}
	p.Status, p.ProviderRef, p.FailureReason, p.UpdatedAt = out.Status, out.ProviderRef, out.FailureReason, m.tx.db.now
	m.tx.state.payments[id] = p
	return p, nil
}
