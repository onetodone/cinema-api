//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/payment/local"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// scriptedProvider is a payment provider whose answers a test scripts. It keeps a ledger like a real provider:
// unscripted, Pay charges and records the charge, and Status answers from the ledger.
type scriptedProvider struct {
	id string

	mu        sync.Mutex
	pay       func(ctx context.Context, req payment.PayRequest) (payment.Charge, error)
	statusErr error
	ledger    map[uuid.UUID]payment.Charge
	refunds   []payment.RefundRequest
}

func newScriptedProvider(id string) *scriptedProvider {
	return &scriptedProvider{id: id, ledger: map[uuid.UUID]payment.Charge{}}
}

func (s *scriptedProvider) ID() string   { return s.id }
func (s *scriptedProvider) Name() string { return "Scripted " + s.id }

func (s *scriptedProvider) Pay(ctx context.Context, req payment.PayRequest) (payment.Charge, error) {
	s.mu.Lock()
	pay := s.pay
	s.mu.Unlock()
	if pay != nil {
		return pay(ctx, req)
	}
	return s.charge(req.PaymentID), nil
}

// charge records a successful charge of a payment in the ledger and returns it.
func (s *scriptedProvider) charge(id uuid.UUID) payment.Charge {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := payment.Charge{Status: payment.ChargeSucceeded, Ref: "scripted_" + id.String()}
	s.ledger[id] = c
	return c
}

func (s *scriptedProvider) Refund(_ context.Context, req payment.RefundRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refunds = append(s.refunds, req)
	c := s.ledger[req.PaymentID]
	c.Status = payment.ChargeRefunded
	s.ledger[req.PaymentID] = c
	return nil
}

func (s *scriptedProvider) Status(_ context.Context, req payment.StatusRequest) (payment.Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return payment.Charge{}, s.statusErr
	}
	if c, ok := s.ledger[req.PaymentID]; ok {
		return c, nil
	}
	return payment.Charge{Status: payment.ChargeNotFound}, nil
}

func (s *scriptedProvider) script(pay func(ctx context.Context, req payment.PayRequest) (payment.Charge, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pay = pay
}

func (s *scriptedProvider) failStatus(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusErr = err
}

func (s *scriptedProvider) refunded() []payment.RefundRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.refunds)
}

func withLocal(token string) domain.NewPayment {
	return domain.NewPayment{Method: local.ID, Token: token}
}

var withScripted = domain.NewPayment{Method: "scripted", Token: "tok_any"}

// paymentsOf reads the payments of a booking from the database, oldest first.
func paymentsOf(t *testing.T, pool *pgxpool.Pool, bookingID uuid.UUID) []domain.Payment {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT id, booking_id, provider, amount_cents, status, coalesce(provider_ref, ''), coalesce(failure_reason, ''),
       created_at, updated_at
FROM payments WHERE booking_id = $1 ORDER BY id`, bookingID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Payment, error) {
		var p domain.Payment
		err := row.Scan(&p.ID, &p.BookingID, &p.Provider, &p.AmountCents, &p.Status, &p.ProviderRef, &p.FailureReason,
			&p.CreatedAt, &p.UpdatedAt)
		return p, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// backdatePayments moves the start of payments to `ago` before now, by the database clock.
func backdatePayments(t *testing.T, pool *pgxpool.Pool, ago time.Duration, ids ...uuid.UUID) {
	t.Helper()
	exec(t, pool, `UPDATE payments SET created_at = now() - $2::interval WHERE id = ANY($1::uuid[])`, ids, ago)
}

// assertSeats checks the state of seats of the env's showtime; holder nil means no booking holds them.
func assertSeats(t *testing.T, env *bookingEnv, status domain.SeatStatus, holder *uuid.UUID, labels ...string) {
	t.Helper()
	states := seatStates(t, env.pool, env.st.ID)
	for _, label := range labels {
		got := states[env.seats[label]]
		if got.status != status || (got.bookingID == nil) != (holder == nil) ||
			(holder != nil && *got.bookingID != *holder) {
			t.Errorf("seat %s = %s held by %v, want %s held by %v", label, got.status, got.bookingID, status, holder)
		}
	}
}

func TestPayLifecycle(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	users := newUsers(t, env.pool, 2)
	ann, bob := users[0], users[1]

	b := must(env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["B1"], env.seats["A1"]}}))(t)
	res, err := env.svc.Pay(ctx, ann, b.ID, withLocal(local.TokenSuccess))
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if p := res.Payment; p.Status != domain.PaymentSucceeded || p.AmountCents != b.TotalCents || p.Provider != local.ID ||
		!strings.HasPrefix(p.ProviderRef, "local_") {
		t.Errorf("payment = %+v", p)
	}
	if got := res.Booking; got.Status != domain.BookingPaid || got.PaidAt.IsZero() || got.PaidAt.Before(got.CreatedAt) ||
		len(got.Seats) != 2 || got.Showtime.Movie.Title != "Dune" {
		t.Errorf("booking = %+v", got)
	}
	assertSeats(t, env, domain.SeatSold, &b.ID, "A1", "B1")
	if stored := paymentsOf(t, env.pool, b.ID); len(stored) != 1 || stored[0].ID != res.Payment.ID ||
		stored[0].Status != domain.PaymentSucceeded || stored[0].ProviderRef != res.Payment.ProviderRef {
		t.Errorf("stored payments = %+v", stored)
	}

	// Sold for good: no cancel, no second payment, no expiry, and nobody else sees the booking.
	if err := env.svc.Cancel(ctx, ann, b.ID); domainCode(err) != domain.CodeBookingNotCancelable {
		t.Errorf("Cancel = %v", err)
	}
	if _, err := env.svc.Pay(ctx, ann, b.ID, withLocal(local.TokenSuccess)); domainCode(err) != domain.CodeBookingAlreadyPaid {
		t.Errorf("second Pay = %v", err)
	}
	if _, err := env.svc.Pay(ctx, bob, b.ID, withLocal(local.TokenSuccess)); domainCode(err) != domain.CodeBookingNotFound {
		t.Errorf("Pay by another user = %v", err)
	}
	backdate(t, env.pool, time.Hour, b.ID)
	if batch := must(env.svc.ExpireBatch(ctx, 10))(t); len(batch.BookingIDs) != 0 {
		t.Errorf("ExpireBatch took the paid booking: %+v", batch)
	}
	assertSeats(t, env, domain.SeatSold, &b.ID, "A1", "B1")

	// Declined: the booking waits for another payment, and the next one goes through.
	b2 := must(env.svc.Create(ctx, bob, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A2"]}}))(t)
	_, err = env.svc.Pay(ctx, bob, b2.ID, withLocal(local.TokenInsufficientFunds))
	var declined *domain.PaymentDeclinedError
	if !errors.As(err, &declined) || declined.DeclineCode != payment.DeclineInsufficientFunds {
		t.Fatalf("declined Pay = %v", err)
	}
	if status := bookingStatus(t, env.pool, b2.ID); status != domain.BookingPending {
		t.Errorf("booking after the decline is %s, want pending", status)
	}
	assertSeats(t, env, domain.SeatHeld, &b2.ID, "A2")
	if _, err := env.svc.Pay(ctx, bob, b2.ID, withLocal(local.TokenSuccess)); err != nil {
		t.Fatalf("Pay after the decline: %v", err)
	}
	assertSeats(t, env, domain.SeatSold, &b2.ID, "A2")
	stored := paymentsOf(t, env.pool, b2.ID)
	if len(stored) != 2 || stored[0].Status != domain.PaymentFailed || stored[0].FailureReason != payment.DeclineInsufficientFunds ||
		stored[1].Status != domain.PaymentSucceeded {
		t.Errorf("payments = %+v, want one failed and one succeeded", stored)
	}
}

func TestPayRejects(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	users := newUsers(t, env.pool, 2)
	ann, bob := users[0], users[1]

	b := must(env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"]}}))(t)
	for _, in := range []domain.NewPayment{{Method: "cash", Token: "x"}, {Method: "retired", Token: "x"}} {
		if _, err := env.svc.Pay(ctx, ann, b.ID, in); domainCode(err) != domain.CodePaymentMethodUnavailable {
			t.Errorf("Pay with %q = %v", in.Method, err)
		}
	}

	// The hold ran out a second ago, and no worker has been there: the database clock still says no.
	backdate(t, env.pool, time.Second, b.ID)
	if _, err := env.svc.Pay(ctx, ann, b.ID, withLocal(local.TokenSuccess)); domainCode(err) != domain.CodeBookingExpired {
		t.Errorf("Pay after the deadline = %v", err)
	}
	if status := bookingStatus(t, env.pool, b.ID); status != domain.BookingPending {
		t.Errorf("booking is %s; the payment must leave the expiry to the worker", status)
	}
	must(env.svc.ExpireBatch(ctx, 10))(t)
	if _, err := env.svc.Pay(ctx, ann, b.ID, withLocal(local.TokenSuccess)); domainCode(err) != domain.CodeBookingExpired {
		t.Errorf("Pay of an expired booking = %v", err)
	}

	b2 := must(env.svc.Create(ctx, bob, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A2"]}}))(t)
	if err := env.svc.Cancel(ctx, bob, b2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Pay(ctx, bob, b2.ID, withLocal(local.TokenSuccess)); domainCode(err) != domain.CodeBookingCanceled {
		t.Errorf("Pay of a canceled booking = %v", err)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM payments`); n != 0 {
		t.Errorf("%d payments recorded, want none", n)
	}
}

// TestPayOutlastsTheHold: a slow charge starts just before the hold runs out and ends after it, while expiry
// workers sweep every 20 ms. The processing state keeps them away, and the seats are sold.
func TestPayOutlastsTheHold(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	ann := newUsers(t, env.pool, 1)[0]
	b := must(env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"], env.seats["A2"]}}))(t)
	exec(t, env.pool, `UPDATE bookings SET expires_at = now() + interval '100 milliseconds' WHERE id = $1`, b.ID)

	expirers, stop := startExpirers(t, env.pool, 2, 10, env.logs, false)
	res, err := env.svc.Pay(ctx, ann, b.ID, withLocal(local.TokenSlow))
	time.Sleep(100 * time.Millisecond) // several more sweeps after the payment
	stop()
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}

	if res.Booking.Status != domain.BookingPaid || !res.Booking.PaidAt.After(res.Booking.ExpiresAt) {
		t.Errorf("booking = %+v, want paid after its hold ran out", res.Booking)
	}
	assertSeats(t, env, domain.SeatSold, &b.ID, "A1", "A2")
	for _, e := range expirers {
		if ids := e.expired(); len(ids) != 0 {
			t.Errorf("a worker expired %v", ids)
		}
	}
}

// TestPayOutcomeUnknownIsReconciled covers payments that the API leaves in flight, and how the reconciliation
// settles each of them by asking the provider:
//   - ann: tok_timeout, no answer within the payment timeout; nothing was charged;
//   - bob: the provider charged, but the answer was lost;
//   - cat: tok_error, the connection broke; nothing was charged, and the hold has run out meanwhile;
//   - dan: like bob, but the provider cannot be asked at first.
func TestPayOutcomeUnknownIsReconciled(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	users := newUsers(t, env.pool, 4)
	ann, bob, cat, dan := users[0], users[1], users[2], users[3]
	create := func(user uuid.UUID, label string) domain.Booking {
		return must(env.svc.Create(ctx, user, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats[label]}}))(t)
	}
	annB, bobB, catB, danB := create(ann, "A1"), create(bob, "A2"), create(cat, "B1"), create(dan, "B2")

	inFlight := func(user uuid.UUID, b domain.Booking, in domain.NewPayment) domain.Payment {
		t.Helper()
		res, err := env.svc.Pay(ctx, user, b.ID, in)
		if err != nil || res.Payment.Status != domain.PaymentPending || res.Booking.Status != domain.BookingProcessing {
			t.Fatalf("Pay = %+v, %v; want the payment in flight", res, err)
		}
		return res.Payment
	}
	start := time.Now()
	annP := inFlight(ann, annB, withLocal(local.TokenTimeout))
	if waited := time.Since(start); waited < testPaymentTimeout {
		t.Errorf("tok_timeout answered after %s, before the %s payment timeout", waited, testPaymentTimeout)
	}
	env.scripted.script(func(_ context.Context, req payment.PayRequest) (payment.Charge, error) {
		env.scripted.charge(req.PaymentID)
		return payment.Charge{}, errors.New("connection reset after the charge")
	})
	bobP, danP := inFlight(bob, bobB, withScripted), inFlight(dan, danB, withScripted)
	catP := inFlight(cat, catB, withLocal(local.TokenError))

	// The expiry worker leaves payments in flight alone, even after the hold has run out.
	backdate(t, env.pool, time.Minute, catB.ID)
	if batch := must(env.svc.ExpireBatch(ctx, 10))(t); len(batch.BookingIDs) != 0 {
		t.Errorf("ExpireBatch = %+v", batch)
	}
	// Within the grace period, the API may still be working on them.
	if batch := must(env.svc.ReconcileBatch(ctx, uuid.UUID{}, 10))(t); batch.Checked != 0 {
		t.Errorf("ReconcileBatch within the grace period = %+v", batch)
	}

	backdatePayments(t, env.pool, testPaymentGrace, annP.ID, bobP.ID, catP.ID, danP.ID)
	env.scripted.failStatus(errors.New("provider unreachable"))
	batch, err := env.svc.ReconcileBatch(ctx, uuid.UUID{}, 10)
	if batch.Checked != 4 || batch.Paid != 0 || batch.Failed != 2 ||
		err == nil || !strings.Contains(err.Error(), bobP.ID.String()) || !strings.Contains(err.Error(), danP.ID.String()) {
		t.Fatalf("first ReconcileBatch = %+v, %v; want ann's and cat's payments settled, bob's and dan's left", batch, err)
	}
	env.scripted.failStatus(nil)
	batch, err = env.svc.ReconcileBatch(ctx, uuid.UUID{}, 10)
	if err != nil || batch.Checked != 2 || batch.Paid != 2 {
		t.Fatalf("second ReconcileBatch = %+v, %v", batch, err)
	}

	for _, tt := range []struct {
		name    string
		booking domain.Booking
		payment domain.Payment
		status  domain.BookingStatus
		paid    domain.PaymentStatus
		reason  string
		seat    string
		seatNow domain.SeatStatus
	}{
		{"ann", annB, annP, domain.BookingPending, domain.PaymentFailed, booking.ReasonNotFoundAtProvider, "A1", domain.SeatHeld},
		{"bob", bobB, bobP, domain.BookingPaid, domain.PaymentSucceeded, "", "A2", domain.SeatSold},
		{"cat", catB, catP, domain.BookingExpired, domain.PaymentFailed, booking.ReasonNotFoundAtProvider, "B1", domain.SeatAvailable},
		{"dan", danB, danP, domain.BookingPaid, domain.PaymentSucceeded, "", "B2", domain.SeatSold},
	} {
		if status := bookingStatus(t, env.pool, tt.booking.ID); status != tt.status {
			t.Errorf("%s's booking is %s, want %s", tt.name, status, tt.status)
		}
		p := paymentsOf(t, env.pool, tt.booking.ID)
		if len(p) != 1 || p[0].ID != tt.payment.ID || p[0].Status != tt.paid || p[0].FailureReason != tt.reason {
			t.Errorf("%s's payments = %+v, want one %s (%q)", tt.name, p, tt.paid, tt.reason)
		}
		holder := &tt.booking.ID
		if tt.seatNow == domain.SeatAvailable {
			holder = nil
		}
		assertSeats(t, env, tt.seatNow, holder, tt.seat)
	}

	// Ann's hold has not run out, so she can pay again.
	if _, err := env.svc.Pay(ctx, ann, annB.ID, withLocal(local.TokenSuccess)); err != nil {
		t.Errorf("ann pays again: %v", err)
	}
	if batch := must(env.svc.ReconcileBatch(ctx, uuid.UUID{}, 10))(t); batch.Checked != 0 {
		t.Errorf("third ReconcileBatch = %+v, want nothing left", batch)
	}
}

// TestPayRefundsALateCharge forces what the grace period makes rare: the provider answers only after the
// reconciliation has given the payment up. The charge is refunded, and the booking takes another payment.
func TestPayRefundsALateCharge(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	ann := newUsers(t, env.pool, 1)[0]
	b := must(env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"]}}))(t)

	charging, answer := make(chan struct{}), make(chan struct{})
	env.scripted.script(func(_ context.Context, req payment.PayRequest) (payment.Charge, error) {
		close(charging)
		<-answer // a provider that answers late, whatever the caller's deadline
		return env.scripted.charge(req.PaymentID), nil
	})
	var (
		payErr error
		paid   = make(chan struct{})
	)
	go func() {
		defer close(paid)
		_, payErr = env.svc.Pay(ctx, ann, b.ID, withScripted)
	}()

	<-charging
	p := paymentsOf(t, env.pool, b.ID)[0]
	backdatePayments(t, env.pool, testPaymentGrace, p.ID)
	if batch := must(env.svc.ReconcileBatch(ctx, uuid.UUID{}, 10))(t); batch.Failed != 1 {
		t.Fatalf("ReconcileBatch = %+v, want the payment given up", batch)
	}
	close(answer)
	<-paid

	if domainCode(payErr) != domain.CodePaymentRefunded {
		t.Fatalf("Pay = %v, want PAYMENT_REFUNDED", payErr)
	}
	stored := paymentsOf(t, env.pool, b.ID)
	if len(stored) != 1 || stored[0].Status != domain.PaymentRefunded ||
		stored[0].ProviderRef != "scripted_"+p.ID.String() || stored[0].FailureReason != booking.ReasonBookingNotPayable {
		t.Errorf("payment = %+v", stored)
	}
	if refunds := env.scripted.refunded(); len(refunds) != 1 || refunds[0].PaymentID != p.ID || refunds[0].AmountCents != b.TotalCents {
		t.Errorf("refunds = %+v", refunds)
	}
	if status := bookingStatus(t, env.pool, b.ID); status != domain.BookingPending {
		t.Errorf("booking is %s, want pending", status)
	}
	assertSeats(t, env, domain.SeatHeld, &b.ID, "A1")
	if _, err := env.svc.Pay(ctx, ann, b.ID, withLocal(local.TokenSuccess)); err != nil {
		t.Errorf("Pay after the refund: %v", err)
	}
}

// TestPaymentStoreGuards checks the database guards behind the payment flow: one pending payment per booking,
// and a payment ends only from the status the caller expects.
func TestPaymentStoreGuards(t *testing.T) {
	t.Parallel()
	env := newBookingEnv(t, 3*time.Second)
	ctx := t.Context()
	ann := newUsers(t, env.pool, 1)[0]
	b := must(env.svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{env.seats["A1"]}}))(t)

	first := domain.Payment{ID: uuid.NewV7(), BookingID: b.ID, Provider: local.ID, AmountCents: b.TotalCents}
	err := env.uow.Do(ctx, func(ctx context.Context, r booking.TxRepos) error {
		created, err := r.Payments().Create(ctx, first)
		if err != nil || created.Status != domain.PaymentPending || created.CreatedAt.IsZero() {
			t.Errorf("Create = %+v, %v", created, err)
		}
		second := first
		second.ID = uuid.NewV7()
		_, err = r.Payments().Create(ctx, second)
		return err
	})
	if domainCode(err) != domain.CodePaymentInProgress {
		t.Errorf("second pending payment = %v, want PAYMENT_IN_PROGRESS", err)
	}

	err = env.uow.Do(ctx, func(ctx context.Context, r booking.TxRepos) error {
		if _, err := r.Payments().Create(ctx, first); err != nil {
			return err
		}
		_, err := r.Payments().Finish(ctx, first.ID, domain.PaymentFailed, domain.PaymentOutcome{Status: domain.PaymentRefunded})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "no longer failed") {
		t.Errorf("Finish from the wrong status = %v", err)
	}
	if n := countRows(t, env.pool, `SELECT count(*) FROM payments`); n != 0 {
		t.Errorf("%d payments left behind by rolled-back transactions", n)
	}
}
