package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// stubCatalog records the arguments it receives and returns canned results.
type stubCatalog struct {
	afterID  int64
	limit    int
	schedule catalog.ScheduleQuery
	page     catalog.MoviePage
	details  catalog.MovieDetails
	sched    catalog.Schedule
	showtime domain.Showtime
	seatMap  catalog.SeatMap
	err      error
}

func (s *stubCatalog) ListMovies(_ context.Context, afterID int64, limit int) (catalog.MoviePage, error) {
	s.afterID, s.limit = afterID, limit
	return s.page, s.err
}

func (s *stubCatalog) GetMovie(context.Context, int64) (catalog.MovieDetails, error) {
	return s.details, s.err
}

func (s *stubCatalog) Schedule(_ context.Context, q catalog.ScheduleQuery) (catalog.Schedule, error) {
	s.schedule = q
	return s.sched, s.err
}

func (s *stubCatalog) GetShowtime(context.Context, int64) (domain.Showtime, error) {
	return s.showtime, s.err
}

func (s *stubCatalog) SeatMap(context.Context, int64) (catalog.SeatMap, error) {
	return s.seatMap, s.err
}

func serveCatalog(t *testing.T, svc CatalogService, target string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewCatalog(svc, "USD", slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/movies", h.ListMovies)
	mux.HandleFunc("GET /v1/movies/{movieID}", h.GetMovie)
	mux.HandleFunc("GET /v1/showtimes", h.Schedule)
	mux.HandleFunc("GET /v1/showtimes/{showtimeID}", h.GetShowtime)
	mux.HandleFunc("GET /v1/showtimes/{showtimeID}/seats", h.SeatMap)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) problem.Problem {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, problem.ContentType)
	}
	p := decode[problem.Problem](t, rec)
	if p.Code != code {
		t.Errorf("code = %q, want %q", p.Code, code)
	}
	return p
}

var sampleShowtime = domain.Showtime{
	ID:             11,
	Movie:          domain.MovieSummary{ID: 1, Title: "Dune", DurationMin: 155, AgeRating: "PG-13"},
	Hall:           domain.Hall{ID: 2, Name: "Hall 2"},
	StartsAt:       time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC),
	EndsAt:         time.Date(2026, 10, 1, 21, 50, 0, 0, time.UTC),
	BasePriceCents: 1200,
	Status:         domain.ShowtimeScheduled,
	SeatsAvailable: 90,
	SeatsTotal:     100,
}

func TestListMoviesPassesPagingAndReturnsCursor(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{page: catalog.MoviePage{
		Movies:      []domain.Movie{{ID: 3, Title: "A", DurationMin: 90}, {ID: 4, Title: "B", DurationMin: 100}},
		NextAfterID: 4,
	}}
	rec := serveCatalog(t, svc, "/v1/movies?limit=2&cursor="+encodeCursor(2))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.afterID != 2 || svc.limit != 2 {
		t.Errorf("service got afterID=%d limit=%d, want 2/2", svc.afterID, svc.limit)
	}
	body := decode[struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}](t, rec)
	if len(body.Items) != 2 || body.Items[0]["title"] != "A" {
		t.Errorf("items = %v", body.Items)
	}
	if id, err := decodeCursor(body.NextCursor); err != nil || id != 4 {
		t.Errorf("next_cursor %q decodes to %d, %v; want 4", body.NextCursor, id, err)
	}
}

func TestListMoviesLastPageHasEmptyListNotNull(t *testing.T) {
	t.Parallel()

	rec := serveCatalog(t, &stubCatalog{}, "/v1/movies")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Errorf("body = %s, want {\"items\":[]}", got)
	}
}

func TestListMoviesRejectsBadParameters(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"/v1/movies?limit=0":                     {"limit"},
		"/v1/movies?limit=101":                   {"limit"},
		"/v1/movies?limit=ten":                   {"limit"},
		"/v1/movies?cursor=!!!":                  {"cursor"},
		"/v1/movies?cursor=" + encodeCursor(-5):  {"cursor"},
		"/v1/movies?limit=-1&cursor=not-base-64": {"limit", "cursor"},
	}
	for target, fields := range tests {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			svc := &stubCatalog{}
			p := assertProblem(t, serveCatalog(t, svc, target), http.StatusBadRequest, problem.CodeValidationFailed)
			if len(p.Errors) != len(fields) {
				t.Fatalf("errors = %+v, want fields %v", p.Errors, fields)
			}
			for i, f := range fields {
				if p.Errors[i].Field != f {
					t.Errorf("errors[%d].field = %q, want %q", i, p.Errors[i].Field, f)
				}
			}
		})
	}
}

func TestGetMovie(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{details: catalog.MovieDetails{
		Movie:    domain.Movie{ID: 1, Title: "Dune", DurationMin: 155},
		Upcoming: []domain.Showtime{sampleShowtime},
	}}
	rec := serveCatalog(t, svc, "/v1/movies/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	body := decode[map[string]any](t, rec)
	if body["title"] != "Dune" {
		t.Errorf("title = %v", body["title"])
	}
	upcoming, _ := body["upcoming_showtimes"].([]any)
	if len(upcoming) != 1 {
		t.Fatalf("upcoming_showtimes = %v", body["upcoming_showtimes"])
	}
	first, _ := upcoming[0].(map[string]any)
	if first["currency"] != "USD" || first["starts_at"] != "2026-10-01T19:00:00Z" {
		t.Errorf("showtime = %v", first)
	}
}

func TestGetMovieErrors(t *testing.T) {
	t.Parallel()

	notFound := &stubCatalog{err: domain.NotFound(domain.CodeMovieNotFound, "movie 9 not found")}
	p := assertProblem(t, serveCatalog(t, notFound, "/v1/movies/9"), http.StatusNotFound, domain.CodeMovieNotFound)
	if p.Detail != "movie 9 not found" || p.Instance != "/v1/movies/9" {
		t.Errorf("problem = %+v", p)
	}

	assertProblem(t, serveCatalog(t, &stubCatalog{}, "/v1/movies/abc"), http.StatusBadRequest,
		problem.CodeValidationFailed)
	assertProblem(t, serveCatalog(t, &stubCatalog{}, "/v1/movies/0"), http.StatusBadRequest,
		problem.CodeValidationFailed)

	broken := &stubCatalog{err: errors.New("connection reset by peer")}
	rec := serveCatalog(t, broken, "/v1/movies/1")
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
	if strings.Contains(rec.Body.String(), "connection reset") {
		t.Errorf("500 body leaks the internal error: %s", rec.Body.String())
	}
}

func TestSchedule(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{sched: catalog.Schedule{
		Day:       time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Showtimes: []domain.Showtime{sampleShowtime},
	}}
	rec := serveCatalog(t, svc, "/v1/showtimes?date=2026-10-01&movie_id=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	if svc.schedule.MovieID != 1 || svc.schedule.Day.Format(time.DateOnly) != "2026-10-01" {
		t.Errorf("service got %+v", svc.schedule)
	}
	body := decode[map[string]any](t, rec)
	if body["date"] != "2026-10-01" {
		t.Errorf("date = %v", body["date"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	item, _ := items[0].(map[string]any)
	for k, v := range map[string]any{"seats_available": 90.0, "seats_total": 100.0, "base_price_cents": 1200.0} {
		if item[k] != v {
			t.Errorf("%s = %v, want %v", k, item[k], v)
		}
	}
}

func TestScheduleWithoutDateAsksForToday(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{}
	if rec := serveCatalog(t, svc, "/v1/showtimes"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !svc.schedule.Day.IsZero() || svc.schedule.MovieID != 0 {
		t.Errorf("service got %+v, want the zero query", svc.schedule)
	}
}

func TestScheduleRejectsBadParameters(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		"/v1/showtimes?date=01-10-2026",
		"/v1/showtimes?date=2026-02-30",
		"/v1/showtimes?movie_id=x",
		"/v1/showtimes?movie_id=-3",
	} {
		assertProblem(t, serveCatalog(t, &stubCatalog{}, target), http.StatusBadRequest, problem.CodeValidationFailed)
	}
}

func TestGetShowtime(t *testing.T) {
	t.Parallel()

	rec := serveCatalog(t, &stubCatalog{showtime: sampleShowtime}, "/v1/showtimes/11")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	if body["id"] != 11.0 || body["status"] != "scheduled" {
		t.Errorf("body = %v", body)
	}

	notFound := &stubCatalog{err: domain.NotFound(domain.CodeShowtimeNotFound, "showtime 5 not found")}
	assertProblem(t, serveCatalog(t, notFound, "/v1/showtimes/5"), http.StatusNotFound, domain.CodeShowtimeNotFound)
}

func TestSeatMap(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{seatMap: catalog.SeatMap{
		Showtime: sampleShowtime,
		Seats: []domain.ShowtimeSeat{
			{SeatID: 100, Row: "A", Number: 1, Type: domain.SeatStandard, PriceCents: 1200, Status: domain.SeatAvailable},
			{SeatID: 101, Row: "A", Number: 2, Type: domain.SeatVIP, PriceCents: 1800, Status: domain.SeatHeld},
		},
		Summary: catalog.SeatSummary{Available: 1, Held: 1, Total: 2},
	}}
	rec := serveCatalog(t, svc, "/v1/showtimes/11/seats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	body := decode[struct {
		ShowtimeID int64            `json:"showtime_id"`
		Currency   string           `json:"currency"`
		Summary    map[string]int   `json:"summary"`
		Seats      []map[string]any `json:"seats"`
	}](t, rec)
	if body.ShowtimeID != 11 || body.Currency != "USD" {
		t.Errorf("showtime_id/currency = %d/%s", body.ShowtimeID, body.Currency)
	}
	if body.Summary["available"] != 1 || body.Summary["held"] != 1 || body.Summary["total"] != 2 {
		t.Errorf("summary = %v", body.Summary)
	}
	if len(body.Seats) != 2 || body.Seats[1]["status"] != "held" || body.Seats[1]["type"] != "vip" {
		t.Errorf("seats = %v", body.Seats)
	}
}
