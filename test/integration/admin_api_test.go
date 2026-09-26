//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/auth"
)

// signInAdmin creates the admin account the way cmd/seed does and returns an access token for it.
func (c apiClient) signInAdmin(f *fixture) string {
	c.t.Helper()
	authSvc, err := auth.New(postgres.NewUsers(f.pool), bcrypt.MinCost)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, _, err := authSvc.EnsureAdmin(c.t.Context(), "admin@cinema.local", "admin password"); err != nil {
		c.t.Fatal(err)
	}
	r := c.do(http.MethodPost, "/v1/auth/login", "", map[string]string{"email": "admin@cinema.local", "password": "admin password"})
	token, _ := r.body["access_token"].(string)
	if token == "" {
		c.t.Fatalf("admin login = %d %v", r.status, r.body)
	}
	return token
}

// TestAdminAPI creates a hall and schedules showtimes in it through the real router, services, database, and
// Redis, and books a seat of the new showtime.
func TestAdminAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	adminToken := api.signInAdmin(f)
	customer := api.signUp("ann@example.com")

	// A hall with a standard and a VIP row; a row without a type holds standard seats.
	hallBody := map[string]any{"name": "Hall 9", "rows": []map[string]any{
		{"label": "A", "seats": 3},
		{"label": "B", "seats": 2, "type": "vip"},
	}}
	if r := api.do(http.MethodPost, "/v1/admin/halls", customer, hallBody); r.status != http.StatusForbidden {
		t.Errorf("hall as a customer = %d %v", r.status, r.body)
	}
	hall := api.do(http.MethodPost, "/v1/admin/halls", adminToken, hallBody)
	hallID, _ := hall.body["id"].(float64)
	seats, _ := hall.body["seats"].([]any)
	if hall.status != http.StatusCreated || hallID == 0 || hall.body["seats_total"] != 5.0 || len(seats) != 5 {
		t.Fatalf("create hall = %d %v", hall.status, hall.body)
	}
	if last, _ := seats[4].(map[string]any); last["row"] != "B" || last["number"] != 2.0 || last["type"] != "vip" {
		t.Errorf("last seat = %v, want B2 vip", last)
	}
	if first, _ := seats[0].(map[string]any); first["type"] != "standard" {
		t.Errorf("first seat = %v, want standard", first)
	}
	if r := api.do(http.MethodPost, "/v1/admin/halls", adminToken, hallBody); r.status != http.StatusConflict || r.code() != "HALL_NAME_TAKEN" {
		t.Errorf("duplicate hall = %d %v", r.status, r.body)
	}
	invalid := api.do(http.MethodPost, "/v1/admin/halls", adminToken, map[string]any{
		"name": " ", "rows": []map[string]any{{"label": "a", "seats": 0}, {"label": "a", "seats": 1, "type": "balcony"}},
	})
	if errs, _ := invalid.body["errors"].([]any); invalid.status != http.StatusBadRequest || len(errs) != 5 {
		t.Errorf("invalid hall = %d %v, want name, rows[0].label, rows[0].seats, rows[1].label, rows[1].type", invalid.status, invalid.body)
	}

	// Cache the schedule of the day before the showtime exists: the new showtime must still show up at once,
	// although the cached schedule lives for a minute.
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	day := start.Format(time.DateOnly)
	if r := api.do(http.MethodGet, "/v1/showtimes?date="+day, "", nil); r.status != http.StatusOK || r.body["items"] == nil ||
		len(r.body["items"].([]any)) != 0 {
		t.Fatalf("schedule before = %d %v", r.status, r.body)
	}

	showtimeBody := map[string]any{
		"movie_id": f.dune.ID, "hall_id": hallID, "starts_at": start.Format(time.RFC3339), "base_price_cents": 1000,
	}
	created := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, showtimeBody)
	if created.status != http.StatusCreated || created.body["seats_available"] != 5.0 || created.body["status"] != "scheduled" {
		t.Fatalf("create showtime = %d %v", created.status, created.body)
	}
	// Dune runs 155 minutes, plus 15 minutes of cleaning.
	if want := start.Add(170 * time.Minute).Format(time.RFC3339); created.body["ends_at"] != want {
		t.Errorf("ends_at = %v, want %s", created.body["ends_at"], want)
	}
	location := created.header.Get("Location")
	id, _ := created.body["id"].(float64)
	if location != fmt.Sprintf("/v1/showtimes/%d", int64(id)) {
		t.Errorf("Location = %q", location)
	}
	sched := api.do(http.MethodGet, "/v1/showtimes?date="+day, "", nil)
	if items, _ := sched.body["items"].([]any); len(items) != 1 {
		t.Errorf("schedule after = %v, want the new showtime although the day was cached", sched.body)
	}

	// Every seat of the hall is on sale, VIP seats at 1.5 times the base price, and a customer can book one.
	seatMap := api.do(http.MethodGet, location+"/seats", "", nil)
	mapSeats, _ := seatMap.body["seats"].([]any)
	if seatMap.status != http.StatusOK || len(mapSeats) != 5 {
		t.Fatalf("seat map = %d %v", seatMap.status, seatMap.body)
	}
	vip, _ := mapSeats[3].(map[string]any)
	if vip["price_cents"] != 1500.0 || vip["status"] != "available" {
		t.Errorf("VIP seat = %v, want 1500 and available", vip)
	}
	book := api.do(http.MethodPost, "/v1/bookings", customer, map[string]any{"showtime_id": id, "seat_ids": []any{vip["id"]}})
	if book.status != http.StatusCreated || book.body["total_cents"] != 1500.0 {
		t.Errorf("booking in the new showtime = %d %v", book.status, book.body)
	}

	for _, tt := range []struct {
		name   string
		edit   func(map[string]any)
		status int
		code   string
	}{
		{name: "overlap", edit: func(b map[string]any) { b["starts_at"] = start.Add(2 * time.Hour).Format(time.RFC3339) },
			status: http.StatusConflict, code: "HALL_OVERLAP"},
		{name: "unknown movie", edit: func(b map[string]any) { b["movie_id"] = 999_999 }, status: http.StatusNotFound, code: "MOVIE_NOT_FOUND"},
		{name: "unknown hall", edit: func(b map[string]any) { b["hall_id"] = 999_999 }, status: http.StatusNotFound, code: "HALL_NOT_FOUND"},
		{name: "in the past", edit: func(b map[string]any) { b["starts_at"] = "2020-01-01T10:00:00Z" },
			status: http.StatusBadRequest, code: "VALIDATION_FAILED"},
		{name: "no offset", edit: func(b map[string]any) { b["starts_at"] = start.Format("2006-01-02T15:04:05") },
			status: http.StatusBadRequest, code: "VALIDATION_FAILED"},
	} {
		body := map[string]any{}
		for k, v := range showtimeBody {
			body[k] = v
		}
		tt.edit(body)
		if r := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, body); r.status != tt.status || r.code() != tt.code {
			t.Errorf("%s: create showtime = %d %v, want %d %s", tt.name, r.status, r.body, tt.status, tt.code)
		}
	}

	// The next showtime may start as soon as the previous one and its cleaning are over.
	showtimeBody["starts_at"] = start.Add(170 * time.Minute).Format(time.RFC3339)
	if r := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, showtimeBody); r.status != http.StatusCreated {
		t.Errorf("back-to-back showtime = %d %v", r.status, r.body)
	}
}

// TestAdminAPIConcurrentOverlappingShowtimes: admins who schedule overlapping showtimes in one hall at the same
// moment cannot both succeed. No application lock is involved; the exclusion constraint on the showtimes table
// decides.
func TestAdminAPIConcurrentOverlappingShowtimes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	adminToken := api.signInAdmin(f)

	const admins = 20
	start := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Hour)
	var (
		wg       sync.WaitGroup
		statuses [admins]int
		codes    [admins]string
	)
	for i := range admins {
		wg.Go(func() {
			// Each starts a few minutes apart, and every one overlaps every other.
			r := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, map[string]any{
				"movie_id": f.short.ID, "hall_id": f.hall.ID, "base_price_cents": 900,
				"starts_at": start.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			})
			statuses[i], codes[i] = r.status, r.code()
		})
	}
	wg.Wait()

	created := 0
	for i := range admins {
		switch {
		case statuses[i] == http.StatusCreated:
			created++
		case statuses[i] != http.StatusConflict || codes[i] != "HALL_OVERLAP":
			t.Errorf("request %d = %d %s, want 201 or 409 HALL_OVERLAP", i, statuses[i], codes[i])
		}
	}
	if created != 1 {
		t.Errorf("%d overlapping showtimes created, want exactly 1", created)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM showtimes WHERE hall_id = $1`, f.hall.ID); n != 1 {
		t.Errorf("%d showtimes stored in the hall, want 1", n)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM showtime_seats ss JOIN showtimes s ON s.id = ss.showtime_id WHERE s.hall_id = $1`,
		f.hall.ID); n != 5 {
		t.Errorf("%d inventory rows, want the 5 seats of the one showtime: a failed create leaves nothing behind", n)
	}
}
