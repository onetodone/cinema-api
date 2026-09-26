package domain

import (
	"errors"
	"fmt"
	"net/url"
	"time"
	"unicode/utf8"
)

// CleaningBuffer is the time a hall needs between two showtimes. It is part of a showtime's occupied range.
const CleaningBuffer = 15 * time.Minute

// VIPSurchargePercent is added to the base price for VIP seats.
const VIPSurchargePercent = 50

// Movie is a film that can be screened.
type Movie struct {
	ID          int64
	Title       string
	Description string
	DurationMin int
	AgeRating   string // empty when unknown
	PosterURL   string // empty when unknown
	CreatedAt   time.Time
}

// NewMovie holds the fields needed to create a movie.
type NewMovie struct {
	Title       string
	Description string
	DurationMin int
	AgeRating   string
	PosterURL   string
}

// Movie field limits.
const (
	MaxTitleLength       = 200
	MaxDescriptionLength = 5000
	MaxDurationMin       = 600
	MaxAgeRatingLength   = 16
	MaxPosterURLLength   = 2048
)

// Validate reports every invalid field of m as a *ValidationError, or returns nil.
func (m NewMovie) Validate() error {
	var v Violations
	switch n := utf8.RuneCountInString(m.Title); {
	case n == 0:
		v.Add("title", "is required")
	case n > MaxTitleLength:
		v.Add("title", "must be at most %d characters", MaxTitleLength)
	}
	if utf8.RuneCountInString(m.Description) > MaxDescriptionLength {
		v.Add("description", "must be at most %d characters", MaxDescriptionLength)
	}
	if m.DurationMin < 1 || m.DurationMin > MaxDurationMin {
		v.Add("duration_min", "must be between 1 and %d", MaxDurationMin)
	}
	if utf8.RuneCountInString(m.AgeRating) > MaxAgeRatingLength {
		v.Add("age_rating", "must be at most %d characters", MaxAgeRatingLength)
	}
	if m.PosterURL != "" {
		v.Check("poster_url", checkWebURL(m.PosterURL, MaxPosterURLLength))
	}
	return v.Err()
}

// checkWebURL accepts absolute http and https URLs of at most maxLen bytes.
func checkWebURL(raw string, maxLen int) error {
	if len(raw) > maxLen {
		return fmt.Errorf("must be at most %d characters", maxLen)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

// MovieSummary is the part of a movie shown next to a showtime.
type MovieSummary struct {
	ID          int64
	Title       string
	DurationMin int
	AgeRating   string
}

// Hall is a screening room.
type Hall struct {
	ID   int64
	Name string
}

// SeatType is the category of a physical seat.
type SeatType string

// Seat types.
const (
	SeatStandard   SeatType = "standard"
	SeatVIP        SeatType = "vip"
	SeatAccessible SeatType = "accessible"
)

// Valid reports whether t is a known seat type.
func (t SeatType) Valid() bool {
	switch t {
	case SeatStandard, SeatVIP, SeatAccessible:
		return true
	}
	return false
}

// HallRow describes one row of seats when a hall is created.
type HallRow struct {
	Label string
	Seats int
	Type  SeatType
}

// SeatStatus is the state of a seat for one showtime.
type SeatStatus string

// Seat statuses. The only way into SeatHeld is from SeatAvailable.
const (
	SeatAvailable SeatStatus = "available"
	SeatHeld      SeatStatus = "held"
	SeatSold      SeatStatus = "sold"
)

// ShowtimeStatus is the lifecycle state of a showtime.
type ShowtimeStatus string

// Showtime statuses.
const (
	ShowtimeScheduled ShowtimeStatus = "scheduled"
	ShowtimeCanceled  ShowtimeStatus = "canceled"
)

// Showtime is a screening of a movie in a hall, with seat availability counts.
type Showtime struct {
	ID             int64
	Movie          MovieSummary
	Hall           Hall
	StartsAt       time.Time
	EndsAt         time.Time // includes the cleaning buffer
	BasePriceCents int64
	Status         ShowtimeStatus
	SeatsAvailable int
	SeatsTotal     int
}

// NewShowtime holds the fields needed to schedule a showtime. The end time is derived from the movie duration.
type NewShowtime struct {
	MovieID        int64
	HallID         int64
	StartsAt       time.Time
	BasePriceCents int64
}

// ShowtimeFilter selects showtimes that start in [From, To).
type ShowtimeFilter struct {
	From    time.Time
	To      time.Time
	MovieID int64 // 0 means any movie
	Limit   int   // 0 means no limit
}

// ShowtimeSeat is a seat's state and price for one showtime.
type ShowtimeSeat struct {
	SeatID     int64
	Row        string
	Number     int
	Type       SeatType
	PriceCents int64
	Status     SeatStatus
}

// SeatPrice returns the price of a seat of type t for a showtime with the given base price.
// The database applies the same rule when it materializes a showtime's inventory.
func SeatPrice(basePriceCents int64, t SeatType) int64 {
	if t == SeatVIP {
		return basePriceCents * (100 + VIPSurchargePercent) / 100
	}
	return basePriceCents
}
