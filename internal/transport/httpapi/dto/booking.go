package dto

import (
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// CreateBookingRequest is the body of POST /v1/bookings.
type CreateBookingRequest struct {
	ShowtimeID int64   `json:"showtime_id"`
	SeatIDs    []int64 `json:"seat_ids"`
}

// NewBooking maps the request to the domain input.
func (r CreateBookingRequest) NewBooking() domain.NewBooking {
	return domain.NewBooking{ShowtimeID: r.ShowtimeID, SeatIDs: r.SeatIDs}
}

// BookingShowtime is the showtime a booking is for.
type BookingShowtime struct {
	ID       int64     `json:"id"`
	Movie    MovieRef  `json:"movie"`
	Hall     HallRef   `json:"hall"`
	StartsAt time.Time `json:"starts_at"`
	Language
}

// BookedSeat is one seat of a booking, at the price it was booked for.
type BookedSeat struct {
	ID         int64  `json:"id"`
	Row        string `json:"row"`
	Number     int    `json:"number"`
	Type       string `json:"type"`
	PriceCents int64  `json:"price_cents"`
}

// Booking is a booking as its owner sees it. The showtime's start carries the cinema's offset, like every
// schedule time; the booking's own timestamps are in UTC.
type Booking struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Showtime   BookingShowtime `json:"showtime"`
	Seats      []BookedSeat    `json:"seats"`
	TotalCents int64           `json:"total_cents"`
	Currency   string          `json:"currency"`
	ExpiresAt  time.Time       `json:"expires_at"`
	PaidAt     *time.Time      `json:"paid_at,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// BookingList is one page of the caller's bookings, newest first. NextCursor is omitted on the last page.
type BookingList struct {
	Items      []Booking `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// NewBooking maps a domain booking.
func NewBooking(b domain.Booking, currency string) Booking {
	seats := make([]BookedSeat, 0, len(b.Seats))
	for _, s := range b.Seats {
		seats = append(seats, BookedSeat{
			ID: s.SeatID, Row: s.Row, Number: s.Number, Type: string(s.Type), PriceCents: s.PriceCents,
		})
	}
	var paidAt *time.Time
	if !b.PaidAt.IsZero() {
		t := b.PaidAt.UTC()
		paidAt = &t
	}
	return Booking{
		ID:     b.ID.String(),
		Status: string(b.Status),
		Showtime: BookingShowtime{
			ID:       b.Showtime.ID,
			Movie:    newMovieRef(b.Showtime.Movie),
			Hall:     HallRef{ID: b.Showtime.Hall.ID, Name: b.Showtime.Hall.Name},
			StartsAt: b.Showtime.StartsAt,
			Language: newLanguage(b.Showtime.Language),
		},
		Seats:      seats,
		TotalCents: b.TotalCents,
		Currency:   currency,
		ExpiresAt:  b.ExpiresAt.UTC(),
		PaidAt:     paidAt,
		CreatedAt:  b.CreatedAt.UTC(),
	}
}

// NewBookingList maps a page of bookings; cursor encodes the id to continue before.
func NewBookingList(page booking.Page, currency string, cursor func(beforeID uuid.UUID) string) BookingList {
	out := BookingList{Items: make([]Booking, 0, len(page.Bookings))}
	for _, b := range page.Bookings {
		out.Items = append(out.Items, NewBooking(b, currency))
	}
	if page.NextBeforeID != (uuid.UUID{}) {
		out.NextCursor = cursor(page.NextBeforeID)
	}
	return out
}
