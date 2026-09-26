//go:build integration

package integration

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// TestPayVersusExpiry is the guarantee that ties payments to holds (SPEC N3). 200 owners pay right around the
// moment their hold runs out, some just before and some just after, while four expiry workers sweep every
// 20 ms. Every booking ends in one of two states:
//   - paid: its seats sold, one succeeded payment. This includes charges that began before the deadline and
//     ended after it;
//   - expired: its seats available again, and no payment at all, because the payment was refused at once.
//
// A payment never succeeds for seats that were released, and no seat is both sold and released.
func TestPayVersusExpiry(t *testing.T) {
	f := newFixture(t)
	env := newBookingEnvOn(t, f, newRacePool(t, f.pool), raceLockTimeout)
	hall, seats := bigHall(t, f)
	st := f.showtime(t, f.short, hall, base)

	const owners = 200
	users := newUsers(t, env.pool, owners)
	requests := make([]domain.NewBooking, owners)
	for i := range requests {
		requests[i] = domain.NewBooking{ShowtimeID: st.ID, SeatIDs: seats[2*i : 2*i+2]}
	}
	bookings := createAll(t, env.svc, users, requests)

	// The deadlines follow each other every 2 ms, from 300 ms from now.
	first := time.Now().Add(300 * time.Millisecond)
	ids := make([]uuid.UUID, owners)
	deadlines := make([]time.Time, owners)
	for i, b := range bookings {
		ids[i], deadlines[i] = b.ID, first.Add(time.Duration(i)*2*time.Millisecond)
	}
	exec(t, env.pool, `
UPDATE bookings b SET expires_at = d.deadline
FROM unnest($1::uuid[], $2::timestamptz[]) AS d (id, deadline)
WHERE b.id = d.id`, ids, deadlines)

	// A charge takes 20 to 80 ms, so one that starts shortly before the deadline ends after it.
	env.scripted.script(func(_ context.Context, req payment.PayRequest) (payment.Charge, error) {
		time.Sleep(20*time.Millisecond + rand.N(60*time.Millisecond))
		return env.scripted.charge(req.PaymentID), nil
	})
	expirers, stop := startExpirers(t, env.pool, 4, 25, env.logs, false)

	results := make([]error, owners)
	settled := make([]domain.PaymentStatus, owners)
	var wg sync.WaitGroup
	for i := range bookings {
		wg.Go(func() {
			// Each owner pays within 60 ms before or after the deadline.
			time.Sleep(time.Until(deadlines[i].Add(rand.N(120*time.Millisecond) - 60*time.Millisecond)))
			var res booking.PayResult
			res, results[i] = env.svc.Pay(t.Context(), users[i], bookings[i].ID, withScripted)
			settled[i] = res.Payment.Status
		})
	}
	wg.Wait()
	waitFor(t, 10*time.Second, "the workers to expire every unpaid booking", func() bool {
		return countRows(t, env.pool, `SELECT count(*) FROM bookings WHERE status IN ('pending', 'processing')`) == 0
	})
	stop()

	type outcome struct {
		status              domain.BookingStatus
		late                bool // paid after the hold ran out
		seatsHeld, sold     int  // seats the booking holds in any status, and sold ones
		payments, succeeded int
	}
	rows, err := env.pool.Query(t.Context(), `
SELECT b.id, b.status, coalesce(b.paid_at > b.expires_at, false),
       (SELECT count(*) FROM showtime_seats ss WHERE ss.booking_id = b.id),
       (SELECT count(*) FROM showtime_seats ss WHERE ss.booking_id = b.id AND ss.status = 'sold'),
       (SELECT count(*) FROM payments p WHERE p.booking_id = b.id),
       (SELECT count(*) FROM payments p WHERE p.booking_id = b.id AND p.status = 'succeeded')
FROM bookings b WHERE b.showtime_id = $1`, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[uuid.UUID]outcome{}
	for rows.Next() {
		var (
			id uuid.UUID
			o  outcome
		)
		if err := rows.Scan(&id, &o.status, &o.late, &o.seatsHeld, &o.sold, &o.payments, &o.succeeded); err != nil {
			t.Fatal(err)
		}
		outcomes[id] = o
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	var paid, late, gone int
	for i, err := range results {
		o := outcomes[bookings[i].ID]
		switch {
		case err == nil && settled[i] != domain.PaymentSucceeded:
			t.Errorf("owner %d: the payment was left %s", i, settled[i])
		case err == nil:
			paid++
			if o.late {
				late++
			}
			if o.status != domain.BookingPaid || o.seatsHeld != 2 || o.sold != 2 || o.payments != 1 || o.succeeded != 1 {
				t.Errorf("owner %d paid, but the booking is %+v", i, o)
			}
		case domainCode(err) == domain.CodeBookingExpired:
			gone++
			if o.status != domain.BookingExpired || o.seatsHeld != 0 || o.payments != 0 {
				t.Errorf("owner %d was refused, but the booking is %+v", i, o)
			}
		default:
			t.Errorf("owner %d: %v", i, err)
		}
	}

	expired := map[uuid.UUID]int{}
	for _, e := range expirers {
		for _, id := range e.expired() {
			expired[id]++
		}
	}
	for id, n := range expired {
		if n != 1 || outcomes[id].status != domain.BookingExpired {
			t.Errorf("booking %s was expired %d times and is %s", id, n, outcomes[id].status)
		}
	}
	if len(expired) != gone {
		t.Errorf("the workers expired %d bookings, want the %d refused ones", len(expired), gone)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM showtime_seats WHERE showtime_id = $1 AND status = 'sold'`, st.ID); n != 2*paid {
		t.Errorf("%d seats sold, want %d", n, 2*paid)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM showtime_seats WHERE showtime_id = $1 AND status = 'held'`, st.ID); n != 0 {
		t.Errorf("%d seats still held", n)
	}
	if retries, warnings := env.logs.retried(), env.logs.warned(); len(retries) != 0 || len(warnings) != 0 {
		t.Errorf("retries %v, warnings %v; want none", retries, warnings)
	}

	t.Logf("%d paid (%d of them after the hold ran out), %d refused and expired", paid, late, gone)
	// The test only proves something if both sides of the race happened.
	if paid == 0 || late == 0 || gone == 0 {
		t.Errorf("the race was not exercised: %d paid, %d late, %d refused", paid, late, gone)
	}
}
