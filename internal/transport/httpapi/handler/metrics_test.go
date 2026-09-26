package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
)

// newTestMetrics returns metrics on a registry of their own.
func newTestMetrics() *metrics.Metrics {
	return metrics.New(prometheus.NewRegistry())
}

// asSampleUser returns a request with a JSON body, sent by sampleUser.
func asSampleUser(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(principal.NewContext(req.Context(), domain.Principal{UserID: sampleUser.ID, Role: domain.RoleCustomer}))
}

func TestCreateBookingCountsEveryAttemptByResult(t *testing.T) {
	t.Parallel()

	m := newTestMetrics()
	for _, err := range []error{
		nil,
		nil,
		domain.SeatsUnavailable([]int64{7}),
		domain.Busy(domain.CodeSeatBusy, "locked"),
		domain.Busy(domain.CodeBookingBusy, "locked"),
		domain.Conflict(domain.CodeActiveBookingExists, "one at a time"),
		domain.NotFound(domain.CodeShowtimeNotFound, "no such showtime"),
		errors.New("database is down"),
	} {
		h := NewBookings(&stubBookings{booking: sampleBooking, err: err}, "USD", m, slog.New(slog.DiscardHandler))
		h.Create(httptest.NewRecorder(), asSampleUser(t, http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":[7]}`))
	}
	// A body that cannot be decoded never reaches the service, so it is not an attempt.
	h := NewBookings(&stubBookings{}, "USD", m, slog.New(slog.DiscardHandler))
	h.Create(httptest.NewRecorder(), asSampleUser(t, http.MethodPost, "/v1/bookings", `{`))

	for result, want := range map[string]float64{
		metrics.BookingCreated:             2,
		metrics.BookingSeatUnavailable:     1,
		metrics.BookingBusy:                2,
		metrics.BookingActiveBookingExists: 1,
		metrics.BookingRejected:            1,
		metrics.BookingFailed:              1,
	} {
		if got := testutil.ToFloat64(m.BookingAttempts.WithLabelValues(result)); got != want {
			t.Errorf("cinema_booking_attempts_total{result=%q} = %v, want %v", result, got, want)
		}
	}
}

func TestPayCountsPaymentsThatReachedAProvider(t *testing.T) {
	t.Parallel()

	pending := paidResult()
	pending.Payment.Status = domain.PaymentPending
	m := newTestMetrics()
	for _, stub := range []*stubPayments{
		{result: paidResult()},
		{result: pending},
		{err: domain.PaymentDeclined("card_declined")},
		{err: domain.Unavailable(domain.CodePaymentProviderUnavailable, "later")},
		{err: domain.Conflict(domain.CodePaymentRefunded, "refunded")},
		// Turned away before a payment started: not counted.
		{err: domain.Gone(domain.CodeBookingExpired, "expired")},
		{err: domain.Invalid(domain.CodePaymentMethodUnavailable, "unknown method")},
		{err: errors.New("database is down")},
	} {
		h := NewPayments(stub, stubMethods{}, "USD", m, slog.New(slog.DiscardHandler))
		req := asSampleUser(t, http.MethodPost, payPath, `{"payment_method":"local","payment_token":"tok_success"}`)
		req.SetPathValue("bookingID", sampleBooking.ID.String())
		h.Pay(httptest.NewRecorder(), req)
	}

	for result, want := range map[string]float64{
		metrics.PaymentSucceeded:           1,
		metrics.PaymentPending:             1,
		metrics.PaymentDeclined:            1,
		metrics.PaymentProviderUnavailable: 1,
		metrics.PaymentRefunded:            1,
	} {
		if got := testutil.ToFloat64(m.Payments.WithLabelValues("local", result)); got != want {
			t.Errorf("cinema_payments_total{provider=local,result=%q} = %v, want %v", result, got, want)
		}
	}
	if n := testutil.CollectAndCount(m.Payments); n != 5 {
		t.Errorf("%d payment series, want 5: requests turned away before a payment must not create series", n)
	}
}
