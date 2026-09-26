package domain

import (
	"errors"
	"fmt"
	"net/url"
	"time"
	"unicode/utf8"
	"uuid"
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

// Hall limits.
const (
	MaxHallNameLength = 100
	MaxHallRows       = 50
	MaxRowLabelLength = 3
	MaxRowSeats       = 100
	// MaxHallSeats bounds the rows that scheduling one showtime of the hall inserts.
	MaxHallSeats = 1000
)

// NewHall holds the fields needed to create a hall with its seats. Each row gets seats numbered from 1.
type NewHall struct {
	Name string
	Rows []HallRow
}

// Validate reports every invalid field of h as a *ValidationError, or returns nil. Fields of a row are named
// like rows[2].seats, counting from 0.
func (h NewHall) Validate() error {
	var v Violations
	switch n := utf8.RuneCountInString(h.Name); {
	case n == 0:
		v.Add("name", "is required")
	case n > MaxHallNameLength:
		v.Add("name", "must be at most %d characters", MaxHallNameLength)
	}
	switch n := len(h.Rows); {
	case n == 0:
		v.Add("rows", "must contain at least 1 row")
	case n > MaxHallRows:
		v.Add("rows", "must contain at most %d rows", MaxHallRows)
	}

	seen := make(map[string]bool, len(h.Rows))
	total := 0
	for i, r := range h.Rows {
		field := fmt.Sprintf("rows[%d].", i)
		switch {
		case !isRowLabel(r.Label):
			v.Add(field+"label", "must be 1 to %d uppercase letters or digits", MaxRowLabelLength)
		case seen[r.Label]:
			v.Add(field+"label", "must be unique, but row %s appears more than once", r.Label)
		}
		seen[r.Label] = true
		if r.Seats < 1 || r.Seats > MaxRowSeats {
			v.Add(field+"seats", "must be between 1 and %d", MaxRowSeats)
		}
		if !r.Type.Valid() {
			v.Add(field+"type", "must be %s, %s, or %s", SeatStandard, SeatVIP, SeatAccessible)
		}
		total += r.Seats
	}
	if total > MaxHallSeats {
		v.Add("rows", "must contain at most %d seats in all, got %d", MaxHallSeats, total)
	}
	return v.Err()
}

// isRowLabel accepts short labels of uppercase letters and digits, such as A, AA, or 12. Seat maps sort rows by
// label length first, so AA follows Z and 10 follows 9.
func isRowLabel(s string) bool {
	if s == "" || len(s) > MaxRowLabelLength {
		return false
	}
	for _, c := range s {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// HallSeat is a physical seat of a hall.
type HallSeat struct {
	ID     int64
	Row    string
	Number int
	Type   SeatType
}

// HallLayout is a hall with its seats, in seat map order.
type HallLayout struct {
	Hall
	Seats []HallSeat
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

// Showtime limits.
const (
	// MaxBasePriceCents keeps every price and booking total far below the 32-bit integer columns that store them:
	// a VIP seat costs 1.5 times the base price, and a booking has at most 50 seats.
	MaxBasePriceCents = 1_000_000
	// MaxScheduleAhead is how far in the future a showtime may start.
	MaxScheduleAhead = 366 * 24 * time.Hour
)

// Validate reports every invalid field of s as a *ValidationError, or returns nil. A showtime must start after
// now and at most MaxScheduleAhead later.
func (s NewShowtime) Validate(now time.Time) error {
	var v Violations
	if s.MovieID <= 0 {
		v.Add("movie_id", "must be a positive integer")
	}
	if s.HallID <= 0 {
		v.Add("hall_id", "must be a positive integer")
	}
	switch {
	case s.StartsAt.IsZero():
		v.Add("starts_at", "is required")
	case !s.StartsAt.After(now):
		v.Add("starts_at", "must be in the future")
	case s.StartsAt.After(now.Add(MaxScheduleAhead)):
		v.Add("starts_at", "must be at most %d days ahead", int(MaxScheduleAhead/(24*time.Hour)))
	}
	if s.BasePriceCents < 0 || s.BasePriceCents > MaxBasePriceCents {
		v.Add("base_price_cents", "must be between 0 and %d", MaxBasePriceCents)
	}
	return v.Err()
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
	// BookingID is the booking that holds or bought the seat. Only the locking reads of the booking use cases
	// load it; it is zero for available seats and on seat maps.
	BookingID uuid.UUID
}

// SeatPrice returns the price of a seat of type t for a showtime with the given base price.
// The database applies the same rule when it materializes a showtime's inventory.
func SeatPrice(basePriceCents int64, t SeatType) int64 {
	if t == SeatVIP {
		return basePriceCents * (100 + VIPSurchargePercent) / 100
	}
	return basePriceCents
}
