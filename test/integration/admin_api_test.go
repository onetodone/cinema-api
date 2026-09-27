//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/onetodone/cinema-api/internal/domain"
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
		"audio_language": "eng",
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
		{name: "no audio", edit: func(b map[string]any) { delete(b, "audio_language") },
			status: http.StatusBadRequest, code: "VALIDATION_FAILED"},
		{name: "two-letter subtitles", edit: func(b map[string]any) { b["subtitle_language"] = "th" },
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
				"movie_id": f.short.ID, "hall_id": f.hall.ID, "base_price_cents": 900, "audio_language": "eng",
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

// TestAdminAPIGenresAndLanguages creates a movie with genres and showtimes in two language versions through the
// API, and finds them on every read that shows a movie or a showtime, including a booking.
func TestAdminAPIGenresAndLanguages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	adminToken := api.signInAdmin(f)
	customer := api.signUp("ann@example.com")

	genres := f.genres(t, "science_fiction", "thriller")
	drama := f.genres(t, "drama")[0]
	invalid := api.do(http.MethodPost, "/v1/admin/movies", adminToken, map[string]any{
		"title": "Noir", "duration_min": 90, "genre_ids": []int64{drama.ID, 0, drama.ID},
	})
	if fields := errorFields(invalid); invalid.status != http.StatusBadRequest || !slices.Equal(fields, []string{"genre_ids[1]", "genre_ids[2]"}) {
		t.Errorf("invalid genres = %d %v, want 400 on genre_ids[1] and genre_ids[2]", invalid.status, invalid.body)
	}
	unknown := api.do(http.MethodPost, "/v1/admin/movies", adminToken, map[string]any{
		"title": "Noir", "duration_min": 90, "genre_ids": []int64{drama.ID, 999_999},
	})
	if fields := errorFields(unknown); unknown.status != http.StatusBadRequest || !slices.Equal(fields, []string{"genre_ids[1]"}) {
		t.Errorf("unknown genre = %d %v, want 400 on genre_ids[1]", unknown.status, unknown.body)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM movies WHERE title = 'Noir'`); n != 0 {
		t.Errorf("%d movies stored by refused requests", n)
	}

	movie := api.do(http.MethodPost, "/v1/admin/movies", adminToken, map[string]any{
		"title": "Orbit of Glass", "duration_min": 142, "genre_ids": genreIDs(genres),
	})
	movieID, _ := movie.body["id"].(float64)
	wantGenres := genresJSON(genres...)
	if movie.status != http.StatusCreated || !reflect.DeepEqual(movie.body["genres"], wantGenres) {
		t.Fatalf("create movie = %d %v", movie.status, movie.body)
	}

	// The service trims and lower-cases the codes.
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	subtitled := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, map[string]any{
		"movie_id": movieID, "hall_id": f.hall.ID, "starts_at": start.Format(time.RFC3339), "base_price_cents": 1000,
		"audio_language": " ENG", "subtitle_language": "Tha ",
	})
	if subtitled.status != http.StatusCreated || subtitled.body["audio_language"] != "eng" || subtitled.body["subtitle_language"] != "tha" {
		t.Fatalf("create subtitled showtime = %d %v", subtitled.status, subtitled.body)
	}
	dubbed := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, map[string]any{
		"movie_id": movieID, "hall_id": f.hall2.ID, "starts_at": start.Format(time.RFC3339), "base_price_cents": 1000,
		"audio_language": "tha",
	})
	if _, ok := dubbed.body["subtitle_language"]; dubbed.status != http.StatusCreated || ok {
		t.Fatalf("create dubbed showtime = %d %v, want no subtitle_language", dubbed.status, dubbed.body)
	}

	// hasVersion checks a showtime (or a seat map or a booking's showtime) and the genres of its movie.
	hasVersion := func(where string, st map[string]any, audio, subtitles string) {
		t.Helper()
		sub, ok := st["subtitle_language"]
		if st["audio_language"] != audio || (subtitles == "") == ok || (ok && sub != subtitles) {
			t.Errorf("%s: languages of %v, want %q and %q", where, st, audio, subtitles)
		}
		if m, _ := st["movie"].(map[string]any); !reflect.DeepEqual(m["genres"], wantGenres) {
			t.Errorf("%s: movie %v, want genres %v", where, st["movie"], wantGenres)
		}
	}

	list := api.do(http.MethodGet, "/v1/movies", "", nil)
	items, _ := list.body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		switch m["id"] {
		case movieID:
			if !reflect.DeepEqual(m["genres"], wantGenres) {
				t.Errorf("movie list: %v, want genres %v", m, wantGenres)
			}
		case float64(f.dune.ID):
			if !reflect.DeepEqual(m["genres"], []any{}) {
				t.Errorf("movie list: %v, want genres [] for a movie without any", m)
			}
		}
	}

	page := api.do(http.MethodGet, fmt.Sprintf("/v1/movies/%d", int64(movieID)), "", nil)
	upcoming, _ := page.body["upcoming_showtimes"].([]any)
	if !reflect.DeepEqual(page.body["genres"], wantGenres) || len(upcoming) != 2 {
		t.Fatalf("movie page = %d %v", page.status, page.body)
	}
	for _, u := range upcoming {
		st, _ := u.(map[string]any)
		if st["id"] == subtitled.body["id"] {
			hasVersion("movie page", st, "eng", "tha")
		} else {
			hasVersion("movie page", st, "tha", "")
		}
	}

	sched := api.do(http.MethodGet, fmt.Sprintf("/v1/showtimes?date=%s&movie_id=%d", start.Format(time.DateOnly), int64(movieID)), "", nil)
	if items, _ := sched.body["items"].([]any); len(items) != 2 {
		t.Errorf("schedule = %v, want both versions", sched.body)
	}

	location := subtitled.header.Get("Location")
	hasVersion("showtime", api.do(http.MethodGet, location, "", nil).body, "eng", "tha")
	seatMap := api.do(http.MethodGet, location+"/seats", "", nil)
	hasVersion("seat map", seatMap.body, "eng", "tha")

	seats, _ := seatMap.body["seats"].([]any)
	seat, _ := seats[0].(map[string]any)
	book := api.do(http.MethodPost, "/v1/bookings", customer, map[string]any{"showtime_id": subtitled.body["id"], "seat_ids": []any{seat["id"]}})
	if book.status != http.StatusCreated {
		t.Fatalf("booking = %d %v", book.status, book.body)
	}
	bookedShowtime, _ := book.body["showtime"].(map[string]any)
	hasVersion("new booking", bookedShowtime, "eng", "tha")
	read := api.do(http.MethodGet, book.header.Get("Location"), customer, nil)
	readShowtime, _ := read.body["showtime"].(map[string]any)
	hasVersion("read booking", readShowtime, "eng", "tha")
}

// genresJSON is how genres decode from a response into map[string]any.
func genresJSON(genres ...domain.Genre) []any {
	out := make([]any, len(genres))
	for i, g := range genres {
		out[i] = map[string]any{"id": float64(g.ID), "slug": g.Slug, "name": g.Name}
	}
	return out
}

// errorFields returns the fields a 400 VALIDATION_FAILED names, in order.
func errorFields(r apiResponse) []string {
	errs, _ := r.body["errors"].([]any)
	fields := make([]string, 0, len(errs))
	for _, e := range errs {
		fe, _ := e.(map[string]any)
		field, _ := fe["field"].(string)
		fields = append(fields, field)
	}
	return fields
}
