//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/payment/local"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

const (
	testHoldTTL      = 15 * time.Minute
	testHoldClaimTTL = 15 * time.Second
)

// Payment settings of the tests. The local provider's tok_slow takes testSlowDelay, and a charge without an
// answer is given up after testPaymentTimeout.
const (
	testPaymentTimeout = time.Second
	testPaymentGrace   = time.Minute
	testSlowDelay      = 300 * time.Millisecond
)

// testProviders returns the payment providers of the tests: the local provider and a scripted one, both
// enabled, and another scripted one, "retired", that takes no new payments.
func testProviders(t *testing.T) (*payment.Registry, *scriptedProvider) {
	t.Helper()
	scripted := newScriptedProvider("scripted")
	providers := payment.NewRegistry()
	for _, err := range []error{
		providers.Register(local.New(local.Config{SlowDelay: testSlowDelay}), true),
		providers.Register(scripted, true),
		providers.Register(newScriptedProvider("retired"), false),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return providers, scripted
}

// newBookingService builds the booking service on pool the way internal/app does, with the test settings. Its
// transaction retries count in m.
func newBookingService(pool *pgxpool.Pool, lockTimeout time.Duration, providers *payment.Registry, m *metrics.Metrics,
	logger *slog.Logger, opts ...booking.Option,
) *booking.Service {
	return booking.New(postgres.NewUnitOfWork(pool, lockTimeout, m, logger), postgres.NewBookings(pool), providers,
		booking.Config{
			Location: time.UTC, Currency: "USD", HoldTTL: testHoldTTL, MaxSeats: 10, HoldClaimTTL: testHoldClaimTTL,
			PaymentTimeout: testPaymentTimeout, PaymentGrace: testPaymentGrace,
		}, logger, opts...)
}

// withRedis wires the hold gate and the seat map cache of env into a booking service, as the API does.
func withRedis(env *redisEnv) []booking.Option {
	return []booking.Option{
		booking.WithHoldGate(env.store.HoldGate()),
		booking.WithSeatMapCache(env.store.CatalogCache(testSeatMapTTL, testScheduleTTL)),
	}
}

// bookingEnv is the fixture catalog with the booking service on top of the real repositories.
type bookingEnv struct {
	*fixture
	svc      *booking.Service
	uow      *postgres.UnitOfWork
	metrics  *metrics.Metrics // of both svc and uow
	logs     *logRecorder
	scripted *scriptedProvider // the "scripted" payment provider
	st       domain.Showtime   // Dune in Hall 1 at base
	seats    map[string]int64  // seat ids of Hall 1 by label, such as "A1" and "AA1"
}

func newBookingEnv(t *testing.T, lockTimeout time.Duration, opts ...booking.Option) *bookingEnv {
	t.Helper()
	f := newFixture(t)
	return newBookingEnvOn(t, f, f.pool, lockTimeout, opts...)
}

// newBookingEnvOn builds the booking service on pool, which must reach the fixture's database.
func newBookingEnvOn(t *testing.T, f *fixture, pool *pgxpool.Pool, lockTimeout time.Duration, opts ...booking.Option) *bookingEnv {
	t.Helper()
	logs := &logRecorder{}
	providers, scripted := testProviders(t)
	m := metrics.New(prometheus.NewRegistry())
	return &bookingEnv{
		fixture:  f,
		uow:      postgres.NewUnitOfWork(pool, lockTimeout, m, slog.New(logs)),
		metrics:  m,
		logs:     logs,
		svc:      newBookingService(pool, lockTimeout, providers, m, slog.New(logs), opts...),
		scripted: scripted,
		st:       f.showtime(t, f.dune, f.hall, base),
		seats:    seatIDsByLabel(t, f.pool, f.hall.ID),
	}
}

// seatIDsByLabel maps "A1", "B2", ... to the seat ids of a hall.
func seatIDsByLabel(t *testing.T, pool *pgxpool.Pool, hallID int64) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT row_label || seat_number, id FROM hall_seats WHERE hall_id = $1`, hallID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seats := map[string]int64{}
	for rows.Next() {
		var (
			label string
			id    int64
		)
		if err := rows.Scan(&label, &id); err != nil {
			t.Fatal(err)
		}
		seats[label] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return seats
}

// newUsers inserts n accounts and returns their ids.
func newUsers(t *testing.T, pool *pgxpool.Pool, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, n)
	emails := make([]string, n)
	for i := range ids {
		ids[i] = uuid.NewV7()
		emails[i] = ids[i].String() + "@example.com"
	}
	exec(t, pool, `
INSERT INTO users (id, email, password_hash)
SELECT id, email, 'x' FROM unnest($1::uuid[], $2::text[]) AS u (id, email)`, ids, emails)
	return ids
}

// seatState is a row of showtime_seats.
type seatState struct {
	status    domain.SeatStatus
	bookingID *uuid.UUID
}

func seatStates(t *testing.T, pool *pgxpool.Pool, showtimeID int64) map[int64]seatState {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT seat_id, status, booking_id FROM showtime_seats WHERE showtime_id = $1`, showtimeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[int64]seatState{}
	for rows.Next() {
		var (
			id int64
			s  seatState
		)
		if err := rows.Scan(&id, &s.status, &s.bookingID); err != nil {
			t.Fatal(err)
		}
		states[id] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return states
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// logRecorder is a slog.Handler that keeps the SQLSTATE of every transaction retry and every record logged at
// warn level or above.
type logRecorder struct {
	mu       sync.Mutex
	retries  []string
	warnings []string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecorder) WithGroup(string) slog.Handler            { return l }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "sqlstate" {
			l.retries = append(l.retries, a.Value.String())
		}
		line += " " + a.String()
		return true
	})
	if r.Level >= slog.LevelWarn {
		l.warnings = append(l.warnings, line)
	}
	return nil
}

func (l *logRecorder) retried() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.retries)
}

func (l *logRecorder) warned() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.warnings)
}

func TestBookingLifecycle(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	users := newUsers(t, env.pool, 2)
	ann, bob := users[0], users[1]
	a1, a2, b1 := env.seats["A1"], env.seats["A2"], env.seats["B1"]

	// Ann holds A2 and A1; the request order does not matter.
	held, err := env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{a2, a1}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if held.Status != domain.BookingPending || held.TotalCents != 2000 || len(held.Seats) != 2 ||
		held.Seats[0].SeatID != a1 || held.Showtime.Movie.Title != "Dune" || held.Showtime.Hall.Name != "Hall 1" {
		t.Errorf("booking = %+v", held)
	}
	if got := held.ExpiresAt.Sub(held.CreatedAt); got != testHoldTTL {
		t.Errorf("hold = %s, want %s from the database clock", got, testHoldTTL)
	}
	states := seatStates(t, env.pool, env.st.ID)
	for _, id := range []int64{a1, a2} {
		if s := states[id]; s.status != domain.SeatHeld || s.bookingID == nil || *s.bookingID != held.ID {
			t.Errorf("seat %d = %+v, want held by %s", id, s, held.ID)
		}
	}
	seatMap := must(env.catalog.ListShowtimeSeats(ctx, env.st.ID))(t)
	for _, s := range seatMap {
		if want := map[bool]domain.SeatStatus{true: domain.SeatHeld, false: domain.SeatAvailable}[s.SeatID == a1 || s.SeatID == a2]; s.Status != want {
			t.Errorf("seat map shows seat %d as %s, want %s", s.SeatID, s.Status, want)
		}
	}

	// Bob cannot take A1, and his other seat stays free.
	_, err = env.svc.Create(ctx, bob, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{b1, a1}})
	var taken *domain.SeatsUnavailableError
	if !errors.As(err, &taken) || !slices.Equal(taken.SeatIDs, []int64{a1}) {
		t.Fatalf("bob: %v, want A1 unavailable", err)
	}
	if s := seatStates(t, env.pool, env.st.ID)[b1]; s.status != domain.SeatAvailable {
		t.Errorf("B1 = %s after bob's failed request", s.status)
	}

	// One active booking per user and showtime.
	if _, err := env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{b1}}); domainCode(err) != domain.CodeActiveBookingExists {
		t.Errorf("ann's second booking: %v, want ACTIVE_BOOKING_EXISTS", err)
	}

	// Reads are scoped to the owner.
	got := must(env.svc.Get(ctx, ann, held.ID))(t)
	if got.ID != held.ID || len(got.Seats) != 2 || got.Seats[0].Row != "A" || !got.ExpiresAt.Equal(held.ExpiresAt) {
		t.Errorf("get = %+v", got)
	}
	if _, err := env.svc.Get(ctx, bob, held.ID); domainCode(err) != domain.CodeBookingNotFound {
		t.Errorf("bob reads ann's booking: %v", err)
	}
	if err := env.svc.Cancel(ctx, bob, held.ID); domainCode(err) != domain.CodeBookingNotFound {
		t.Errorf("bob cancels ann's booking: %v", err)
	}

	// Cancel releases the seats, keeps the booking's seat list, and is idempotent.
	if err := env.svc.Cancel(ctx, ann, held.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for _, id := range []int64{a1, a2} {
		if s := seatStates(t, env.pool, env.st.ID)[id]; s.status != domain.SeatAvailable || s.bookingID != nil {
			t.Errorf("seat %d after cancel = %+v", id, s)
		}
	}
	canceled := must(env.svc.Get(ctx, ann, held.ID))(t)
	if canceled.Status != domain.BookingCanceled || len(canceled.Seats) != 2 || !canceled.UpdatedAt.After(canceled.CreatedAt) {
		t.Errorf("canceled booking = %+v", canceled)
	}
	if err := env.svc.Cancel(ctx, ann, held.ID); err != nil {
		t.Errorf("second cancel: %v", err)
	}

	// Ann can book again; her list is newest first.
	again := must(env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{a1}}))(t)
	page := must(env.svc.List(ctx, ann, uuid.UUID{}, 1))(t)
	if len(page.Bookings) != 1 || page.Bookings[0].ID != again.ID || page.NextBeforeID != again.ID {
		t.Fatalf("page 1 = %+v", page)
	}
	page = must(env.svc.List(ctx, ann, page.NextBeforeID, 1))(t)
	if len(page.Bookings) != 1 || page.Bookings[0].ID != held.ID || len(page.Bookings[0].Seats) != 2 || page.NextBeforeID != (uuid.UUID{}) {
		t.Errorf("page 2 = %+v", page)
	}
	if page := must(env.svc.List(ctx, bob, uuid.UUID{}, 10))(t); len(page.Bookings) != 0 {
		t.Errorf("bob's list = %+v", page.Bookings)
	}

	// A paid booking cannot be canceled.
	exec(t, env.pool, `UPDATE bookings SET status = 'paid', paid_at = now() WHERE id = $1`, again.ID)
	exec(t, env.pool, `UPDATE showtime_seats SET status = 'sold' WHERE booking_id = $1`, again.ID)
	if err := env.svc.Cancel(ctx, ann, again.ID); domainCode(err) != domain.CodeBookingNotCancelable {
		t.Errorf("cancel of a paid booking: %v", err)
	}
	if paid := must(env.svc.Get(ctx, ann, again.ID))(t); paid.PaidAt.IsZero() {
		t.Error("paid_at is not read")
	}
}

func TestBookingRejectsWhatCannotBeBooked(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	user := newUsers(t, env.pool, 1)[0]

	started := env.showtime(t, env.short, env.hall2, time.Now().Add(-time.Hour))
	canceled := env.showtime(t, env.short, env.hall2, base.Add(24*time.Hour))
	exec(t, env.pool, `UPDATE showtimes SET status = 'canceled' WHERE id = $1`, canceled.ID)
	otherHallSeat := seatIDsByLabel(t, env.pool, env.hall2.ID)["A1"]

	tests := []struct {
		name string
		user uuid.UUID
		in   domain.NewBooking
		code string
	}{
		{name: "unknown showtime", user: user, in: domain.NewBooking{ShowtimeID: 999_999, SeatIDs: []int64{1}}, code: domain.CodeShowtimeNotFound},
		{name: "started showtime", user: user, in: domain.NewBooking{ShowtimeID: started.ID, SeatIDs: []int64{otherHallSeat}}, code: domain.CodeShowtimeNotBookable},
		{name: "canceled showtime", user: user, in: domain.NewBooking{ShowtimeID: canceled.ID, SeatIDs: []int64{otherHallSeat}}, code: domain.CodeShowtimeNotBookable},
		{name: "seat of another hall", user: user, in: domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"], otherHallSeat}}, code: domain.CodeUnknownSeat},
		{name: "deleted account", user: uuid.NewV7(), in: domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"]}}, code: domain.CodeInvalidToken},
	}
	for _, tt := range tests {
		_, err := env.svc.Create(ctx, tt.user, tt.in)
		if domainCode(err) != tt.code {
			t.Errorf("%s: %v, want %s", tt.name, err, tt.code)
		}
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM bookings`); n != 0 {
		t.Errorf("%d bookings left behind by rejected requests", n)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM showtime_seats WHERE status <> 'available'`); n != 0 {
		t.Errorf("%d seats changed by rejected requests", n)
	}
}

// TestBookingLockTimeoutIsBusy holds a seat lock in another transaction: the booking gives up after the lock
// timeout with SEAT_BUSY instead of waiting, and succeeds once the lock is gone.
func TestBookingLockTimeoutIsBusy(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 150*time.Millisecond)
	ctx := t.Context()
	user := newUsers(t, env.pool, 1)[0]
	a1 := env.seats["A1"]

	blocker, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx,
		`SELECT 1 FROM showtime_seats WHERE showtime_id = $1 AND seat_id = $2 FOR UPDATE`, env.st.ID, a1); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = env.svc.Create(ctx, user, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{a1}})
	elapsed := time.Since(start)
	if !errors.Is(err, domain.ErrBusy) || domainCode(err) != domain.CodeSeatBusy {
		t.Fatalf("err = %v, want SEAT_BUSY", err)
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("gave up after %s, want about the 150ms lock timeout", elapsed)
	}

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Create(ctx, user, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{a1}}); err != nil {
		t.Errorf("booking after the lock was released: %v", err)
	}
}

// TestUnitOfWorkRetriesDeadlocks provokes a real deadlock by locking two seats in opposite orders, which the
// booking code never does. PostgreSQL aborts one transaction with 40P01, and the unit of work runs it again.
func TestUnitOfWorkRetriesDeadlocks(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 10*time.Second)
	first, second := env.seats["A1"], env.seats["A2"]

	var (
		bothLocked sync.WaitGroup
		attempts   atomic.Int32
		wg         sync.WaitGroup
		errs       [2]error
	)
	bothLocked.Add(2)
	lockInOrder := func(i int, a, b int64) {
		errs[i] = env.uow.Do(t.Context(), func(ctx context.Context, r booking.TxRepos) error {
			n := attempts.Add(1)
			if _, err := r.Seats().LockOrdered(ctx, env.st.ID, []int64{a}); err != nil {
				return err
			}
			if n <= 2 { // first attempts only: make sure each holds one lock before asking for the other
				bothLocked.Done()
				bothLocked.Wait()
			}
			_, err := r.Seats().LockOrdered(ctx, env.st.ID, []int64{b})
			return err
		})
	}
	wg.Go(func() { lockInOrder(0, first, second) })
	wg.Go(func() { lockInOrder(1, second, first) })
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("transaction %d: %v", i, err)
		}
	}
	if got := env.logs.retried(); !slices.Equal(got, []string{"40P01"}) {
		t.Errorf("retries = %v, want exactly one after a deadlock", got)
	}
	if n := testCount(env.metrics.TxRetries.WithLabelValues(metrics.SQLStateDeadlock)); n != 1 {
		t.Errorf("cinema_db_tx_retries_total{sqlstate=40P01} = %v, want 1", n)
	}
	if n := attempts.Load(); n != 3 {
		t.Errorf("attempts = %d, want 3 (two first tries and one retry)", n)
	}
}

// describe summarizes an error for test failure messages.
func describe(err error) string {
	if c := domainCode(err); c != "" {
		return c
	}
	return fmt.Sprintf("%T: %v", err, err)
}
