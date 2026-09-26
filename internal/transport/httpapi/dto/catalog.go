// Package dto defines the JSON shapes of API requests and responses and maps them from domain types.
package dto

import (
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/catalog"
)

// Movie is a movie in a list or on its own page.
type Movie struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	DurationMin int    `json:"duration_min"`
	AgeRating   string `json:"age_rating,omitempty"`
	PosterURL   string `json:"poster_url,omitempty"`
}

// MovieList is one page of movies. NextCursor is omitted on the last page.
type MovieList struct {
	Items      []Movie `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// MovieDetails is a movie with its upcoming showtimes.
type MovieDetails struct {
	Movie
	UpcomingShowtimes []Showtime `json:"upcoming_showtimes"`
}

// MovieRef is the part of a movie shown with a showtime.
type MovieRef struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	DurationMin int    `json:"duration_min"`
	AgeRating   string `json:"age_rating,omitempty"`
}

// HallRef identifies a hall.
type HallRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Showtime is one screening with its availability.
type Showtime struct {
	ID             int64     `json:"id"`
	Movie          MovieRef  `json:"movie"`
	Hall           HallRef   `json:"hall"`
	StartsAt       time.Time `json:"starts_at"`
	EndsAt         time.Time `json:"ends_at"`
	Status         string    `json:"status"`
	BasePriceCents int64     `json:"base_price_cents"`
	Currency       string    `json:"currency"`
	SeatsAvailable int       `json:"seats_available"`
	SeatsTotal     int       `json:"seats_total"`
}

// Schedule lists the showtimes of one local calendar day.
type Schedule struct {
	Date  string     `json:"date"`
	Items []Showtime `json:"items"`
}

// SeatSummary counts seats by status.
type SeatSummary struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Sold      int `json:"sold"`
	Total     int `json:"total"`
}

// Seat is one seat of a showtime.
type Seat struct {
	ID         int64  `json:"id"`
	Row        string `json:"row"`
	Number     int    `json:"number"`
	Type       string `json:"type"`
	PriceCents int64  `json:"price_cents"`
	Status     string `json:"status"`
}

// SeatMap is the seat layout of a showtime.
type SeatMap struct {
	ShowtimeID int64       `json:"showtime_id"`
	Movie      MovieRef    `json:"movie"`
	Hall       HallRef     `json:"hall"`
	StartsAt   time.Time   `json:"starts_at"`
	Currency   string      `json:"currency"`
	Summary    SeatSummary `json:"summary"`
	Seats      []Seat      `json:"seats"`
}

// NewMovie maps a domain movie.
func NewMovie(m domain.Movie) Movie {
	return Movie{
		ID:          m.ID,
		Title:       m.Title,
		Description: m.Description,
		DurationMin: m.DurationMin,
		AgeRating:   m.AgeRating,
		PosterURL:   m.PosterURL,
	}
}

// NewMovieList maps a page of movies; cursor encodes the id to continue after.
func NewMovieList(page catalog.MoviePage, cursor func(afterID int64) string) MovieList {
	out := MovieList{Items: make([]Movie, 0, len(page.Movies))}
	for _, m := range page.Movies {
		out.Items = append(out.Items, NewMovie(m))
	}
	if page.NextAfterID != 0 {
		out.NextCursor = cursor(page.NextAfterID)
	}
	return out
}

// NewMovieDetails maps a movie with its upcoming showtimes.
func NewMovieDetails(d catalog.MovieDetails, currency string) MovieDetails {
	return MovieDetails{Movie: NewMovie(d.Movie), UpcomingShowtimes: NewShowtimes(d.Upcoming, currency)}
}

// NewShowtime maps a domain showtime.
func NewShowtime(s domain.Showtime, currency string) Showtime {
	return Showtime{
		ID:             s.ID,
		Movie:          newMovieRef(s.Movie),
		Hall:           HallRef{ID: s.Hall.ID, Name: s.Hall.Name},
		StartsAt:       s.StartsAt,
		EndsAt:         s.EndsAt,
		Status:         string(s.Status),
		BasePriceCents: s.BasePriceCents,
		Currency:       currency,
		SeatsAvailable: s.SeatsAvailable,
		SeatsTotal:     s.SeatsTotal,
	}
}

// NewShowtimes maps a list of showtimes. The result is never nil, so it encodes as [] rather than null.
func NewShowtimes(list []domain.Showtime, currency string) []Showtime {
	out := make([]Showtime, 0, len(list))
	for _, s := range list {
		out = append(out, NewShowtime(s, currency))
	}
	return out
}

// NewSchedule maps a day's schedule.
func NewSchedule(s catalog.Schedule, currency string) Schedule {
	return Schedule{Date: s.Day.Format(time.DateOnly), Items: NewShowtimes(s.Showtimes, currency)}
}

// NewSeatMap maps a showtime's seat map.
func NewSeatMap(sm catalog.SeatMap, currency string) SeatMap {
	seats := make([]Seat, 0, len(sm.Seats))
	for _, s := range sm.Seats {
		seats = append(seats, Seat{
			ID:         s.SeatID,
			Row:        s.Row,
			Number:     s.Number,
			Type:       string(s.Type),
			PriceCents: s.PriceCents,
			Status:     string(s.Status),
		})
	}
	return SeatMap{
		ShowtimeID: sm.Showtime.ID,
		Movie:      newMovieRef(sm.Showtime.Movie),
		Hall:       HallRef{ID: sm.Showtime.Hall.ID, Name: sm.Showtime.Hall.Name},
		StartsAt:   sm.Showtime.StartsAt,
		Currency:   currency,
		Summary: SeatSummary{
			Available: sm.Summary.Available,
			Held:      sm.Summary.Held,
			Sold:      sm.Summary.Sold,
			Total:     sm.Summary.Total,
		},
		Seats: seats,
	}
}

func newMovieRef(m domain.MovieSummary) MovieRef {
	return MovieRef{ID: m.ID, Title: m.Title, DurationMin: m.DurationMin, AgeRating: m.AgeRating}
}
