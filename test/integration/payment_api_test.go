//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"
)

// TestPaymentAPI drives the payment endpoints through the real router, services, database, and local provider.
func TestPaymentAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	api := newAPIServer(t, f)
	ann, bob, cat := api.signUp("ann@example.com"), api.signUp("bob@example.com"), api.signUp("cat@example.com")

	// Only enabled providers are offered; "retired" is registered but switched off.
	methods := api.do(http.MethodGet, "/v1/payment-methods", "", nil)
	items, _ := methods.body["items"].([]any)
	if methods.status != http.StatusOK || len(items) != 2 {
		t.Fatalf("payment methods = %d %v", methods.status, methods.body)
	}
	if first, _ := items[0].(map[string]any); first["id"] != "local" || first["name"] != "Test card" {
		t.Errorf("first payment method = %v", first)
	}

	book := func(token, label string) string {
		t.Helper()
		r := api.do(http.MethodPost, "/v1/bookings", token, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats[label]}})
		if r.status != http.StatusCreated {
			t.Fatalf("book %s = %d %v", label, r.status, r.body)
		}
		return r.header.Get("Location")
	}
	pay := func(token, location, method, paymentToken string) apiResponse {
		t.Helper()
		return api.doWith(http.MethodPost, location+"/payments", token,
			map[string]string{"payment_method": method, "payment_token": paymentToken}, withKey(""))
	}

	annBooking := book(ann, "A1")
	for _, tt := range []struct {
		name   string
		token  string
		method string
		pay    string
		status int
		code   string
	}{
		{name: "no token", method: "local", pay: "tok_success", status: http.StatusUnauthorized, code: "UNAUTHENTICATED"},
		{name: "another user", token: bob, method: "local", pay: "tok_success", status: http.StatusNotFound, code: "BOOKING_NOT_FOUND"},
		{name: "unknown method", token: ann, method: "cash", pay: "tok_success", status: http.StatusUnprocessableEntity, code: "PAYMENT_METHOD_UNAVAILABLE"},
		{name: "disabled method", token: ann, method: "retired", pay: "tok_success", status: http.StatusUnprocessableEntity, code: "PAYMENT_METHOD_UNAVAILABLE"},
		{name: "no token given", token: ann, method: "local", status: http.StatusBadRequest, code: "VALIDATION_FAILED"},
		{name: "declined", token: ann, method: "local", pay: "tok_declined", status: http.StatusPaymentRequired, code: "PAYMENT_DECLINED"},
	} {
		r := pay(tt.token, annBooking, tt.method, tt.pay)
		if r.status != tt.status || r.code() != tt.code {
			t.Errorf("%s: %d %v, want %d %s", tt.name, r.status, r.body, tt.status, tt.code)
		}
		if tt.code == "PAYMENT_DECLINED" && r.body["decline_code"] != "card_declined" {
			t.Errorf("%s: decline_code = %v", tt.name, r.body["decline_code"])
		}
	}

	paid := pay(ann, annBooking, "local", "tok_success")
	payment, _ := paid.body["payment"].(map[string]any)
	booking, _ := paid.body["booking"].(map[string]any)
	if paid.status != http.StatusOK || payment["status"] != "succeeded" || payment["amount_cents"] != 1000.0 ||
		payment["payment_method"] != "local" || booking["status"] != "paid" || booking["paid_at"] == nil {
		t.Fatalf("pay = %d %v", paid.status, paid.body)
	}
	if statuses := api.seatStatuses(st.ID); statuses[float64(seats["A1"])] != "sold" {
		t.Errorf("seat map after payment = %v", statuses)
	}
	if r := pay(ann, annBooking, "local", "tok_success"); r.status != http.StatusConflict || r.code() != "BOOKING_ALREADY_PAID" {
		t.Errorf("second payment = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodDelete, annBooking, ann, nil); r.status != http.StatusConflict || r.code() != "BOOKING_NOT_CANCELABLE" {
		t.Errorf("cancel after payment = %d %v", r.status, r.body)
	}

	// No answer from the provider: accepted, and the booking to follow is named.
	bobBooking := book(bob, "A2")
	start := time.Now()
	accepted := pay(bob, bobBooking, "local", "tok_timeout")
	if accepted.status != http.StatusAccepted || accepted.header.Get("Location") != bobBooking || time.Since(start) < testPaymentTimeout {
		t.Errorf("pay without an answer = %d %v (Location %q)", accepted.status, accepted.body, accepted.header.Get("Location"))
	}
	if r := api.do(http.MethodGet, bobBooking, bob, nil); r.body["status"] != "processing" {
		t.Errorf("booking while the payment is in flight = %v", r.body)
	}
	if r := pay(bob, bobBooking, "local", "tok_success"); r.status != http.StatusConflict || r.code() != "PAYMENT_IN_PROGRESS" {
		t.Errorf("second payment while in flight = %d %v", r.status, r.body)
	}

	// The provider refuses the request: nothing charged, try again later.
	catBooking := book(cat, "B1")
	if r := pay(cat, catBooking, "local", "tok_unavailable"); r.status != http.StatusServiceUnavailable ||
		r.code() != "PAYMENT_PROVIDER_UNAVAILABLE" || r.header.Get("Retry-After") != "5" {
		t.Errorf("unavailable provider = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodGet, catBooking, cat, nil); r.body["status"] != "pending" {
		t.Errorf("booking after the refusal = %v", r.body)
	}

	// The hold has run out; no worker has been there yet.
	exec(t, f.pool, `UPDATE bookings SET expires_at = now() - interval '1 second' WHERE status = 'pending'`)
	if r := pay(cat, catBooking, "local", "tok_success"); r.status != http.StatusGone || r.code() != "BOOKING_EXPIRED" {
		t.Errorf("pay after the deadline = %d %v", r.status, r.body)
	}
}
