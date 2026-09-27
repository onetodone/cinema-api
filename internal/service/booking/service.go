// Package booking implements seat holds and their payment: creating, reading, listing, canceling, expiring,
// and paying for bookings, and settling payments whose outcome the API did not record.
package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Paging limits for booking lists.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Config holds the booking rules that come from configuration.
type Config struct {
	Location *time.Location // the cinema's time zone, for the showtimes of returned bookings
	Currency string         // ISO 4217 code of all prices, sent to payment providers
	HoldTTL  time.Duration  // how long a booking holds its seats while waiting for payment
	MaxSeats int            // most seats one booking may hold
	// HoldClaimTTL is how long the hold gate reserves seats for a booking whose transaction has not committed.
	HoldClaimTTL time.Duration
	// PaymentTimeout bounds each call to a payment provider.
	PaymentTimeout time.Duration
	// PaymentGrace is how long a payment may stay in flight before ReconcileBatch asks its provider about it.
	// It must be longer than PaymentTimeout, so that the API has given up on the charge by then.
	PaymentGrace time.Duration
}

// Service runs the booking use cases. Times of returned showtimes are in the cinema's time zone.
type Service struct {
	uow       UnitOfWork
	reader    Reader
	providers PaymentProviders
	cfg       Config
	logger    *slog.Logger
	gate      HoldGate     // nil: no gate, the database alone turns requests for taken seats away
	seatMaps  SeatMapCache // nil: no seat map cache to invalidate
}

// Option customizes a Service.
type Option func(*Service)

// WithHoldGate checks requested seats against g before the database locks them (see Create).
func WithHoldGate(g HoldGate) Option {
	return func(s *Service) { s.gate = g }
}

// WithSeatMapCache invalidates the cached seat maps of c after every committed seat change.
func WithSeatMapCache(c SeatMapCache) Option {
	return func(s *Service) { s.seatMaps = c }
}

// New returns a booking Service.
func New(uow UnitOfWork, reader Reader, providers PaymentProviders, cfg Config, logger *slog.Logger, opts ...Option) *Service {
	s := &Service{uow: uow, reader: reader, providers: providers, cfg: cfg, logger: logger}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create holds seats of one showtime for userID until the hold expires. The request is all or nothing: if
// any seat is taken, nothing is held and the error lists every taken seat.
//
// Errors: a *domain.ValidationError for bad input; SHOWTIME_NOT_FOUND; SHOWTIME_NOT_BOOKABLE for a canceled
// or started showtime; UNKNOWN_SEAT for seats that are not part of the showtime; a *domain.SeatsUnavailableError
// (SEAT_UNAVAILABLE); SEAT_BUSY when the seats stay locked by other bookings for longer than the lock timeout;
// a *domain.ActiveBookingExistsError (ACTIVE_BOOKING_EXISTS) when the user already has an unpaid booking for the
// showtime, naming that booking.
//
// How double-selling is prevented: the transaction locks the seat rows in seat id order, checks under the
// lock that every seat is available, and then flips them to held with an UPDATE that repeats the
// status = 'available' condition. A competing transaction waits on the row locks and, once the winner has
// committed, sees the seats as held.
//
// With a hold gate, the seats are claimed in the gate first. A request whose seats another booking holds or claims
// fails with SEAT_UNAVAILABLE right there, listing the seats the gate knows to be taken, without touching the
// database. After the transaction, the claim is extended to the booking's deadline, or released if the
// transaction failed. When the gate cannot be reached, the request goes to the database unchecked.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, nb domain.NewBooking) (domain.Booking, error) {
	if err := nb.Validate(s.cfg.MaxSeats); err != nil {
		return domain.Booking{}, err
	}
	seatIDs := slices.Sorted(slices.Values(nb.SeatIDs))
	// The id exists before the transaction does. Retries of the transaction reuse it, and the hold gate uses it
	// as the token of its claim.
	id := uuid.NewV7()

	claimed, err := s.claimSeats(ctx, nb.ShowtimeID, seatIDs, id)
	if err != nil {
		return domain.Booking{}, err
	}

	var created domain.Booking
	err = s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		st, started, err := r.Showtimes().GetForBooking(ctx, nb.ShowtimeID)
		if err != nil {
			return err
		}
		if err := domain.CheckBookable(st, started); err != nil {
			return err
		}

		seats, err := r.Seats().LockOrdered(ctx, st.ID, seatIDs)
		if err != nil {
			return err
		}
		booked, err := bookableSeats(st.ID, seatIDs, seats)
		if err != nil {
			return err
		}

		var total int64
		for _, seat := range booked {
			total += seat.PriceCents
		}
		b, err := r.Bookings().Create(ctx, domain.Booking{
			ID:         id,
			UserID:     userID,
			Showtime:   st,
			Status:     domain.BookingPending,
			Seats:      booked,
			TotalCents: total,
		}, s.cfg.HoldTTL)
		if err != nil {
			return err
		}

		held, err := r.Seats().Hold(ctx, st.ID, seatIDs, id)
		if err != nil {
			return err
		}
		if held != int64(len(seatIDs)) {
			// Impossible while the locks from LockOrdered are held. If it happens anyway, rolling back is the
			// only safe answer.
			return fmt.Errorf("hold seats for booking %s: %d of %d seats changed", id, held, len(seatIDs))
		}

		created = b
		return nil
	})

	// The outcome is decided; a client that hangs up now must not leave the gate or the cache behind.
	after := context.WithoutCancel(ctx)
	if err != nil {
		if claimed {
			_ = s.gate.Release(after, nb.ShowtimeID, seatIDs, id) // on failure, the claim expires by itself
		}
		return domain.Booking{}, s.nameActiveBooking(ctx, userID, nb.ShowtimeID, err)
	}
	if claimed {
		// The gate now rejects contenders for as long as the database holds the seats.
		_ = s.gate.ExtendUntil(after, created.Showtime.ID, seatIDs, id, created.ExpiresAt)
	}
	s.seatsChanged(after, created.Showtime.ID)
	return s.localize(created), nil
}

// nameActiveBooking adds the id of the blocking booking to an ACTIVE_BOOKING_EXISTS error, so that a client can
// offer to continue with that booking or to cancel it. The unique index violation aborted the transaction, so
// the booking is read after it, outside any transaction. If it ended in the meantime, or the read fails, the
// error goes out as it is: the conflict itself is certain, the id only a convenience. Other errors pass through.
func (s *Service) nameActiveBooking(ctx context.Context, userID uuid.UUID, showtimeID int64, err error) error {
	var active *domain.ActiveBookingExistsError
	if !errors.As(err, &active) || active.BookingID != (uuid.UUID{}) {
		return err
	}
	id, lookupErr := s.reader.ActiveBookingID(ctx, userID, showtimeID)
	if lookupErr != nil {
		if ctx.Err() == nil { // a client that hung up is not worth a warning
			s.logger.WarnContext(ctx, "active booking lookup failed; answering without its id",
				slog.Int64("showtime_id", showtimeID), slog.Any("error", lookupErr))
		}
		return err
	}
	if id == (uuid.UUID{}) {
		return err
	}
	return domain.ActiveBookingExists(showtimeID, id)
}

// claimSeats claims the seats in the hold gate for the booking token and reports whether it did. Seats that
// other bookings hold or claim fail with a *domain.SeatsUnavailableError. Without a gate, or when the gate
// fails, nothing is claimed and the database decides alone.
func (s *Service) claimSeats(ctx context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID) (bool, error) {
	if s.gate == nil {
		return false, nil
	}
	conflicts, err := s.gate.Acquire(ctx, showtimeID, seatIDs, token, s.cfg.HoldClaimTTL)
	if len(conflicts) > 0 {
		return false, domain.SeatsUnavailable(conflicts)
	}
	// A gate that failed claimed nothing, and the database decides alone: fail open. The gate recorded the failure.
	return err == nil, nil
}

// seatsChanged invalidates the cached seat maps of showtimes whose seats a committed transaction changed.
func (s *Service) seatsChanged(ctx context.Context, showtimeIDs ...int64) {
	if s.seatMaps == nil || len(showtimeIDs) == 0 {
		return
	}
	_ = s.seatMaps.InvalidateSeatMaps(ctx, showtimeIDs...) // on failure, the cached seat maps expire by themselves
}

// releaseClaims frees the hold gate claims of bookings whose seats a committed transaction released, as they
// were locked: each seat carries its holder. A claim would expire at the booking's deadline by itself; releasing
// it at once lets contenders through without waiting for that, and keeps the gate right even if the clocks of
// Redis and the database disagree. If it fails, the gate turns contenders for those seats away until then,
// though the seat map shows the seats available.
func (s *Service) releaseClaims(ctx context.Context, bookings []domain.Booking, released []domain.ShowtimeSeat) {
	if s.gate == nil {
		return
	}
	seatsOf := make(map[uuid.UUID][]int64, len(bookings))
	for _, seat := range released {
		seatsOf[seat.BookingID] = append(seatsOf[seat.BookingID], seat.SeatID)
	}
	for _, b := range bookings {
		if seatIDs := seatsOf[b.ID]; len(seatIDs) > 0 {
			_ = s.gate.Release(ctx, b.Showtime.ID, seatIDs, b.ID) // the gate records a failure
		}
	}
}

// bookableSeats checks the locked seats against the requested ones and returns them in seat map order.
func bookableSeats(showtimeID int64, requested []int64, locked []domain.ShowtimeSeat) ([]domain.BookedSeat, error) {
	if len(locked) != len(requested) {
		var unknown []int64
		for _, id := range requested {
			if !slices.ContainsFunc(locked, func(s domain.ShowtimeSeat) bool { return s.SeatID == id }) {
				unknown = append(unknown, id)
			}
		}
		return nil, domain.UnknownSeats(showtimeID, unknown)
	}

	var taken []int64
	booked := make([]domain.BookedSeat, 0, len(locked))
	for _, seat := range locked {
		if seat.Status != domain.SeatAvailable {
			taken = append(taken, seat.SeatID)
			continue
		}
		booked = append(booked, domain.BookedSeat{
			SeatID: seat.SeatID, Row: seat.Row, Number: seat.Number, Type: seat.Type, PriceCents: seat.PriceCents,
		})
	}
	if len(taken) > 0 {
		return nil, domain.SeatsUnavailable(taken)
	}

	slices.SortFunc(booked, func(a, b domain.BookedSeat) int {
		return domain.CompareSeatPositions(a.Row, a.Number, b.Row, b.Number)
	})
	return booked, nil
}

// Get returns a booking of userID. Bookings of other users fail with BOOKING_NOT_FOUND.
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (domain.Booking, error) {
	b, err := s.reader.GetBooking(ctx, id, userID)
	if err != nil {
		return domain.Booking{}, err
	}
	return s.localize(b), nil
}

// Page is one page of bookings, newest first. NextBeforeID is the zero UUID on the last page.
type Page struct {
	Bookings     []domain.Booking
	NextBeforeID uuid.UUID
}

// ListQuery selects a page of a user's bookings.
type ListQuery struct {
	// BeforeID continues the list with the bookings older than this one; the zero UUID starts at the newest.
	BeforeID uuid.UUID
	// Limit is the page size; outside [1, MaxPageSize] it is clamped.
	Limit int
	// Statuses keeps only the bookings in one of these statuses; empty keeps them all. The next page needs the
	// same statuses.
	Statuses []domain.BookingStatus
}

// List returns one page of the bookings of userID that q selects, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, q ListQuery) (Page, error) {
	limit, beforeID := q.Limit, q.BeforeID
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	if beforeID == (uuid.UUID{}) {
		beforeID = uuid.Max()
	}

	// Fetch one extra row to learn whether another page exists without a COUNT query.
	list, err := s.reader.ListBookings(ctx, userID, beforeID, q.Statuses, limit+1)
	if err != nil {
		return Page{}, err
	}

	page := Page{Bookings: list}
	if len(list) > limit {
		page.Bookings = list[:limit]
		page.NextBeforeID = page.Bookings[limit-1].ID
	}
	for i := range page.Bookings {
		page.Bookings[i] = s.localize(page.Bookings[i])
	}
	return page, nil
}

// Cancel releases the seats of a pending booking of userID. Canceling a booking that is already canceled or
// expired succeeds without changes, so a retried request gets the same answer. A booking with a payment in
// progress, or a paid one, fails with BOOKING_NOT_CANCELABLE.
func (s *Service) Cancel(ctx context.Context, userID, id uuid.UUID) error {
	var (
		canceled domain.Booking
		released []domain.ShowtimeSeat
	)
	err := s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		released = nil // a retried attempt starts from scratch
		// Lock order: the booking first, then its seats.
		b, _, err := r.Bookings().LockForUser(ctx, id, userID)
		if err != nil {
			return err
		}
		switch {
		case b.Status == domain.BookingCanceled || b.Status == domain.BookingExpired:
			return nil
		case !b.Status.CanBecome(domain.BookingCanceled):
			return domain.Conflict(domain.CodeBookingNotCancelable,
				"booking %s is %s and can no longer be canceled", id, b.Status)
		}

		seats, err := releaseSeats(ctx, r, id)
		if err != nil {
			return err
		}
		if err := r.Bookings().SetStatus(ctx, b.Status, domain.BookingCanceled, id); err != nil {
			return err
		}
		canceled, released = b, seats
		return nil
	})
	if err != nil || len(released) == 0 {
		return err
	}

	after := context.WithoutCancel(ctx)
	s.releaseClaims(after, []domain.Booking{canceled}, released)
	s.seatsChanged(after, canceled.Showtime.ID)
	return nil
}

// ExpiredBatch reports what one ExpireBatch call changed.
type ExpiredBatch struct {
	BookingIDs []uuid.UUID // the expired bookings, earliest deadline first
	Seats      int64       // seats made available again
}

// ExpireBatch expires up to limit pending bookings whose hold has run out, judged by the database clock, and
// makes their seats available again, all in one transaction. A batch with fewer than limit bookings means
// that no more were due when it ran.
//
// Any number of callers may run at once, in one process or in several. Each locks the bookings it takes and
// skips those locked by someone else, so every booking is expired exactly once and callers never wait for
// each other. A booking that its owner is canceling at that moment is skipped as well; if it is still pending
// afterwards, a later batch takes it.
//
// Bookings with a payment in progress are left alone.
func (s *Service) ExpireBatch(ctx context.Context, limit int) (ExpiredBatch, error) {
	if limit < 1 {
		return ExpiredBatch{}, fmt.Errorf("expire bookings: limit must be positive, got %d", limit)
	}

	var (
		batch     ExpiredBatch
		due       []domain.Booking
		released  []domain.ShowtimeSeat
		showtimes []int64
	)
	err := s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		batch, showtimes = ExpiredBatch{}, nil // a retried attempt starts from scratch
		// Lock order: the bookings first, then their seats.
		var err error
		if due, err = r.Bookings().LockExpired(ctx, limit); err != nil || len(due) == 0 {
			return err
		}
		ids := make([]uuid.UUID, len(due))
		for i, b := range due {
			ids[i] = b.ID
			if !slices.Contains(showtimes, b.Showtime.ID) {
				showtimes = append(showtimes, b.Showtime.ID)
			}
		}
		if released, err = releaseSeats(ctx, r, ids...); err != nil {
			return err
		}
		if err := r.Bookings().SetStatus(ctx, domain.BookingPending, domain.BookingExpired, ids...); err != nil {
			return err
		}
		batch = ExpiredBatch{BookingIDs: ids, Seats: int64(len(released))}
		return nil
	})
	if err != nil {
		return ExpiredBatch{}, err
	}

	after := context.WithoutCancel(ctx)
	s.releaseClaims(after, due, released)
	s.seatsChanged(after, showtimes...)
	return batch, nil
}

// releaseSeats locks the seats of bookings whose rows the caller has locked, makes them available, and returns
// them. Every seat of an unpaid booking is held, so a count that differs from the locked seats means the
// inventory is inconsistent, and the transaction must not commit.
func releaseSeats(ctx context.Context, r TxRepos, bookingIDs ...uuid.UUID) ([]domain.ShowtimeSeat, error) {
	seats, err := r.Seats().LockByBookings(ctx, bookingIDs...)
	if err != nil {
		return nil, err
	}
	released, err := r.Seats().Release(ctx, bookingIDs...)
	if err != nil {
		return nil, err
	}
	if released != int64(len(seats)) {
		return nil, fmt.Errorf("release seats: %d of %d locked seats changed", released, len(seats))
	}
	return seats, nil
}

func (s *Service) localize(b domain.Booking) domain.Booking {
	b.Showtime.StartsAt = b.Showtime.StartsAt.In(s.cfg.Location)
	return b
}
