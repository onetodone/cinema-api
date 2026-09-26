// Package booking implements seat holds: creating, reading, listing, and canceling bookings.
package booking

import (
	"context"
	"fmt"
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
	HoldTTL  time.Duration // how long a booking holds its seats while waiting for payment
	MaxSeats int           // most seats one booking may hold
}

// Service runs the booking use cases. Times of returned showtimes are in the cinema's time zone.
type Service struct {
	uow    UnitOfWork
	reader Reader
	loc    *time.Location
	cfg    Config
}

// New returns a booking Service. loc is the cinema's time zone.
func New(uow UnitOfWork, reader Reader, loc *time.Location, cfg Config) *Service {
	return &Service{uow: uow, reader: reader, loc: loc, cfg: cfg}
}

// Create holds seats of one showtime for userID until the hold expires. The request is all or nothing: if
// any seat is taken, nothing is held and the error lists every taken seat.
//
// Errors: a *domain.ValidationError for bad input; SHOWTIME_NOT_FOUND; SHOWTIME_NOT_BOOKABLE for a canceled
// or started showtime; UNKNOWN_SEAT for seats that are not part of the showtime; a *domain.SeatsUnavailableError
// (SEAT_UNAVAILABLE); SEAT_BUSY when the seats stay locked by other bookings for longer than the lock timeout;
// ACTIVE_BOOKING_EXISTS when the user already has an unpaid booking for the showtime.
//
// How double-selling is prevented: the transaction locks the seat rows in seat id order, checks under the
// lock that every seat is available, and then flips them to held with an UPDATE that repeats the
// status = 'available' condition. A competing transaction waits on the row locks and, once the winner has
// committed, sees the seats as held.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, nb domain.NewBooking) (domain.Booking, error) {
	if err := nb.Validate(s.cfg.MaxSeats); err != nil {
		return domain.Booking{}, err
	}
	seatIDs := slices.Sorted(slices.Values(nb.SeatIDs))
	// The id exists before the transaction does. Retries of the transaction reuse it, and the Redis hold gate
	// will use it as its owner token.
	id := uuid.NewV7()

	var created domain.Booking
	err := s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
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
	if err != nil {
		return domain.Booking{}, err
	}
	return s.localize(created), nil
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

// List returns the bookings of userID that are older than beforeID, newest first. The zero beforeID starts
// at the newest booking. A limit outside [1, MaxPageSize] is clamped.
func (s *Service) List(ctx context.Context, userID, beforeID uuid.UUID, limit int) (Page, error) {
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
	list, err := s.reader.ListBookings(ctx, userID, beforeID, limit+1)
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
	return s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		// Lock order: the booking first, then its seats.
		b, err := r.Bookings().LockForUser(ctx, id, userID)
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

		seats, err := r.Seats().LockByBooking(ctx, id)
		if err != nil {
			return err
		}
		released, err := r.Seats().Release(ctx, id)
		if err != nil {
			return err
		}
		if released != int64(len(seats)) {
			return fmt.Errorf("release seats of booking %s: %d of %d seats changed", id, released, len(seats))
		}
		return r.Bookings().SetStatus(ctx, id, b.Status, domain.BookingCanceled)
	})
}

func (s *Service) localize(b domain.Booking) domain.Booking {
	b.Showtime.StartsAt = b.Showtime.StartsAt.In(s.loc)
	return b
}
