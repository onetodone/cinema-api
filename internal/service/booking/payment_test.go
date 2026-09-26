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

// fakeProvider is a payment.Provider whose answers the test scripts. Unscripted, it charges every payment and
// answers Status with "not found".
type fakeProvider struct {
	id string

	mu        sync.Mutex
	pay       func(ctx context.Context, req payment.PayRequest) (payment.Charge, error)
	status    func(req payment.StatusRequest) (payment.Charge, error)
	refundErr error
	paid      []payment.PayRequest
	refunded  []payment.RefundRequest
}

func (f *fakeProvider) ID() string   { return f.id }
func (f *fakeProvider) Name() string { return "Fake " + f.id }

func (f *fakeProvider) Pay(ctx context.Context, req payment.PayRequest) (payment.Charge, error) {
	f.mu.Lock()
	f.paid = append(f.paid, req)
	pay := f.pay
	f.mu.Unlock()
	if pay != nil {
		return pay(ctx, req)
	}
	return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_" + req.PaymentID.String()}, nil
}

func (f *fakeProvider) Refund(_ context.Context, req payment.RefundRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunded = append(f.refunded, req)
	return f.refundErr
}

func (f *fakeProvider) Status(_ context.Context, req payment.StatusRequest) (payment.Charge, error) {
	f.mu.Lock()
	status := f.status
	f.mu.Unlock()
	if status != nil {
		return status(req)
	}
	return payment.Charge{Status: payment.ChargeNotFound}, nil
}

func (f *fakeProvider) onPay(pay func(ctx context.Context, req payment.PayRequest) (payment.Charge, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pay = pay
}

func (f *fakeProvider) onStatus(status func(req payment.StatusRequest) (payment.Charge, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeProvider) charges() []payment.PayRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paid)
}

func (f *fakeProvider) refunds() []payment.RefundRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refunded)
}

// Canned provider answers.
var (
	declined = func(code string) func(context.Context, payment.PayRequest) (payment.Charge, error) {
		return func(context.Context, payment.PayRequest) (payment.Charge, error) {
			return payment.Charge{Status: payment.ChargeDeclined, Ref: "ch_declined", DeclineCode: code}, nil
		}
	}
	noAnswer = func(context.Context, payment.PayRequest) (payment.Charge, error) {
		return payment.Charge{}, errors.New("connection reset")
	}
)

var card = domain.NewPayment{Method: "card", Token: "tok_visa"}

// book holds seats of showtime 1 for user.
func book(t *testing.T, svc *Service, user uuid.UUID, seats ...int64) domain.Booking {
	t.Helper()
	b, err := svc.Create(t.Context(), user, domain.NewBooking{ShowtimeID: 1, SeatIDs: seats})
	if err != nil {
		t.Fatalf("book seats %v: %v", seats, err)
	}
	return b
}

// assertBooking checks the status of a booking and of the given seats of its showtime.
func assertBooking(t *testing.T, db *memDB, b domain.Booking, status domain.BookingStatus, seats domain.SeatStatus) {
	t.Helper()
	got, _ := db.booking(b.ID)
	if got.Status != status {
		t.Errorf("booking is %s, want %s", got.Status, status)
	}
	for _, seat := range b.Seats {
		s := db.seat(b.Showtime.ID, seat.SeatID)
		holder := s.bookingID == b.ID
		if s.seat.Status != seats || holder == (seats == domain.SeatAvailable) {
			t.Errorf("seat %d = %s held by %s, want %s", seat.SeatID, s.seat.Status, s.bookingID, seats)
		}
	}
}

func TestPaySellsSeats(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 4, 1)

	var phase1 []string
	provider.onPay(func(context.Context, payment.PayRequest) (payment.Charge, error) {
		phase1 = db.lastCalls()
		return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_1"}, nil
	})
	res, err := svc.Pay(t.Context(), ann, b.ID, card)
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}

	p := res.Payment
	if p.Status != domain.PaymentSucceeded || p.ProviderRef != "ch_1" || p.AmountCents != 2500 ||
		p.Provider != "card" || p.BookingID != b.ID || p.FailureReason != "" {
		t.Errorf("payment = %+v", p)
	}
	if res.Booking.Status != domain.BookingPaid || !res.Booking.PaidAt.Equal(testNow) || len(res.Booking.Seats) != 2 ||
		res.Booking.Showtime.StartsAt.Location() != cinema {
		t.Errorf("booking = %+v", res.Booking)
	}
	assertBooking(t, db, b, domain.BookingPaid, domain.SeatSold)

	charges := provider.charges()
	want := payment.PayRequest{PaymentID: p.ID, BookingID: b.ID, AmountCents: 2500, Currency: "EUR", Token: "tok_visa"}
	if len(charges) != 1 || charges[0] != want {
		t.Errorf("provider got %+v, want %+v", charges, want)
	}
	if want := []string{"lock booking", "set status processing", "insert payment"}; !slices.Equal(phase1, want) {
		t.Errorf("step 1 calls = %q, want %q", phase1, want)
	}
	want3 := []string{
		"lock booking", "get payment", "lock seats of 1 bookings", "sell seats", "set status paid", "finish payment succeeded",
	}
	if got := db.lastCalls(); !slices.Equal(got, want3) {
		t.Errorf("step 3 calls = %q, want %q (the booking locked before its seats)", got, want3)
	}

	// A paid booking can be neither canceled nor paid again.
	if err := svc.Cancel(t.Context(), ann, b.ID); code(err) != domain.CodeBookingNotCancelable {
		t.Errorf("Cancel = %v", err)
	}
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); code(err) != domain.CodeBookingAlreadyPaid {
		t.Errorf("second Pay = %v", err)
	}
	if n := len(provider.charges()); n != 1 {
		t.Errorf("the provider was called %d times, want once", n)
	}
}

func TestPayDeclined(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	provider.onPay(declined("insufficient_funds"))
	_, err := svc.Pay(t.Context(), ann, b.ID, card)
	var de *domain.PaymentDeclinedError
	if !errors.As(err, &de) || de.DeclineCode != "insufficient_funds" || !errors.Is(err, domain.ErrPaymentRequired) {
		t.Fatalf("Pay = %v, want a decline for insufficient_funds", err)
	}
	// The booking waits for another payment, and still holds its seats.
	assertBooking(t, db, b, domain.BookingPending, domain.SeatHeld)

	provider.onPay(nil)
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); err != nil {
		t.Fatalf("Pay after the decline: %v", err)
	}
	assertBooking(t, db, b, domain.BookingPaid, domain.SeatSold)

	payments := db.paymentsOf(b.ID)
	if len(payments) != 2 ||
		payments[0].Status != domain.PaymentFailed || payments[0].FailureReason != "insufficient_funds" ||
		payments[0].ProviderRef != "ch_declined" || payments[1].Status != domain.PaymentSucceeded {
		t.Errorf("payments = %+v, want one failed and one succeeded", payments)
	}
}

// TestPayOutlastsTheHold shows why a booking is processing while its payment is in flight: the hold runs out
// during the charge, the expiry worker comes by, and the charge still ends in a sale.
func TestPayOutlastsTheHold(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1, 2)

	provider.onPay(func(ctx context.Context, _ payment.PayRequest) (payment.Charge, error) {
		db.advance(holdTTL + time.Minute)
		if batch, err := svc.ExpireBatch(ctx, 10); err != nil || len(batch.BookingIDs) != 0 {
			t.Errorf("ExpireBatch during the charge = %+v, %v; want the booking left alone", batch, err)
		}
		return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_slow"}, nil
	})
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	assertBooking(t, db, b, domain.BookingPaid, domain.SeatSold)
}

func TestPayDeclinedAfterTheHoldRanOut(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1, 2)

	decline := declined("card_declined")
	provider.onPay(func(ctx context.Context, req payment.PayRequest) (payment.Charge, error) {
		db.advance(holdTTL)
		return decline(ctx, req)
	})
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); code(err) != domain.CodePaymentDeclined {
		t.Fatalf("Pay = %v, want a decline", err)
	}
	// Nothing waits for another payment: the booking expires at once and releases its seats.
	assertBooking(t, db, b, domain.BookingExpired, domain.SeatAvailable)
	if p := db.paymentsOf(b.ID); len(p) != 1 || p[0].Status != domain.PaymentFailed {
		t.Errorf("payments = %+v", p)
	}
	want := []string{
		"lock booking", "get payment", "lock seats of 1 bookings", "release seats", "set status expired", "finish payment failed",
	}
	if got := db.lastCalls(); !slices.Equal(got, want) {
		t.Errorf("step 3 calls = %q, want %q", got, want)
	}
}

func TestPayRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(db *memDB, b domain.Booking)
		user  uuid.UUID
		in    domain.NewPayment
		code  string // "" for a validation error
	}{
		{name: "invalid input", in: domain.NewPayment{}},
		{name: "unknown method", in: domain.NewPayment{Method: "cash", Token: "x"}, code: domain.CodePaymentMethodUnavailable},
		{name: "disabled method", in: domain.NewPayment{Method: "old", Token: "x"}, code: domain.CodePaymentMethodUnavailable},
		{name: "booking of another user", user: bob, code: domain.CodeBookingNotFound},
		{
			// The expiry worker has not been there yet, but the database clock decides.
			name: "hold ran out", code: domain.CodeBookingExpired,
			setup: func(db *memDB, _ domain.Booking) { db.advance(holdTTL) },
		},
		{
			name: "payment in progress", code: domain.CodePaymentInProgress,
			setup: func(db *memDB, b domain.Booking) { db.setBookingStatus(b.ID, domain.BookingProcessing) },
		},
		{
			name: "paid", code: domain.CodeBookingAlreadyPaid,
			setup: func(db *memDB, b domain.Booking) { db.setBookingStatus(b.ID, domain.BookingPaid) },
		},
		{
			name: "expired", code: domain.CodeBookingExpired,
			setup: func(db *memDB, b domain.Booking) { db.setBookingStatus(b.ID, domain.BookingExpired) },
		},
		{
			name: "canceled", code: domain.CodeBookingCanceled,
			setup: func(db *memDB, b domain.Booking) { db.setBookingStatus(b.ID, domain.BookingCanceled) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, db, provider := newPayTestService(t)
			b := book(t, svc, ann, 1)
			before, _ := db.booking(b.ID)
			if tt.setup != nil {
				tt.setup(db, b)
				before, _ = db.booking(b.ID)
			}
			user, in := ann, card
			if tt.user != (uuid.UUID{}) {
				user = tt.user
			}
			if tt.in != (domain.NewPayment{}) || tt.code == "" {
				in = tt.in
			}

			_, err := svc.Pay(t.Context(), user, b.ID, in)
			if tt.code == "" {
				var ve *domain.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("Pay = %v, want a validation error", err)
				}
			} else if code(err) != tt.code {
				t.Errorf("Pay = %v, want %s", err, tt.code)
			}
			if tt.code == domain.CodeBookingExpired && !errors.Is(err, domain.ErrGone) {
				t.Errorf("err = %v, want kind ErrGone", err)
			}

			if n := len(provider.charges()); n != 0 {
				t.Errorf("the provider was called %d times", n)
			}
			if p := db.paymentsOf(b.ID); len(p) != 0 {
				t.Errorf("payments = %+v, want none", p)
			}
			if after, _ := db.booking(b.ID); after.Status != before.Status {
				t.Errorf("booking went from %s to %s", before.Status, after.Status)
			}
		})
	}
}

func TestPayProviderUnavailable(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	provider.onPay(func(context.Context, payment.PayRequest) (payment.Charge, error) {
		return payment.Charge{}, fmt.Errorf("maintenance: %w", payment.ErrUnavailable)
	})
	_, err := svc.Pay(t.Context(), ann, b.ID, card)
	if code(err) != domain.CodePaymentProviderUnavailable || !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Pay = %v, want PAYMENT_PROVIDER_UNAVAILABLE", err)
	}
	assertBooking(t, db, b, domain.BookingPending, domain.SeatHeld)
	if p := db.paymentsOf(b.ID); len(p) != 1 || p[0].Status != domain.PaymentFailed || p[0].FailureReason != ReasonProviderUnavailable {
		t.Errorf("payments = %+v", p)
	}
}

// TestPayOutcomeUnknown covers a provider that does not answer: the payment stays pending and the booking
// processing, and nothing but the reconciliation moves them.
func TestPayOutcomeUnknown(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	provider.onPay(noAnswer)
	res, err := svc.Pay(t.Context(), ann, b.ID, card)
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res.Payment.Status != domain.PaymentPending || res.Booking.Status != domain.BookingProcessing ||
		res.Booking.ID != b.ID || len(res.Booking.Seats) != 1 {
		t.Errorf("result = %+v", res)
	}
	assertBooking(t, db, b, domain.BookingProcessing, domain.SeatHeld)

	db.advance(holdTTL + time.Hour)
	if batch, err := svc.ExpireBatch(t.Context(), 10); err != nil || len(batch.BookingIDs) != 0 {
		t.Errorf("ExpireBatch = %+v, %v; want the processing booking left alone", batch, err)
	}
	if err := svc.Cancel(t.Context(), ann, b.ID); code(err) != domain.CodeBookingNotCancelable {
		t.Errorf("Cancel = %v", err)
	}
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); code(err) != domain.CodePaymentInProgress {
		t.Errorf("second Pay = %v", err)
	}
}

// TestPayOutlivesTheRequest: once the payment is in flight, a client that hangs up does not cut the charge
// short, and the outcome is recorded. The charge has its own deadline instead.
func TestPayOutlivesTheRequest(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	ctx, hangUp := context.WithCancel(t.Context())
	provider.onPay(func(ctx context.Context, _ payment.PayRequest) (payment.Charge, error) {
		hangUp()
		if ctx.Err() != nil {
			t.Error("the charge was canceled together with the request")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > paymentTimeout {
			t.Errorf("charge deadline = %v (set %t), want within %s", deadline, ok, paymentTimeout)
		}
		return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_1"}, nil
	})
	if _, err := svc.Pay(ctx, ann, b.ID, card); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	assertBooking(t, db, b, domain.BookingPaid, domain.SeatSold)
}

// TestPayRecordingFails: the provider charged, but the outcome could not be stored. The client learns that the
// payment is in flight, and the reconciliation later sells the seats.
func TestPayRecordingFails(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	db.setFinishErr(errors.New("database is down"))
	res, err := svc.Pay(t.Context(), ann, b.ID, card)
	if err != nil || res.Payment.Status != domain.PaymentPending || res.Booking.Status != domain.BookingProcessing {
		t.Fatalf("Pay = %+v, %v; want the payment in flight", res, err)
	}
	assertBooking(t, db, b, domain.BookingProcessing, domain.SeatHeld) // the failed step rolled back

	db.setFinishErr(nil)
	provider.onStatus(func(req payment.StatusRequest) (payment.Charge, error) {
		return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_" + req.PaymentID.String()}, nil
	})
	db.advance(paymentGrace)
	batch, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 10)
	if err != nil || batch.Checked != 1 || batch.Paid != 1 || batch.LastID != res.Payment.ID {
		t.Fatalf("ReconcileBatch = %+v, %v", batch, err)
	}
	assertBooking(t, db, b, domain.BookingPaid, domain.SeatSold)
	if p := db.payment(res.Payment.ID); p.Status != domain.PaymentSucceeded || p.ProviderRef != "ch_"+p.ID.String() {
		t.Errorf("payment = %+v", p)
	}
}

// TestPayRollsBackAnIncompleteSale: a sale that does not change every held seat means the inventory is
// inconsistent. Nothing of it is committed, and the payment stays in flight for the reconciliation.
func TestPayRollsBackAnIncompleteSale(t *testing.T) {
	t.Parallel()
	svc, db, _ := newPayTestService(t)
	b := book(t, svc, ann, 1, 2)

	db.mu.Lock()
	db.sellShortfall = 1
	db.mu.Unlock()
	res, err := svc.Pay(t.Context(), ann, b.ID, card)
	if err != nil || res.Payment.Status != domain.PaymentPending {
		t.Fatalf("Pay = %+v, %v; want the payment in flight", res, err)
	}
	assertBooking(t, db, b, domain.BookingProcessing, domain.SeatHeld)
}

func TestPayUnavailableAndRecordingFails(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	db.setFinishErr(errors.New("database is down"))
	provider.onPay(func(context.Context, payment.PayRequest) (payment.Charge, error) {
		return payment.Charge{}, payment.ErrUnavailable
	})
	res, err := svc.Pay(t.Context(), ann, b.ID, card)
	if err != nil || res.Payment.Status != domain.PaymentPending {
		t.Fatalf("Pay = %+v, %v; want the payment in flight", res, err)
	}
	assertBooking(t, db, b, domain.BookingProcessing, domain.SeatHeld)
}

// TestPayRefundsALateCharge: the charge hangs for longer than the grace period, the reconciliation finds no
// charge at the provider and gives the payment up, and then the charge goes through after all. The API refunds
// it, and the booking stays available to another payment.
func TestPayRefundsALateCharge(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	provider.onPay(func(ctx context.Context, _ payment.PayRequest) (payment.Charge, error) {
		db.advance(paymentGrace)
		if batch, err := svc.ReconcileBatch(ctx, uuid.UUID{}, 10); err != nil || batch.Failed != 1 {
			t.Errorf("ReconcileBatch during the charge = %+v, %v", batch, err)
		}
		return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_late"}, nil
	})
	_, err := svc.Pay(t.Context(), ann, b.ID, card)
	if code(err) != domain.CodePaymentRefunded || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Pay = %v, want PAYMENT_REFUNDED", err)
	}

	payments := db.paymentsOf(b.ID)
	if len(payments) != 1 {
		t.Fatalf("payments = %+v", payments)
	}
	p := payments[0]
	if p.Status != domain.PaymentRefunded || p.ProviderRef != "ch_late" || p.FailureReason != ReasonBookingNotPayable {
		t.Errorf("payment = %+v", p)
	}
	wantRefund := payment.RefundRequest{PaymentID: p.ID, ProviderRef: "ch_late", AmountCents: 1000, Currency: "EUR"}
	if refunds := provider.refunds(); len(refunds) != 1 || refunds[0] != wantRefund {
		t.Errorf("refunds = %+v, want %+v", refunds, wantRefund)
	}
	// The hold has not run out, so the booking waits for another payment.
	assertBooking(t, db, b, domain.BookingPending, domain.SeatHeld)
	provider.onPay(nil)
	if _, err := svc.Pay(t.Context(), ann, b.ID, card); err != nil {
		t.Errorf("Pay after the refund: %v", err)
	}
}

func TestPayRefundFails(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	provider.refundErr = errors.New("refunds are down")
	provider.onPay(func(ctx context.Context, _ payment.PayRequest) (payment.Charge, error) {
		db.advance(paymentGrace)
		_, _ = svc.ReconcileBatch(ctx, uuid.UUID{}, 10)
		return payment.Charge{Status: payment.ChargeSucceeded, Ref: "ch_late"}, nil
	})
	_, err := svc.Pay(t.Context(), ann, b.ID, card)
	var de *domain.Error
	if err == nil || errors.As(err, &de) || !strings.Contains(err.Error(), "refunded by hand") {
		t.Fatalf("Pay = %v, want an internal error that asks for a manual refund", err)
	}
	if p := db.paymentsOf(b.ID); p[0].Status != domain.PaymentFailed {
		t.Errorf("payment = %+v, want it left failed", p[0])
	}
}

func TestPayRejectsAnAnswerOutsideTheContract(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	b := book(t, svc, ann, 1)

	provider.onPay(func(context.Context, payment.PayRequest) (payment.Charge, error) {
		return payment.Charge{Status: payment.ChargeNotFound}, nil
	})
	_, err := svc.Pay(t.Context(), ann, b.ID, card)
	var de *domain.Error
	if err == nil || errors.As(err, &de) {
		t.Fatalf("Pay = %v, want an internal error", err)
	}
	// Nothing was charged, so the booking waits for another payment.
	assertBooking(t, db, b, domain.BookingPending, domain.SeatHeld)
}

// stuckPayments books one seat of showtime 1 per seat id, each for another user, and starts a payment for each
// that the provider does not answer.
func stuckPayments(t *testing.T, svc *Service, provider *fakeProvider, seats ...int64) ([]domain.Booking, []domain.Payment) {
	t.Helper()
	provider.onPay(noAnswer)
	defer provider.onPay(nil)
	var (
		bookings []domain.Booking
		payments []domain.Payment
	)
	for _, seat := range seats {
		user := uuid.NewV7()
		b := book(t, svc, user, seat)
		res, err := svc.Pay(t.Context(), user, b.ID, card)
		if err != nil || res.Payment.Status != domain.PaymentPending {
			t.Fatalf("Pay = %+v, %v", res, err)
		}
		bookings, payments = append(bookings, b), append(payments, res.Payment)
	}
	return bookings, payments
}

func TestReconcileBatch(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	bookings, payments := stuckPayments(t, svc, provider, 1, 2, 3, 4)

	answers := map[uuid.UUID]payment.Charge{
		payments[0].ID: {Status: payment.ChargeSucceeded, Ref: "ch_0"},
		payments[1].ID: {Status: payment.ChargeDeclined, Ref: "ch_1", DeclineCode: "expired_card"},
		payments[2].ID: {Status: payment.ChargeNotFound},
		payments[3].ID: {Status: payment.ChargeRefunded, Ref: "ch_3"},
	}
	provider.onStatus(func(req payment.StatusRequest) (payment.Charge, error) {
		return answers[req.PaymentID], nil
	})

	// Younger than the grace period: the API may still be working on them.
	db.advance(paymentGrace - time.Second)
	if batch, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 10); err != nil || batch.Checked != 0 {
		t.Fatalf("ReconcileBatch within the grace period = %+v, %v", batch, err)
	}

	db.advance(time.Second)
	batch, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 10)
	if err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}
	if batch.Checked != 4 || batch.Paid != 1 || batch.Failed != 3 || batch.LastID != payments[3].ID {
		t.Errorf("batch = %+v", batch)
	}

	assertBooking(t, db, bookings[0], domain.BookingPaid, domain.SeatSold)
	for _, b := range bookings[1:] {
		assertBooking(t, db, b, domain.BookingPending, domain.SeatHeld)
	}
	for i, want := range []domain.PaymentOutcome{
		{Status: domain.PaymentSucceeded, ProviderRef: "ch_0"},
		{Status: domain.PaymentFailed, ProviderRef: "ch_1", FailureReason: "expired_card"},
		{Status: domain.PaymentFailed, FailureReason: ReasonNotFoundAtProvider},
		{Status: domain.PaymentRefunded, ProviderRef: "ch_3", FailureReason: ReasonRefundedAtProvider},
	} {
		p := db.payment(payments[i].ID)
		if got := (domain.PaymentOutcome{Status: p.Status, ProviderRef: p.ProviderRef, FailureReason: p.FailureReason}); got != want {
			t.Errorf("payment %d = %+v, want %+v", i, got, want)
		}
	}

	// Nothing is left to settle.
	if batch, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 10); err != nil || batch.Checked != 0 {
		t.Errorf("second ReconcileBatch = %+v, %v", batch, err)
	}
}

// TestReconcileBatchExpiresBookings: a payment that is settled as failed after its booking's hold has run out
// leaves nothing to wait for, so the booking expires and its seats are released.
func TestReconcileBatchExpiresBookings(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	bookings, _ := stuckPayments(t, svc, provider, 1)

	db.advance(holdTTL)
	if batch, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 10); err != nil || batch.Failed != 1 {
		t.Fatalf("ReconcileBatch = %+v, %v", batch, err)
	}
	assertBooking(t, db, bookings[0], domain.BookingExpired, domain.SeatAvailable)
}

func TestReconcileBatchPages(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	_, payments := stuckPayments(t, svc, provider, 1, 2, 3)
	db.advance(paymentGrace)

	first, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 2)
	if err != nil || first.Checked != 2 || first.LastID != payments[1].ID {
		t.Fatalf("first batch = %+v, %v", first, err)
	}
	second, err := svc.ReconcileBatch(t.Context(), first.LastID, 2)
	if err != nil || second.Checked != 1 || second.LastID != payments[2].ID {
		t.Fatalf("second batch = %+v, %v", second, err)
	}
	if _, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 0); err == nil {
		t.Error("a limit of 0 was accepted")
	}
}

// TestReconcileBatchLeavesUnsettledPayments: a payment whose provider cannot be asked stays pending, the error
// names it, and the payments after it are settled anyway.
func TestReconcileBatchLeavesUnsettledPayments(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	bookings, payments := stuckPayments(t, svc, provider, 1, 2, 3)
	db.advance(paymentGrace)

	db.mu.Lock()
	gone := db.state.payments[payments[1].ID]
	gone.Provider = "gone" // a provider that this build no longer has
	db.state.payments[gone.ID] = gone
	old := db.state.payments[payments[2].ID]
	old.Provider = "old" // disabled for new payments, but still registered
	db.state.payments[old.ID] = old
	db.mu.Unlock()
	provider.onStatus(func(req payment.StatusRequest) (payment.Charge, error) {
		if req.PaymentID == payments[0].ID {
			return payment.Charge{}, errors.New("provider unreachable")
		}
		return payment.Charge{Status: payment.ChargeNotFound}, nil
	})

	batch, err := svc.ReconcileBatch(t.Context(), uuid.UUID{}, 10)
	if batch.Checked != 3 || batch.Failed != 1 || batch.Paid != 0 || batch.Unsettled != 2 {
		t.Errorf("batch = %+v", batch)
	}
	for _, want := range []string{
		payments[0].ID.String() + ": ask payment provider card: provider unreachable",
		payments[1].ID.String() + `: payment provider "gone" is not configured`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	assertBooking(t, db, bookings[0], domain.BookingProcessing, domain.SeatHeld)
	assertBooking(t, db, bookings[1], domain.BookingProcessing, domain.SeatHeld)
	assertBooking(t, db, bookings[2], domain.BookingPending, domain.SeatHeld)
}

// TestReconcileBatchStopsWhenCanceled: the payment in flight is finished, and no other one is started.
func TestReconcileBatchStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	svc, db, provider := newPayTestService(t)
	_, payments := stuckPayments(t, svc, provider, 1, 2)
	db.advance(paymentGrace)

	ctx, cancel := context.WithCancel(t.Context())
	provider.onStatus(func(payment.StatusRequest) (payment.Charge, error) {
		cancel()
		return payment.Charge{Status: payment.ChargeNotFound}, nil
	})
	batch, err := svc.ReconcileBatch(ctx, uuid.UUID{}, 10)
	if err != nil || batch.Checked != 1 || batch.Failed != 1 || batch.LastID != payments[0].ID {
		t.Errorf("batch = %+v, %v; want only the first payment settled", batch, err)
	}
	if p := db.payment(payments[1].ID); p.Status != domain.PaymentPending {
		t.Errorf("second payment = %+v, want untouched", p)
	}
}
