package booking

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
)

type gateHold struct {
	token uuid.UUID
	until time.Time // zero while only claimed
}

// fakeGate is an in-memory HoldGate that never expires anything by itself. A non-nil err makes every call fail.
type fakeGate struct {
	mu    sync.Mutex
	holds map[seatKey]gateHold
	calls []string
	err   error
}

func newFakeGate() *fakeGate { return &fakeGate{holds: map[seatKey]gateHold{}} }

func (g *fakeGate) Acquire(_ context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID, ttl time.Duration) ([]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("acquire %v for %s", seatIDs, ttl))
	if g.err != nil {
		return nil, g.err
	}
	var conflicts []int64
	for _, id := range seatIDs {
		if h, ok := g.holds[seatKey{showtimeID, id}]; ok && h.token != token {
			conflicts = append(conflicts, id)
		}
	}
	if len(conflicts) == 0 {
		for _, id := range seatIDs {
			g.holds[seatKey{showtimeID, id}] = gateHold{token: token}
		}
	}
	return conflicts, nil
}

func (g *fakeGate) ExtendUntil(_ context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID, until time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("extend %v", seatIDs))
	if g.err != nil {
		return g.err
	}
	for _, id := range seatIDs {
		if h, ok := g.holds[seatKey{showtimeID, id}]; ok && h.token == token {
			g.holds[seatKey{showtimeID, id}] = gateHold{token: token, until: until}
		}
	}
	return nil
}

func (g *fakeGate) Release(_ context.Context, showtimeID int64, seatIDs []int64, token uuid.UUID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("release %v", seatIDs))
	if g.err != nil {
		return g.err
	}
	for _, id := range seatIDs {
		if h, ok := g.holds[seatKey{showtimeID, id}]; ok && h.token == token {
			delete(g.holds, seatKey{showtimeID, id})
		}
	}
	return nil
}

func (g *fakeGate) hold(showtimeID, seatID int64) (gateHold, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.holds[seatKey{showtimeID, seatID}]
	return h, ok
}

func (g *fakeGate) callLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// fakeSeatMaps records the showtimes whose seat maps were invalidated, one entry per call.
type fakeSeatMaps struct {
	mu          sync.Mutex
	invalidated [][]int64
}

func (c *fakeSeatMaps) InvalidateSeatMaps(_ context.Context, showtimeIDs ...int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidated = append(c.invalidated, slices.Sorted(slices.Values(showtimeIDs)))
	return nil
}

func (c *fakeSeatMaps) calls() [][]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.invalidated)
}

// newGatedTestService is newPayTestService with a hold gate and a seat map cache.
func newGatedTestService(t *testing.T) (*Service, *memDB, *fakeProvider, *fakeGate, *fakeSeatMaps) {
	t.Helper()
	gate, seatMaps := newFakeGate(), &fakeSeatMaps{}
	svc, db, provider := newPayTestService(t, WithHoldGate(gate), WithSeatMapCache(seatMaps))
	return svc, db, provider, gate, seatMaps
}

func TestCreateClaimsSeatsInTheGate(t *testing.T) {
	t.Parallel()
	svc, _, _, gate, seatMaps := newGatedTestService(t)

	b := book(t, svc, ann, 2, 1)

	for _, id := range []int64{1, 2} {
		if h, ok := gate.hold(1, id); !ok || h.token != b.ID || !h.until.Equal(b.ExpiresAt) {
			t.Errorf("gate hold of seat %d = %+v, want the booking's until its deadline %s", id, h, b.ExpiresAt)
		}
	}
	if want := []string{"acquire [1 2] for 15s", "extend [1 2]"}; !slices.Equal(gate.callLog(), want) {
		t.Errorf("gate calls = %q, want %q", gate.callLog(), want)
	}
	if got := seatMaps.calls(); !slices.EqualFunc(got, [][]int64{{1}}, slices.Equal) {
		t.Errorf("invalidated seat maps = %v, want showtime 1 once", got)
	}
}

func TestCreateTurnedAwayByTheGateSkipsTheDatabase(t *testing.T) {
	t.Parallel()
	svc, db, _, gate, seatMaps := newGatedTestService(t)
	other := uuid.NewV7()
	if _, err := gate.Acquire(t.Context(), 1, []int64{2, 3}, other, time.Minute); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{3, 1, 2}})

	var taken *domain.SeatsUnavailableError
	if !errors.As(err, &taken) || !slices.Equal(taken.SeatIDs, []int64{2, 3}) {
		t.Fatalf("Create = %v, want SEAT_UNAVAILABLE for seats [2 3]", err)
	}
	if calls := db.lastCalls(); len(calls) != 0 {
		t.Errorf("the database was called: %q", calls)
	}
	if h, ok := gate.hold(1, 1); ok {
		t.Errorf("seat 1 is claimed by %s; a rejected request must claim nothing", h.token)
	}
	if got := seatMaps.calls(); len(got) != 0 {
		t.Errorf("invalidated seat maps = %v, want none", got)
	}
}

func TestCreateReleasesTheClaimWhenTheTransactionFails(t *testing.T) {
	t.Parallel()
	svc, db, _, gate, seatMaps := newGatedTestService(t)
	book(t, svc, ann, 1)

	// Seat 1 is held in the database. Pretend the gate lost its key, so the request reaches the database.
	first, _ := gate.hold(1, 1)
	if err := gate.Release(t.Context(), 1, []int64{1}, first.token); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(t.Context(), bob, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2}})

	if code(err) != domain.CodeSeatUnavailable {
		t.Fatalf("Create = %v, want SEAT_UNAVAILABLE from the database", err)
	}
	if len(db.lastCalls()) == 0 {
		t.Error("the database was not asked")
	}
	for _, id := range []int64{1, 2} {
		if h, ok := gate.hold(1, id); ok {
			t.Errorf("seat %d is still claimed by %s after the transaction failed", id, h.token)
		}
	}
	if got := gate.callLog(); got[len(got)-1] != "release [1 2]" {
		t.Errorf("last gate call = %q, want the release of the failed claim", got[len(got)-1])
	}
	if got := seatMaps.calls(); len(got) != 1 {
		t.Errorf("invalidated seat maps = %v, want only the first booking's", got)
	}
}

func TestCreateCarriesOnWhenTheGateFails(t *testing.T) {
	t.Parallel()
	svc, db, _, gate, _ := newGatedTestService(t)
	gate.err = errors.New("redis down")

	b := book(t, svc, ann, 1, 2)

	assertBooking(t, db, b, domain.BookingPending, domain.SeatHeld)
	if want := []string{"acquire [1 2] for 15s"}; !slices.Equal(gate.callLog(), want) {
		t.Errorf("gate calls = %q, want only the failed claim: nothing to extend or release", gate.callLog())
	}
}

func TestCancelReleasesTheGateClaim(t *testing.T) {
	t.Parallel()
	svc, _, _, gate, seatMaps := newGatedTestService(t)
	b := book(t, svc, ann, 1, 2)

	for range 2 { // the second cancel changes nothing
		if err := svc.Cancel(t.Context(), ann, b.ID); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []int64{1, 2} {
		if h, ok := gate.hold(1, id); ok {
			t.Errorf("seat %d is still claimed by %s after the cancel", id, h.token)
		}
	}
	if got := gate.callLog(); len(got) != 3 || got[2] != "release [1 2]" {
		t.Errorf("gate calls = %q, want one release after the claim and extend", got)
	}
	if got := seatMaps.calls(); !slices.EqualFunc(got, [][]int64{{1}, {1}}, slices.Equal) {
		t.Errorf("invalidated seat maps = %v, want showtime 1 for the booking and the first cancel", got)
	}
	// Another user can take the seats at once, through the gate.
	book(t, svc, bob, 1, 2)
}

func TestExpireBatchInvalidatesEachShowtimeOnce(t *testing.T) {
	t.Parallel()
	svc, db, _, _, seatMaps := newGatedTestService(t)
	db.addShowtime(domain.ShowtimeRef{ID: 4, Status: domain.ShowtimeScheduled}, false, 30, 1000)
	book(t, svc, ann, 1)
	book(t, svc, bob, 2)
	if _, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 4, SeatIDs: []int64{30}}); err != nil {
		t.Fatal(err)
	}
	db.advance(holdTTL)

	batch, err := svc.ExpireBatch(t.Context(), 10)
	if err != nil || len(batch.BookingIDs) != 3 {
		t.Fatalf("ExpireBatch = %+v, %v; want 3 bookings", batch, err)
	}
	got := seatMaps.calls()
	if last := got[len(got)-1]; !slices.Equal(last, []int64{1, 4}) {
		t.Errorf("last invalidation = %v, want showtimes [1 4] once each", last)
	}
}

func TestExpireBatchReleasesTheGateClaims(t *testing.T) {
	t.Parallel()
	svc, db, _, gate, _ := newGatedTestService(t)
	a := book(t, svc, ann, 1, 2)
	b := book(t, svc, bob, 3)
	db.advance(holdTTL)

	if _, err := svc.ExpireBatch(t.Context(), 10); err != nil {
		t.Fatal(err)
	}

	for _, seat := range []int64{1, 2, 3} {
		if h, ok := gate.hold(1, seat); ok {
			t.Errorf("seat %d is still claimed by %s after its booking expired", seat, h.token)
		}
	}
	calls := gate.callLog()
	released := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "release") })
	slices.Sort(released)
	if want := []string{"release [1 2]", "release [3]"}; !slices.Equal(released, want) {
		t.Errorf("releases = %q, want one per booking, each on its own seats (%s, %s)", released, a.ID, b.ID)
	}
}

func TestPayKeepsSoldSeatsClaimedUntilTheShowtime(t *testing.T) {
	t.Parallel()
	svc, _, _, gate, seatMaps := newGatedTestService(t)
	b := book(t, svc, ann, 1)

	res, err := svc.Pay(t.Context(), ann, b.ID, card)
	if err != nil {
		t.Fatal(err)
	}

	if h, ok := gate.hold(1, 1); !ok || h.token != b.ID || !h.until.Equal(res.Booking.Showtime.StartsAt) {
		t.Errorf("gate hold = %+v, want the booking's until the showtime starts at %s", h, res.Booking.Showtime.StartsAt)
	}
	if got := seatMaps.calls(); len(got) != 2 {
		t.Errorf("invalidated seat maps = %v, want showtime 1 for the booking and the sale", got)
	}
}

func TestSettleInvalidatesOnlyWhenSeatsChange(t *testing.T) {
	t.Parallel()
	svc, db, provider, gate, seatMaps := newGatedTestService(t)
	b := book(t, svc, ann, 1)

	provider.onPay(declined("card_declined"))
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); code(err) != domain.CodePaymentDeclined {
		t.Fatalf("Pay = %v, want a decline", err)
	}
	if got := seatMaps.calls(); len(got) != 1 {
		t.Errorf("invalidated seat maps after a decline = %v; the seats did not change", got)
	}

	decline := declined("card_declined")
	provider.onPay(func(ctx context.Context, req payment.PayRequest) (payment.Charge, error) {
		db.advance(holdTTL)
		return decline(ctx, req)
	})
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); code(err) != domain.CodePaymentDeclined {
		t.Fatalf("Pay = %v, want a decline", err)
	}
	assertBooking(t, db, b, domain.BookingExpired, domain.SeatAvailable)
	if got := seatMaps.calls(); len(got) != 2 {
		t.Errorf("invalidated seat maps = %v, want a second one for the seats released at expiry", got)
	}
	if h, ok := gate.hold(1, 1); ok {
		t.Errorf("seat 1 is still claimed by %s after its booking expired", h.token)
	}
}
