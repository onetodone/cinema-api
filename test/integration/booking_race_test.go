//go:build integration

package integration

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
)

// The race tests do not call t.Parallel: they run alone, so their larger connection pool does not compete
// with other tests for the server's connection limit, and nothing else skews their timing.

// racePoolSize is the connection pool of the race tests. More connections than CPUs means that many
// transactions really wait on the same row lock at once, instead of queueing for a connection.
const racePoolSize = 40

// raceLockTimeout is long enough that no request under test gives up with SEAT_BUSY, even with -race.
const raceLockTimeout = 10 * time.Second

// stressDeadline bounds TestBookingOverlappingMultiSeat, which takes about a second. If the lock order ever
// breaks, deadlocks make it crawl; the deadline turns that into a clear failure instead of a test timeout.
const stressDeadline = 30 * time.Second

func newRacePool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.MaxConns = racePoolSize
	racePool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(racePool.Close) // runs before the database is dropped
	return racePool
}

// TestBookingRaceOneSeat is the core guarantee: 200 users try to book the same seat at the same moment, and
// exactly one of them gets it.
func TestBookingRaceOneSeat(t *testing.T) {
	f := newFixture(t)
	env := newBookingEnvOn(t, f, newRacePool(t, f.pool), raceLockTimeout)
	const contenders = 200
	users := newUsers(t, env.pool, contenders)
	seat := env.seats["A1"]

	var (
		start sync.WaitGroup
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  []domain.Booking
		lost  int
		other []string
	)
	start.Add(1)
	for _, user := range users {
		wg.Go(func() {
			start.Wait()
			b, err := env.svc.Create(t.Context(), user, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{seat}})

			mu.Lock()
			defer mu.Unlock()
			var taken *domain.SeatsUnavailableError
			switch {
			case err == nil:
				wins = append(wins, b)
			case errors.As(err, &taken) && slices.Equal(taken.SeatIDs, []int64{seat}):
				lost++
			default:
				other = append(other, describe(err))
			}
		})
	}
	began := time.Now()
	start.Done()
	wg.Wait()
	t.Logf("%d concurrent bookings of one seat finished in %s", contenders, time.Since(began))

	if len(wins) != 1 || lost != contenders-1 || len(other) != 0 {
		t.Fatalf("%d won, %d got SEAT_UNAVAILABLE, other errors %v; want 1 and %d and none",
			len(wins), lost, other, contenders-1)
	}

	winner := wins[0]
	if s := seatStates(t, env.pool, env.st.ID)[seat]; s.status != domain.SeatHeld || s.bookingID == nil || *s.bookingID != winner.ID {
		t.Errorf("seat = %+v, want held by the winner %s", s, winner.ID)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM bookings`); n != 1 {
		t.Errorf("%d bookings stored, want 1", n)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM booking_seats WHERE seat_id = $1`, seat); n != 1 {
		t.Errorf("seat appears in %d bookings, want 1", n)
	}
	if retries := env.logs.retried(); len(retries) != 0 {
		t.Errorf("transactions were retried %v; a single-seat race cannot deadlock", retries)
	}
}

// TestBookingOverlappingMultiSeat lets many users book and cancel overlapping multi-seat sets, each listed in
// random order, on one showtime. Without the lock order, two bookings could each lock a seat the other one
// needs and deadlock; with it, no transaction is ever aborted, and no seat ends up in two bookings.
func TestBookingOverlappingMultiSeat(t *testing.T) {
	f := newFixture(t)
	env := newBookingEnvOn(t, f, newRacePool(t, f.pool), raceLockTimeout)
	ctx, cancel := context.WithTimeout(t.Context(), stressDeadline)
	defer cancel()

	// A 20-seat hall: few seats for many users, so almost every request overlaps with others.
	raceHall := must(f.catalog.CreateHall(ctx, "Race hall", []domain.HallRow{
		{Label: "A", Seats: 10, Type: domain.SeatStandard},
		{Label: "B", Seats: 10, Type: domain.SeatVIP},
	}))(t)
	st := f.showtime(t, f.short, raceHall, base)
	var allSeats []int64
	for _, id := range seatIDsByLabel(t, f.pool, raceHall.ID) {
		allSeats = append(allSeats, id)
	}

	const (
		workers    = 40
		iterations = 25
	)
	users := newUsers(t, env.pool, workers)
	seed := uint64(time.Now().UnixNano())
	t.Logf("random seed %d", seed)

	var (
		start     sync.WaitGroup
		wg        sync.WaitGroup
		mu        sync.Mutex
		created   int
		conflicts int
		failures  []string
	)
	fail := func(format string) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, format)
	}

	start.Add(1)
	for w, user := range users {
		rng := rand.New(rand.NewPCG(seed, uint64(w)))
		wg.Go(func() {
			start.Wait()
			var active uuid.UUID // the booking this user currently holds, if any
			for i := range iterations {
				// Hold the previous booking while others try their luck, then let it go.
				if active != (uuid.UUID{}) {
					if err := env.svc.Cancel(ctx, user, active); err != nil {
						fail("cancel: " + describe(err))
					}
					active = uuid.UUID{}
				}

				want := slices.Clone(allSeats)
				rng.Shuffle(len(want), func(a, b int) { want[a], want[b] = want[b], want[a] })
				want = want[:2+rng.IntN(5)] // 2 to 6 seats, in random order

				b, err := env.svc.Create(ctx, user, domain.NewBooking{ShowtimeID: st.ID, SeatIDs: want})
				var taken *domain.SeatsUnavailableError
				switch {
				case err == nil:
					var got []int64
					for _, s := range b.Seats {
						got = append(got, s.SeatID)
					}
					slices.Sort(got)
					slices.Sort(want)
					if !slices.Equal(got, want) {
						fail("booking holds other seats than requested")
					}
					mu.Lock()
					created++
					mu.Unlock()
					if i < iterations-1 {
						active = b.ID
					}
				case errors.As(err, &taken):
					mu.Lock()
					conflicts++
					mu.Unlock()
				default:
					fail("create: " + describe(err))
				}
			}
		})
	}
	began := time.Now()
	start.Done()
	wg.Wait()
	t.Logf("%d booking attempts in %s: %d created, %d rejected with SEAT_UNAVAILABLE",
		workers*iterations, time.Since(began), created, conflicts)

	if len(failures) > 0 {
		t.Fatalf("%d unexpected errors, such as %q; %d transactions were retried after a deadlock",
			len(failures), failures[0], len(env.logs.retried()))
	}
	if created == 0 || conflicts == 0 {
		t.Errorf("created %d, conflicts %d: the test did not produce contention", created, conflicts)
	}
	if retries := env.logs.retried(); len(retries) != 0 {
		t.Errorf("transactions were retried %v; ordered locking should make deadlocks impossible", retries)
	}

	// No seat is listed by two live bookings.
	if n := countRows(t, env.pool, `
SELECT count(*) FROM (
    SELECT bs.seat_id
    FROM booking_seats bs
    JOIN bookings b ON b.id = bs.booking_id
    WHERE b.showtime_id = $1 AND b.status = 'pending'
    GROUP BY bs.seat_id
    HAVING count(*) > 1
) doubled`, st.ID); n != 0 {
		t.Errorf("%d seats belong to more than one pending booking", n)
	}
	// Every held seat is held by a pending booking that lists it.
	if n := countRows(t, env.pool, `
SELECT count(*)
FROM showtime_seats ss
LEFT JOIN bookings b ON b.id = ss.booking_id AND b.status = 'pending'
LEFT JOIN booking_seats bs ON bs.booking_id = ss.booking_id AND bs.seat_id = ss.seat_id
WHERE ss.showtime_id = $1 AND ss.status = 'held' AND (b.id IS NULL OR bs.seat_id IS NULL)`, st.ID); n != 0 {
		t.Errorf("%d held seats have no matching pending booking", n)
	}
	// Every seat listed by a pending booking is held by that booking, and canceled bookings hold nothing.
	if n := countRows(t, env.pool, `
SELECT count(*)
FROM booking_seats bs
JOIN bookings b ON b.id = bs.booking_id
JOIN showtime_seats ss ON ss.showtime_id = bs.showtime_id AND ss.seat_id = bs.seat_id
WHERE b.showtime_id = $1
  AND ((b.status = 'pending' AND ss.booking_id IS DISTINCT FROM b.id)
    OR (b.status = 'canceled' AND ss.booking_id = b.id))`, st.ID); n != 0 {
		t.Errorf("%d booking seats disagree with the inventory", n)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM bookings WHERE status = 'pending'`); n > workers {
		t.Errorf("%d pending bookings for %d users", n, workers)
	}
}
