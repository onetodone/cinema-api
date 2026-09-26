// Package seed fills an empty database with demo movies, halls, and a week of showtimes.
package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Store is the write access the seeder needs. It is implemented by repository/postgres.Catalog.
type Store interface {
	CreateMovie(ctx context.Context, m domain.NewMovie) (domain.Movie, error)
	CreateHall(ctx context.Context, name string, rows []domain.HallRow) (domain.HallLayout, error)
	CreateShowtime(ctx context.Context, s domain.NewShowtime) (domain.Showtime, error)
}

// Options controls how much data is generated.
type Options struct {
	Days     int            // number of days to schedule, starting with FirstDay
	FirstDay time.Time      // only year, month, and day are used
	Location *time.Location // the cinema's time zone
}

// Stats reports what was created.
type Stats struct {
	Movies    int
	Halls     int
	Seats     int
	Showtimes int
}

// Schedule rules for generated showtimes, in cinema-local time.
const (
	firstShowHour    = 10
	lastStartHour    = 22
	lastStartMinute  = 30
	startGranule     = 15 * time.Minute // starts are aligned to quarter hours
	eveningHour      = 17
	matineePrice     = 900
	eveningPrice     = 1300
	weekendSurcharge = 200
)

var movies = []domain.NewMovie{
	{
		Title:       "The Last Projectionist",
		Description: "A small-town projectionist fights to keep the last film cinema in the county alive.",
		DurationMin: 118, AgeRating: "PG-13",
	},
	{
		Title:       "Midnight Matinee",
		Description: "A horror comedy about a late-night screening that refuses to end.",
		DurationMin: 96, AgeRating: "R",
	},
	{
		Title:       "Orbit of Glass",
		Description: "The crew of a fragile research station must choose between rescue and discovery.",
		DurationMin: 142, AgeRating: "PG-13",
	},
	{
		Title:       "Paper Lanterns",
		Description: "An animated journey of two siblings who follow a lantern across a sleeping city.",
		DurationMin: 104, AgeRating: "PG",
	},
	{
		Title:       "The Quiet Heist",
		Description: "Four retired engineers plan a robbery that must not make a sound.",
		DurationMin: 127, AgeRating: "R",
	},
	{
		Title:       "Salt and Thunder",
		Description: "A sailing race around a storm-bound archipelago turns into a rescue mission.",
		DurationMin: 133, AgeRating: "PG-13",
	},
}

type hallSpec struct {
	name string
	rows []domain.HallRow
}

var halls = []hallSpec{
	{name: "Hall 1", rows: rows(
		rowSpec{labels: "A", seats: 12, typ: domain.SeatAccessible},
		rowSpec{labels: "BCDEFGH", seats: 14, typ: domain.SeatStandard},
		rowSpec{labels: "IJ", seats: 14, typ: domain.SeatVIP},
	)},
	{name: "Hall 2", rows: rows(
		rowSpec{labels: "ABCDEFG", seats: 12, typ: domain.SeatStandard},
		rowSpec{labels: "H", seats: 12, typ: domain.SeatVIP},
	)},
	{name: "Hall 3", rows: rows(
		rowSpec{labels: "ABCDE", seats: 8, typ: domain.SeatVIP},
	)},
}

type rowSpec struct {
	labels string // one row per letter
	seats  int
	typ    domain.SeatType
}

func rows(specs ...rowSpec) []domain.HallRow {
	var out []domain.HallRow
	for _, s := range specs {
		for _, label := range s.labels {
			out = append(out, domain.HallRow{Label: string(label), Seats: s.seats, Type: s.typ})
		}
	}
	return out
}

// Run creates the demo catalog through store.
func Run(ctx context.Context, store Store, opts Options) (Stats, error) {
	var stats Stats

	created := make([]domain.Movie, 0, len(movies))
	for _, nm := range movies {
		m, err := store.CreateMovie(ctx, nm)
		if err != nil {
			return stats, fmt.Errorf("seed movies: %w", err)
		}
		created = append(created, m)
		stats.Movies++
	}

	hallIDs := make([]int64, 0, len(halls))
	for _, h := range halls {
		hall, err := store.CreateHall(ctx, h.name, h.rows)
		if err != nil {
			return stats, fmt.Errorf("seed halls: %w", err)
		}
		hallIDs = append(hallIDs, hall.ID)
		stats.Halls++
		for _, r := range h.rows {
			stats.Seats += r.Seats
		}
	}

	first := time.Date(opts.FirstDay.Year(), opts.FirstDay.Month(), opts.FirstDay.Day(), 0, 0, 0, 0, opts.Location)
	for d := range opts.Days {
		day := first.AddDate(0, 0, d)
		for hallIdx, hallID := range hallIDs {
			for _, slot := range PlanDay(day, hallIdx+d, created) {
				_, err := store.CreateShowtime(ctx, domain.NewShowtime{
					MovieID:        slot.Movie.ID,
					HallID:         hallID,
					StartsAt:       slot.StartsAt,
					BasePriceCents: slot.BasePriceCents,
				})
				if err != nil {
					return stats, fmt.Errorf("seed showtimes: %w", err)
				}
				stats.Showtimes++
			}
		}
	}

	return stats, nil
}

// Slot is one planned showtime.
type Slot struct {
	Movie          domain.Movie
	StartsAt       time.Time
	EndsAt         time.Time
	BasePriceCents int64
}

// PlanDay plans back-to-back showtimes for one hall on one local day. offset rotates the movies so that halls
// and days do not all show the same film at the same time. day must be local midnight in the cinema time zone.
func PlanDay(day time.Time, offset int, movieList []domain.Movie) []Slot {
	if len(movieList) == 0 {
		return nil
	}

	lastStart := time.Date(day.Year(), day.Month(), day.Day(), lastStartHour, lastStartMinute, 0, 0, day.Location())
	start := time.Date(day.Year(), day.Month(), day.Day(), firstShowHour, 0, 0, 0, day.Location())

	var slots []Slot
	for i := 0; !start.After(lastStart); i++ {
		m := movieList[(offset+i)%len(movieList)]
		end := start.Add(time.Duration(m.DurationMin)*time.Minute + domain.CleaningBuffer)
		slots = append(slots, Slot{Movie: m, StartsAt: start, EndsAt: end, BasePriceCents: price(start)})
		start = roundUp(end, startGranule)
	}
	return slots
}

func price(start time.Time) int64 {
	p := int64(matineePrice)
	if start.Hour() >= eveningHour {
		p = eveningPrice
	}
	switch start.Weekday() {
	case time.Friday, time.Saturday, time.Sunday:
		p += weekendSurcharge
	}
	return p
}

// roundUp rounds t up to the next multiple of d in t's own time zone.
func roundUp(t time.Time, d time.Duration) time.Time {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	elapsed := t.Sub(midnight)
	if rem := elapsed % d; rem != 0 {
		return t.Add(d - rem)
	}
	return t
}
