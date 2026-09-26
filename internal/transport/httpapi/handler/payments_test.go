package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// stubPayments records the arguments it receives and returns canned results.
type stubPayments struct {
	userID    uuid.UUID
	bookingID uuid.UUID
	input     domain.NewPayment
	result    booking.PayResult
	err       error
}

func (s *stubPayments) Pay(_ context.Context, userID, bookingID uuid.UUID, np domain.NewPayment) (booking.PayResult, error) {
	s.userID, s.bookingID, s.input = userID, bookingID, np
	return s.result, s.err
}

type stubMethods []payment.Method

func (s stubMethods) Methods() []payment.Method { return s }

func paidResult() booking.PayResult {
	b := sampleBooking
	b.Status = domain.BookingPaid
	b.PaidAt = time.Date(2030, 1, 10, 10, 5, 0, 0, time.UTC)
	return booking.PayResult{
		Payment: domain.Payment{
			ID: uuid.MustParse("01920000-0000-7000-8000-0000000000c1"), BookingID: b.ID, Provider: "local",
			AmountCents: 3000, Status: domain.PaymentSucceeded, ProviderRef: "local_x",
			CreatedAt: time.Date(2030, 1, 10, 14, 5, 0, 0, time.FixedZone("GST", 4*3600)),
		},
		Booking: b,
	}
}

// servePayments sends a request as sampleUser, or anonymously when anonymous is set.
func servePayments(t *testing.T, svc PaymentService, method, target, body string, anonymous bool) *httptest.ResponseRecorder {
	t.Helper()
	h := NewPayments(svc, stubMethods{{ID: "local", Name: "Test card"}}, "USD", slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/payment-methods", h.ListMethods)
	mux.HandleFunc("POST /v1/bookings/{bookingID}/payments", h.Pay)

	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if !anonymous {
		req = req.WithContext(principal.NewContext(req.Context(),
			domain.Principal{UserID: sampleUser.ID, Role: domain.RoleCustomer}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var payPath = "/v1/bookings/" + sampleBooking.ID.String() + "/payments"

func TestListPaymentMethods(t *testing.T) {
	t.Parallel()

	rec := servePayments(t, &stubPayments{}, http.MethodGet, "/v1/payment-methods", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[{"id":"local","name":"Test card"}]}` {
		t.Errorf("body = %s", got)
	}

	// No provider enabled: an empty list, not null.
	h := NewPayments(&stubPayments{}, stubMethods{}, "USD", slog.New(slog.DiscardHandler))
	rec = httptest.NewRecorder()
	h.ListMethods(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/payment-methods", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Errorf("empty list = %s", got)
	}
}

func TestPay(t *testing.T) {
	t.Parallel()

	svc := &stubPayments{result: paidResult()}
	rec := servePayments(t, svc, http.MethodPost, payPath, `{"payment_method":"local","payment_token":"tok_success"}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Errorf("Location = %q on a settled payment", rec.Header().Get("Location"))
	}
	if svc.userID != sampleUser.ID || svc.bookingID != sampleBooking.ID ||
		svc.input != (domain.NewPayment{Method: "local", Token: "tok_success"}) {
		t.Errorf("service got user %s booking %s input %+v", svc.userID, svc.bookingID, svc.input)
	}

	body := decode[struct {
		Payment map[string]any `json:"payment"`
		Booking map[string]any `json:"booking"`
	}](t, rec)
	want := map[string]any{
		"id": "01920000-0000-7000-8000-0000000000c1", "status": "succeeded", "payment_method": "local",
		"amount_cents": 3000.0, "currency": "USD", "created_at": "2030-01-10T10:05:00Z",
	}
	for k, v := range want {
		if body.Payment[k] != v {
			t.Errorf("payment.%s = %v, want %v", k, body.Payment[k], v)
		}
	}
	if _, ok := body.Payment["failure_reason"]; ok {
		t.Error("failure_reason is set on a succeeded payment")
	}
	if _, ok := body.Payment["provider_ref"]; ok {
		t.Error("the provider's reference leaks to the client")
	}
	if body.Booking["status"] != "paid" || body.Booking["paid_at"] != "2030-01-10T10:05:00Z" ||
		body.Booking["id"] != sampleBooking.ID.String() {
		t.Errorf("booking = %v", body.Booking)
	}
}

// TestPayInFlight: a payment whose outcome is not known yet is accepted, and Location names the booking to follow.
func TestPayInFlight(t *testing.T) {
	t.Parallel()

	res := paidResult()
	res.Payment.Status, res.Payment.ProviderRef = domain.PaymentPending, ""
	res.Booking.Status, res.Booking.PaidAt = domain.BookingProcessing, time.Time{}
	rec := servePayments(t, &stubPayments{result: res}, http.MethodPost, payPath,
		`{"payment_method":"local","payment_token":"tok_timeout"}`, false)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/bookings/"+sampleBooking.ID.String() {
		t.Errorf("Location = %q", loc)
	}
	body := decode[struct {
		Payment map[string]any `json:"payment"`
		Booking map[string]any `json:"booking"`
	}](t, rec)
	if body.Payment["status"] != "pending" || body.Booking["status"] != "processing" {
		t.Errorf("body = %+v", body)
	}
}

func TestPayErrors(t *testing.T) {
	t.Parallel()

	good := `{"payment_method":"local","payment_token":"tok_success"}`
	tests := []struct {
		name   string
		err    error
		path   string
		body   string
		anon   bool
		status int
		code   string
	}{
		{name: "declined", err: domain.PaymentDeclined("card_declined"), status: http.StatusPaymentRequired, code: domain.CodePaymentDeclined},
		{name: "expired", err: domain.ExpiredBooking(sampleBooking.ID), status: http.StatusGone, code: domain.CodeBookingExpired},
		{
			name: "in progress", err: domain.Conflict(domain.CodePaymentInProgress, "busy"),
			status: http.StatusConflict, code: domain.CodePaymentInProgress,
		},
		{
			name: "method unavailable", err: domain.Invalid(domain.CodePaymentMethodUnavailable, "no"),
			status: http.StatusUnprocessableEntity, code: domain.CodePaymentMethodUnavailable,
		},
		{
			name: "provider unavailable", err: domain.Unavailable(domain.CodePaymentProviderUnavailable, "later"),
			status: http.StatusServiceUnavailable, code: domain.CodePaymentProviderUnavailable,
		},
		{name: "malformed booking id", path: "/v1/bookings/42/payments", status: http.StatusBadRequest, code: problem.CodeValidationFailed},
		{name: "unknown field", body: `{"payment_method":"local","card":"4242"}`, status: http.StatusBadRequest, code: problem.CodeMalformedBody},
		{name: "not routed behind authentication", anon: true, status: http.StatusInternalServerError, code: problem.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, body := payPath, good
			if tt.path != "" {
				path = tt.path
			}
			if tt.body != "" {
				body = tt.body
			}
			rec := servePayments(t, &stubPayments{err: tt.err}, http.MethodPost, path, body, tt.anon)
			p := assertProblem(t, rec, tt.status, tt.code)
			if tt.code == domain.CodePaymentDeclined && p.DeclineCode != "card_declined" {
				t.Errorf("decline_code = %q", p.DeclineCode)
			}
			if tt.code == domain.CodePaymentProviderUnavailable && rec.Header().Get("Retry-After") != "5" {
				t.Errorf("Retry-After = %q, want 5", rec.Header().Get("Retry-After"))
			}
		})
	}
}

// TestPayLogsServerSideFailures: an unexpected failure is an error; a payment provider that refuses requests is
// worth a warning only, because this service works as intended.
func TestPayLogsServerSideFailures(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		err  error
		want string
	}{
		{err: domain.Unavailable(domain.CodePaymentProviderUnavailable, "later"), want: `level=WARN msg="request failed" error=later`},
		{err: errors.New("database is down"), want: `level=ERROR msg="request failed" error="database is down"`},
	} {
		var logs strings.Builder
		h := NewPayments(&stubPayments{err: tt.err}, stubMethods{}, "USD", slog.New(slog.NewTextHandler(&logs, nil)))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, payPath,
			strings.NewReader(`{"payment_method":"local","payment_token":"tok_success"}`))
		req.SetPathValue("bookingID", sampleBooking.ID.String())
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(principal.NewContext(req.Context(), domain.Principal{UserID: sampleUser.ID, Role: domain.RoleCustomer}))
		h.Pay(httptest.NewRecorder(), req)
		if !strings.Contains(logs.String(), tt.want) {
			t.Errorf("log = %q, want %q", logs.String(), tt.want)
		}
	}
}
