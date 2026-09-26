package dto

import (
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// CreateMovieRequest is the body of POST /v1/admin/movies.
type CreateMovieRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	DurationMin int    `json:"duration_min"`
	AgeRating   string `json:"age_rating"`
	PosterURL   string `json:"poster_url"`
}

// NewMovie maps the request to the domain input.
func (r CreateMovieRequest) NewMovie() domain.NewMovie {
	return domain.NewMovie{
		Title:       r.Title,
		Description: r.Description,
		DurationMin: r.DurationMin,
		AgeRating:   r.AgeRating,
		PosterURL:   r.PosterURL,
	}
}

// CreateHallRequest is the body of POST /v1/admin/halls.
type CreateHallRequest struct {
	Name string    `json:"name"`
	Rows []HallRow `json:"rows"`
}

// HallRow is one row of seats in a CreateHallRequest. Type is standard when empty.
type HallRow struct {
	Label string `json:"label"`
	Seats int    `json:"seats"`
	Type  string `json:"type"`
}

// NewHall maps the request to the domain input.
func (r CreateHallRequest) NewHall() domain.NewHall {
	rows := make([]domain.HallRow, len(r.Rows))
	for i, row := range r.Rows {
		rows[i] = domain.HallRow{Label: row.Label, Seats: row.Seats, Type: domain.SeatType(row.Type)}
	}
	return domain.NewHall{Name: r.Name, Rows: rows}
}

// Hall is a hall with its seats, in seat map order.
type Hall struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	SeatsTotal int        `json:"seats_total"`
	Seats      []HallSeat `json:"seats"`
}

// HallSeat is a physical seat of a hall. Its id is the seat id of every showtime in the hall.
type HallSeat struct {
	ID     int64  `json:"id"`
	Row    string `json:"row"`
	Number int    `json:"number"`
	Type   string `json:"type"`
}

// NewHall maps a hall with its seats.
func NewHall(h domain.HallLayout) Hall {
	seats := make([]HallSeat, len(h.Seats))
	for i, s := range h.Seats {
		seats[i] = HallSeat{ID: s.ID, Row: s.Row, Number: s.Number, Type: string(s.Type)}
	}
	return Hall{ID: h.ID, Name: h.Name, SeatsTotal: len(seats), Seats: seats}
}

// CreateShowtimeRequest is the body of POST /v1/admin/showtimes. StartsAt is an RFC 3339 date-time with a time
// zone offset, parsed by the handler so that a malformed value is reported as a field error.
type CreateShowtimeRequest struct {
	MovieID        int64  `json:"movie_id"`
	HallID         int64  `json:"hall_id"`
	StartsAt       string `json:"starts_at"`
	BasePriceCents int64  `json:"base_price_cents"`
}

// NewShowtime maps the request to the domain input, with the start time the handler parsed.
func (r CreateShowtimeRequest) NewShowtime(startsAt time.Time) domain.NewShowtime {
	return domain.NewShowtime{
		MovieID:        r.MovieID,
		HallID:         r.HallID,
		StartsAt:       startsAt,
		BasePriceCents: r.BasePriceCents,
	}
}
