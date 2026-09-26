package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

var sampleBooking = domain.Booking{
	ID:     uuid.MustParse("01920000-0000-7000-8000-0000000000b1"),
	UserID: sampleUser.ID,
	Showtime: domain.ShowtimeRef{
		ID:       11,
		Movie:    domain.MovieSummary{ID: 1, Title: "Dune", DurationMin: 155, AgeRating: "PG-13"},
		Hall:     domain.Hall{ID: 2, Name: "Hall 2"},
		StartsAt: time.Date(2030, 1, 10, 19, 0, 0, 0, time.FixedZone("GST", 4*3600)),
		Status:   domain.ShowtimeScheduled,
	},
	Status: domain.BookingPending,
	Seats: []domain.BookedSeat{
		{SeatID: 7, Row: "A", Number: 7, Type: domain.SeatStandard, PriceCents: 1200},
		{SeatID: 8, Row: "A", Number: 8, Type: domain.SeatVIP, PriceCents: 1800},
	},
	TotalCents: 3000,
	ExpiresAt:  time.Date(2030, 1, 10, 10, 15, 0, 0, time.FixedZone("GST", 4*3600)),
	CreatedAt:  time.Date(2030, 1, 10, 10, 0, 0, 0, time.UTC),
}

// stubBookings records the arguments it receives and returns canned results.
type stubBookings struct {
	userID   uuid.UUID
	id       uuid.UUID
	beforeID uuid.UUID
	limit    int
	input    domain.NewBooking
	booking  domain.Booking
	page     booking.Page
	err      error
}

func (s *stubBookings) Create(_ context.Context, userID uuid.UUID, nb domain.NewBooking) (domain.Booking, error) {
	s.userID, s.input = userID, nb
	return s.booking, s.err
}

func (s *stubBookings) Get(_ context.Context, userID, id uuid.UUID) (domain.Booking, error) {
	s.userID, s.id = userID, id
	return s.booking, s.err
}

func (s *stubBookings) List(_ context.Context, userID, beforeID uuid.UUID, limit int) (booking.Page, error) {
	s.userID, s.beforeID, s.limit = userID, beforeID, limit
	return s.page, s.err
}

func (s *stubBookings) Cancel(_ context.Context, userID, id uuid.UUID) error {
	s.userID, s.id = userID, id
	return s.err
}

// serveBookings sends a request as sampleUser, or anonymously when anonymous is set.
func serveBookings(t *testing.T, svc BookingService, method, target, body string, anonymous bool) *httptest.ResponseRecorder {
	t.Helper()
	h := NewBookings(svc, "USD", newTestMetrics(), slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bookings", h.Create)
	mux.HandleFunc("GET /v1/bookings", h.List)
	mux.HandleFunc("GET /v1/bookings/{bookingID}", h.Get)
	mux.HandleFunc("DELETE /v1/bookings/{bookingID}", h.Cancel)

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

func TestCreateBooking(t *testing.T) {
	t.Parallel()

	svc := &stubBookings{booking: sampleBooking}
	rec := serveBookings(t, svc, http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":[8,7]}`, false)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/bookings/"+sampleBooking.ID.String() {
		t.Errorf("Location = %q", loc)
	}
	if svc.userID != sampleUser.ID || svc.input.ShowtimeID != 11 || len(svc.input.SeatIDs) != 2 || svc.input.SeatIDs[0] != 8 {
		t.Errorf("service got user %s input %+v", svc.userID, svc.input)
	}

	body := decode[map[string]any](t, rec)
	want := map[string]any{
		"id":          sampleBooking.ID.String(),
		"status":      "pending",
		"total_cents": 3000.0,
		"currency":    "USD",
		"expires_at":  "2030-01-10T06:15:00Z", // the booking's own timestamps are UTC
		"created_at":  "2030-01-10T10:00:00Z",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	if _, ok := body["paid_at"]; ok {
		t.Error("paid_at is set on an unpaid booking")
	}
	showtime, _ := body["showtime"].(map[string]any)
	if showtime["starts_at"] != "2030-01-10T19:00:00+04:00" || showtime["id"] != 11.0 {
		t.Errorf("showtime = %v (the start keeps the cinema offset)", showtime)
	}
	seats, _ := body["seats"].([]any)
	if len(seats) != 2 {
		t.Fatalf("seats = %v", body["seats"])
	}
	if seat, _ := seats[1].(map[string]any); seat["id"] != 8.0 || seat["type"] != "vip" || seat["price_cents"] != 1800.0 {
		t.Errorf("seat = %v", seat)
	}
}

func TestCreateBookingErrors(t *testing.T) {
	t.Parallel()

	rec := serveBookings(t, &stubBookings{err: domain.SeatsUnavailable([]int64{7, 8})},
		http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":[7,8]}`, false)
	p := assertProblem(t, rec, http.StatusConflict, domain.CodeSeatUnavailable)
	if len(p.UnavailableSeatIDs) != 2 || p.UnavailableSeatIDs[0] != 7 {
		t.Errorf("unavailable seats = %v", p.UnavailableSeatIDs)
	}

	rec = serveBookings(t, &stubBookings{err: domain.Busy(domain.CodeSeatBusy, "locked")},
		http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":[7]}`, false)
	assertProblem(t, rec, http.StatusConflict, domain.CodeSeatBusy)
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}

	rec = serveBookings(t, &stubBookings{err: domain.UnknownSeats(11, []int64{99})},
		http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":[99]}`, false)
	assertProblem(t, rec, http.StatusUnprocessableEntity, domain.CodeUnknownSeat)

	rec = serveBookings(t, &stubBookings{}, http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":"7"}`, false)
	assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)

	rec = serveBookings(t, &stubBookings{}, http.MethodPost, "/v1/bookings", `{"showtime_id":11,"seat_ids":[7]}`, true)
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
}

func TestListBookings(t *testing.T) {
	t.Parallel()

	next := uuid.MustParse("01920000-0000-7000-8000-0000000000a0")
	svc := &stubBookings{page: booking.Page{Bookings: []domain.Booking{sampleBooking}, NextBeforeID: next}}
	rec := serveBookings(t, svc, http.MethodGet, "/v1/bookings?limit=1", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.limit != 1 || svc.beforeID != (uuid.UUID{}) || svc.userID != sampleUser.ID {
		t.Errorf("service got limit %d before %s user %s", svc.limit, svc.beforeID, svc.userID)
	}
	body := decode[struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}](t, rec)
	if len(body.Items) != 1 || body.NextCursor == "" {
		t.Fatalf("body = %+v", body)
	}

	// The cursor leads to the next page.
	serveBookings(t, svc, http.MethodGet, "/v1/bookings?cursor="+body.NextCursor, "", false)
	if svc.beforeID != next {
		t.Errorf("cursor decoded to %s, want %s", svc.beforeID, next)
	}

	// An empty last page encodes as [] and has no cursor.
	rec = serveBookings(t, &stubBookings{}, http.MethodGet, "/v1/bookings", "", false)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Errorf("empty page = %s", got)
	}

	rec = serveBookings(t, svc, http.MethodGet, "/v1/bookings?limit=0&cursor=%21", "", false)
	p := assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
	if len(p.Errors) != 2 {
		t.Errorf("errors = %+v, want limit and cursor", p.Errors)
	}
}

func TestGetBooking(t *testing.T) {
	t.Parallel()

	svc := &stubBookings{booking: sampleBooking}
	rec := serveBookings(t, svc, http.MethodGet, "/v1/bookings/"+sampleBooking.ID.String(), "", false)
	if rec.Code != http.StatusOK || svc.id != sampleBooking.ID || svc.userID != sampleUser.ID {
		t.Errorf("status %d, service got id %s user %s", rec.Code, svc.id, svc.userID)
	}

	rec = serveBookings(t, svc, http.MethodGet, "/v1/bookings/42", "", false)
	p := assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Field != "bookingID" {
		t.Errorf("errors = %+v", p.Errors)
	}

	rec = serveBookings(t, &stubBookings{err: domain.BookingNotFound(sampleBooking.ID)},
		http.MethodGet, "/v1/bookings/"+sampleBooking.ID.String(), "", false)
	assertProblem(t, rec, http.StatusNotFound, domain.CodeBookingNotFound)
}

func TestCancelBooking(t *testing.T) {
	t.Parallel()

	svc := &stubBookings{}
	rec := serveBookings(t, svc, http.MethodDelete, "/v1/bookings/"+sampleBooking.ID.String(), "", false)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("status %d body %q, want 204 without a body", rec.Code, rec.Body.String())
	}
	if svc.id != sampleBooking.ID || svc.userID != sampleUser.ID {
		t.Errorf("service got id %s user %s", svc.id, svc.userID)
	}

	rec = serveBookings(t, &stubBookings{err: domain.Conflict(domain.CodeBookingNotCancelable, "paid")},
		http.MethodDelete, "/v1/bookings/"+sampleBooking.ID.String(), "", false)
	assertProblem(t, rec, http.StatusConflict, domain.CodeBookingNotCancelable)

	rec = serveBookings(t, svc, http.MethodDelete, "/v1/bookings/not-a-uuid", "", false)
	assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
}
