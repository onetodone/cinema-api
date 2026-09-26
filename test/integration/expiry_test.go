//go:build integration

package integration

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/worker"
)

// backdate moves the hold deadline of bookings to `ago` before now, by the database clock.
func backdate(t *testing.T, pool *pgxpool.Pool, ago time.Duration, ids ...uuid.UUID) {
	t.Helper()
	exec(t, pool, `UPDATE bookings SET expires_at = now() - $2::interval WHERE id = ANY($1::uuid[])`, ids, ago)
}

func bookingStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) domain.BookingStatus {
	t.Helper()
	var status domain.BookingStatus
	if err := pool.QueryRow(t.Context(), `SELECT status FROM bookings WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("status of booking %s: %v", id, err)
	}
	return status
}

func TestExpireBatch(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	users := newUsers(t, env.pool, 6)
	ann, bob, cat, dan, eve, fay := users[0], users[1], users[2], users[3], users[4], users[5]
	other := env.showtime(t, env.short, env.hall2, base) // a second showtime, in the other hall
	otherSeats := seatIDsByLabel(t, env.pool, env.hall2.ID)

	create := func(user uuid.UUID, showtimeID int64, seats ...int64) domain.Booking {
		t.Helper()
		return must(env.svc.Create(ctx, user, domain.NewBooking{ShowtimeID: showtimeID, SeatIDs: seats}))(t)
	}
	annB := create(ann, env.st.ID, env.seats["A2"], env.seats["A1"])
	bobB := create(bob, other.ID, otherSeats["A1"])
	catB := create(cat, env.st.ID, env.seats["B1"])
	danB := create(dan, env.st.ID, env.seats["B2"]) // not due
	eveB := create(eve, env.st.ID, env.seats["AA1"])
	fayB := create(fay, other.ID, otherSeats["A2"])
	backdate(t, env.pool, 3*time.Minute, annB.ID)
	backdate(t, env.pool, 2*time.Minute, bobB.ID)
	backdate(t, env.pool, time.Minute, catB.ID)
	// Due as well, but a payment is in progress (Sprint 5 reconciles those), or the booking is already paid.
	backdate(t, env.pool, 10*time.Minute, eveB.ID, fayB.ID)
	exec(t, env.pool, `UPDATE bookings SET status = 'processing' WHERE id = $1`, eveB.ID)
	exec(t, env.pool, `UPDATE bookings SET status = 'paid', paid_at = now() WHERE id = $1`, fayB.ID)
	exec(t, env.pool, `UPDATE showtime_seats SET status = 'sold' WHERE booking_id = $1`, fayB.ID)

	// Earliest deadline first, across showtimes.
	batch := must(env.svc.ExpireBatch(ctx, 2))(t)
	if !slices.Equal(batch.BookingIDs, []uuid.UUID{annB.ID, bobB.ID}) || batch.Seats != 3 {
		t.Errorf("first batch = %+v, want ann's and bob's bookings with 3 seats", batch)
	}
	batch = must(env.svc.ExpireBatch(ctx, 10))(t)
	if !slices.Equal(batch.BookingIDs, []uuid.UUID{catB.ID}) || batch.Seats != 1 {
		t.Errorf("second batch = %+v, want cat's booking with 1 seat", batch)
	}
	if batch := must(env.svc.ExpireBatch(ctx, 10))(t); len(batch.BookingIDs) != 0 || batch.Seats != 0 {
		t.Errorf("third batch = %+v, want nothing left", batch)
	}

	for id, want := range map[uuid.UUID]domain.BookingStatus{
		annB.ID: domain.BookingExpired, bobB.ID: domain.BookingExpired, catB.ID: domain.BookingExpired,
		danB.ID: domain.BookingPending, eveB.ID: domain.BookingProcessing, fayB.ID: domain.BookingPaid,
	} {
		if got := bookingStatus(t, env.pool, id); got != want {
			t.Errorf("booking %s is %s, want %s", id, got, want)
		}
	}
	states := seatStates(t, env.pool, env.st.ID)
	maps.Copy(states, seatStates(t, env.pool, other.ID)) // the two halls have distinct seat ids
	for label, want := range map[string]seatState{
		"A1": {status: domain.SeatAvailable}, "A2": {status: domain.SeatAvailable}, "B1": {status: domain.SeatAvailable},
		"other A1": {status: domain.SeatAvailable},
		"B2":       {status: domain.SeatHeld, bookingID: &danB.ID},
		"AA1":      {status: domain.SeatHeld, bookingID: &eveB.ID},
		"other A2": {status: domain.SeatSold, bookingID: &fayB.ID},
	} {
		id := env.seats[label]
		if rest, ok := strings.CutPrefix(label, "other "); ok {
			id = otherSeats[rest]
		}
		got := states[id]
		if got.status != want.status || (got.bookingID == nil) != (want.bookingID == nil) ||
			(want.bookingID != nil && *got.bookingID != *want.bookingID) {
			t.Errorf("seat %s = %+v, want %+v", label, got, want)
		}
	}

	// The expired booking keeps its seat list and reads as expired; canceling it changes nothing.
	expired := must(env.svc.Get(ctx, ann, annB.ID))(t)
	if expired.Status != domain.BookingExpired || len(expired.Seats) != 2 || !expired.UpdatedAt.After(expired.CreatedAt) {
		t.Errorf("expired booking = %+v", expired)
	}
	if err := env.svc.Cancel(ctx, ann, annB.ID); err != nil {
		t.Errorf("cancel of an expired booking: %v", err)
	}
	// Ann's slot for the showtime is free again, and so are her seats.
	create(ann, env.st.ID, env.seats["A1"], env.seats["A2"])
}

// TestExpireBatchSkipsLockedBookings holds the lock a user's cancel would take on a due booking. The batch
// neither waits for it nor fails: it expires the other due booking, and a later batch takes the locked one.
func TestExpireBatchSkipsLockedBookings(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 10*time.Second) // waiting for the lock would show as a 10 s batch
	ctx := t.Context()
	users := newUsers(t, env.pool, 2)
	locked := must(env.svc.Create(ctx, users[0], domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"]}}))(t)
	free := must(env.svc.Create(ctx, users[1], domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A2"]}}))(t)
	backdate(t, env.pool, 2*time.Minute, locked.ID) // the earlier deadline: first in line
	backdate(t, env.pool, time.Minute, free.ID)

	blocker, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM bookings WHERE id = $1 FOR NO KEY UPDATE`, locked.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	batch := must(env.svc.ExpireBatch(ctx, 10))(t)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the batch took %s; it waited for the lock instead of skipping the booking", elapsed)
	}
	if !slices.Equal(batch.BookingIDs, []uuid.UUID{free.ID}) {
		t.Errorf("batch = %+v, want only the unlocked booking", batch)
	}
	if s := seatStates(t, env.pool, env.st.ID)[env.seats["A1"]]; s.status != domain.SeatHeld {
		t.Errorf("seat of the locked booking = %+v, want still held", s)
	}

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if batch := must(env.svc.ExpireBatch(ctx, 10))(t); !slices.Equal(batch.BookingIDs, []uuid.UUID{locked.ID}) {
		t.Errorf("batch after the lock was released = %+v, want the formerly locked booking", batch)
	}
}

// recordingExpirer passes batches through to a booking service and keeps the ids of every booking it expired.
type recordingExpirer struct {
	svc *booking.Service
	// firstCall, if set, holds each worker's first batch until every worker has reached its first batch, so
	// that all of them compete from the start.
	firstCall *sync.WaitGroup
	once      sync.Once

	mu  sync.Mutex
	ids []uuid.UUID
}

func (r *recordingExpirer) ExpireBatch(ctx context.Context, limit int) (booking.ExpiredBatch, error) {
	if r.firstCall != nil {
		r.once.Do(func() {
			r.firstCall.Done()
			r.firstCall.Wait()
		})
	}
	batch, err := r.svc.ExpireBatch(ctx, limit)
	r.mu.Lock()
	r.ids = append(r.ids, batch.BookingIDs...)
	r.mu.Unlock()
	return batch, err
}

func (r *recordingExpirer) expired() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

// startExpirers runs n expiry workers the way n worker processes would run: each has its own connection pool,
// unit of work, and booking service. The returned function stops them and waits until they have returned.
func startExpirers(t *testing.T, pool *pgxpool.Pool, n, batchSize int, logs *logRecorder, lockstep bool) ([]*recordingExpirer, func()) {
	t.Helper()
	var firstCall *sync.WaitGroup
	if lockstep {
		firstCall = &sync.WaitGroup{}
		firstCall.Add(n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	recorders := make([]*recordingExpirer, n)
	for i := range recorders {
		p := newPoolOf(t, pool, 2)
		svc := booking.New(postgres.NewUnitOfWork(p, raceLockTimeout, slog.New(logs)), postgres.NewBookings(p),
			time.UTC, booking.Config{HoldTTL: testHoldTTL, MaxSeats: 10})
		recorders[i] = &recordingExpirer{svc: svc, firstCall: firstCall}
		e := worker.NewExpirer(recorders[i], worker.ExpirerConfig{Interval: 20 * time.Millisecond, BatchSize: batchSize},
			slog.New(logs))
		wg.Go(func() { e.Run(ctx) })
	}

	stop := func() {
		cancel()
		wg.Wait()
	}
	t.Cleanup(stop) // registered after the pools, so it runs before they close
	return recorders, stop
}

// waitFor polls cond until it holds, and fails the test if that takes longer than timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// bigHall creates a hall of 20 rows of 25 seats and returns it with its seat ids in ascending order.
func bigHall(t *testing.T, f *fixture) (domain.Hall, []int64) {
	t.Helper()
	rows := make([]domain.HallRow, 20)
	for i := range rows {
		rows[i] = domain.HallRow{Label: string(rune('A' + i)), Seats: 25, Type: domain.SeatStandard}
	}
	hall := must(f.catalog.CreateHall(t.Context(), "Big hall", rows))(t)
	seats := slices.Sorted(maps.Values(seatIDsByLabel(t, f.pool, hall.ID)))
	if len(seats) != 500 {
		t.Fatalf("big hall has %d seats, want 500", len(seats))
	}
	return hall, seats
}

// createAll runs the booking requests on 16 goroutines and returns the bookings in request order.
func createAll(t *testing.T, svc *booking.Service, users []uuid.UUID, requests []domain.NewBooking) []domain.Booking {
	t.Helper()
	out := make([]domain.Booking, len(requests))
	next := make(chan int)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for i := range next {
				b, err := svc.Create(t.Context(), users[i], requests[i])
				if err != nil {
					t.Errorf("create booking %d: %v", i, err)
					continue
				}
				out[i] = b
			}
		})
	}
	for i := range requests {
		next <- i
	}
	close(next)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	return out
}

// TestExpiryWorkersExpireEachBookingOnce is the guarantee of the expiry worker: four workers, like four
// replicas, sweep 1 000 due bookings at the same time. Every booking is expired by exactly one of them, and
// every seat becomes available again.
func TestExpiryWorkersExpireEachBookingOnce(t *testing.T) {
	f := newFixture(t)
	env := newBookingEnvOn(t, f, newRacePool(t, f.pool), raceLockTimeout)
	hall, seats := bigHall(t, f)

	// 250 users book a pair of seats in each of 4 showtimes: 1 000 bookings holding 2 000 seats.
	const (
		showtimes = 4
		pairs     = 250
	)
	users := newUsers(t, env.pool, pairs)
	var (
		owners   []uuid.UUID
		requests []domain.NewBooking
	)
	for s := range showtimes {
		st := f.showtime(t, f.short, hall, base.Add(time.Duration(s)*3*time.Hour))
		for i, user := range users {
			owners = append(owners, user)
			requests = append(requests, domain.NewBooking{ShowtimeID: st.ID, SeatIDs: []int64{seats[2*i], seats[2*i+1]}})
		}
	}
	bookings := createAll(t, env.svc, owners, requests)
	// Every hold ran out at some point in the last 10 minutes, so the deadlines differ.
	exec(t, env.pool, `UPDATE bookings SET expires_at = now() - interval '1 second' - random() * interval '10 minutes'`)

	logs := &logRecorder{}
	const workers, batchSize = 4, 25
	began := time.Now()
	recorders, stop := startExpirers(t, env.pool, workers, batchSize, logs, true)
	waitFor(t, 30*time.Second, "every booking to expire", func() bool {
		return countRows(t, env.pool, `SELECT count(*) FROM bookings WHERE status = 'pending'`) == 0
	})
	elapsed := time.Since(began)
	stop()

	seen := map[uuid.UUID]int{}
	var shares []int
	for _, r := range recorders {
		ids := r.expired()
		shares = append(shares, len(ids))
		for _, id := range ids {
			seen[id]++
		}
	}
	t.Logf("%d workers expired %d bookings in %s; shares %v", workers, len(seen), elapsed, shares)

	for id, n := range seen {
		if n != 1 {
			t.Errorf("booking %s was expired %d times", id, n)
		}
	}
	for _, b := range bookings {
		if seen[b.ID] != 1 {
			t.Errorf("booking %s was reported by %d workers, want 1", b.ID, seen[b.ID])
		}
	}
	if len(seen) != len(bookings) {
		t.Errorf("workers reported %d bookings, want %d", len(seen), len(bookings))
	}
	for i, share := range shares {
		if share < batchSize {
			t.Errorf("worker %d expired %d bookings; every worker should have taken at least its first full batch", i, share)
		}
	}
	if warnings := logs.warned(); len(warnings) != 0 {
		t.Errorf("%d warnings or errors were logged, such as %q", len(warnings), warnings[0])
	}

	if n := countRows(t, env.pool, `SELECT count(*) FROM bookings WHERE status = 'expired'`); n != len(bookings) {
		t.Errorf("%d bookings expired, want %d", n, len(bookings))
	}
	if n := countRows(t, env.pool, `
SELECT count(*) FROM showtime_seats ss JOIN showtimes s ON s.id = ss.showtime_id
WHERE s.hall_id = $1 AND (ss.status <> 'available' OR ss.booking_id IS NOT NULL)`, hall.ID); n != 0 {
		t.Errorf("%d seats are not available after the expiry", n)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM booking_seats`); n != 2*len(bookings) {
		t.Errorf("%d booking_seats rows, want the %d of the seat history", n, 2*len(bookings))
	}
}

// TestExpiryRacesUserActions runs the workers while the owners of the due bookings act on them. Half of the
// owners cancel their booking, which contends with the workers for the booking row. The other half book two
// other seats of the same showtime: that insert waits on the one-active-booking index for the worker that is
// expiring the old booking, while it holds its own seat locks. Neither interplay may deadlock or fail, and
// each old booking ends up either canceled or expired.
func TestExpiryRacesUserActions(t *testing.T) {
	f := newFixture(t)
	env := newBookingEnvOn(t, f, newRacePool(t, f.pool), raceLockTimeout)
	hall, seats := bigHall(t, f)
	st := f.showtime(t, f.short, hall, base)

	const owners = 100
	users := newUsers(t, env.pool, owners)
	requests := make([]domain.NewBooking, owners)
	for i := range requests {
		requests[i] = domain.NewBooking{ShowtimeID: st.ID, SeatIDs: []int64{seats[2*i], seats[2*i+1]}}
	}
	old := createAll(t, env.svc, users, requests)
	exec(t, env.pool, `UPDATE bookings SET expires_at = now() - interval '1 second' - random() * interval '1 minute'`)

	logs := &logRecorder{}
	var (
		start sync.WaitGroup
		wg    sync.WaitGroup
		mu    sync.Mutex
		fresh = map[uuid.UUID]domain.Booking{} // new bookings by user
	)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start.Add(1)
	for i, user := range users {
		wg.Go(func() {
			start.Wait()
			if i%2 == 0 {
				if err := env.svc.Cancel(ctx, user, old[i].ID); err != nil {
					t.Errorf("cancel: %s", describe(err))
				}
				return
			}
			// Until a worker has expired the old booking, the user still has an active one for the showtime.
			want := domain.NewBooking{ShowtimeID: st.ID, SeatIDs: []int64{seats[200+2*i], seats[201+2*i]}}
			for {
				b, err := env.svc.Create(ctx, user, want)
				if domainCode(err) == domain.CodeActiveBookingExists && ctx.Err() == nil {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				if err != nil {
					t.Errorf("create: %s", describe(err))
					return
				}
				mu.Lock()
				fresh[user] = b
				mu.Unlock()
				return
			}
		})
	}
	recorders, stop := startExpirers(t, env.pool, 4, 5, logs, false)
	start.Done()
	wg.Wait()
	waitFor(t, 30*time.Second, "the old bookings to expire", func() bool {
		return countRows(t, env.pool, `SELECT count(*) FROM bookings WHERE status = 'pending' AND expires_at <= now()`) == 0
	})
	stop()

	reported := map[uuid.UUID]int{}
	for _, r := range recorders {
		for _, id := range r.expired() {
			reported[id]++
		}
	}
	expired, canceled := 0, 0
	for i, b := range old {
		status := bookingStatus(t, env.pool, b.ID)
		switch {
		case status == domain.BookingExpired && reported[b.ID] == 1:
			expired++
		case status == domain.BookingCanceled && reported[b.ID] == 0 && i%2 == 0:
			canceled++
		default:
			t.Errorf("old booking %d is %s and was reported %d times by the workers", i, status, reported[b.ID])
		}
	}
	t.Logf("%d old bookings expired by the workers, %d canceled by their owners first; %d new bookings",
		expired, canceled, len(fresh))

	if len(fresh) != owners/2 {
		t.Errorf("%d users booked again, want %d", len(fresh), owners/2)
	}
	for user, b := range fresh {
		if b.Status != domain.BookingPending || bookingStatus(t, env.pool, b.ID) != domain.BookingPending {
			t.Errorf("new booking of %s = %s, want pending", user, b.Status)
		}
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM showtime_seats WHERE showtime_id = $1 AND status = 'held'`, st.ID); n != owners {
		t.Errorf("%d seats held, want the %d of the new bookings", n, owners)
	}
	if n := countRows(t, env.pool, `
SELECT count(*) FROM showtime_seats ss LEFT JOIN bookings b ON b.id = ss.booking_id
WHERE ss.showtime_id = $1 AND ss.status = 'held' AND b.status IS DISTINCT FROM 'pending'`, st.ID); n != 0 {
		t.Errorf("%d seats are held by bookings that are not pending", n)
	}
	if warnings := logs.warned(); len(warnings) != 0 {
		t.Errorf("%d warnings or errors were logged, such as %q", len(warnings), warnings[0])
	}
	if retries := env.logs.retried(); len(retries) != 0 {
		t.Errorf("user transactions were retried %v; the lock order should rule out deadlocks", retries)
	}
}
