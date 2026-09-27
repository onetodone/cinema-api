//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/seed"
)

// TestActiveBookingConflictNamesTheBooking: a second hold for the same showtime is refused with the id of the
// unpaid booking that blocks it, also in an idempotent replay.
func TestActiveBookingConflictNamesTheBooking(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	api := newAPIServer(t, f)
	ann, bob := api.signUp("ann@example.com"), api.signUp("bob@example.com")
	hold := func(token, label string, header http.Header) apiResponse {
		t.Helper()
		return api.doWith(http.MethodPost, "/v1/bookings", token,
			map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats[label]}}, header)
	}

	first := hold(ann, "A1", nil)
	if first.status != http.StatusCreated {
		t.Fatalf("first hold = %d %v", first.status, first.body)
	}
	firstID := first.body["id"].(string)

	// Other seats pass the hold gate and reach the unique index in PostgreSQL.
	second := hold(ann, "A2", withKey("second-hold"))
	if second.status != http.StatusConflict || second.code() != "ACTIVE_BOOKING_EXISTS" || second.body["booking_id"] != firstID {
		t.Fatalf("second hold = %d %v, want 409 ACTIVE_BOOKING_EXISTS naming %s", second.status, second.body, firstID)
	}
	if second.header.Get("Retry-After") != "" {
		t.Errorf("Retry-After = %q; the conflict does not go away by waiting", second.header.Get("Retry-After"))
	}
	replay := hold(ann, "A2", withKey("second-hold"))
	if replay.header.Get("Idempotent-Replayed") != "true" || replay.status != http.StatusConflict || replay.body["booking_id"] != firstID {
		t.Errorf("replay = %d %v %v, want the stored conflict with booking_id", replay.status, replay.header, replay.body)
	}
	// Another user's hold for the same showtime is not blocked, and names nothing of Ann's.
	if r := hold(bob, "B1", nil); r.status != http.StatusCreated {
		t.Errorf("bob's hold = %d %v", r.status, r.body)
	}

	// The id leads to the booking, which the client may cancel; then a new hold succeeds.
	if r := api.do(http.MethodGet, "/v1/bookings/"+firstID, ann, nil); r.status != http.StatusOK || r.body["status"] != "pending" {
		t.Errorf("GET the blocking booking = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodDelete, "/v1/bookings/"+firstID, ann, nil); r.status != http.StatusNoContent {
		t.Fatalf("cancel = %d %v", r.status, r.body)
	}
	third := hold(ann, "A2", nil)
	if third.status != http.StatusCreated {
		t.Fatalf("hold after cancel = %d %v", third.status, third.body)
	}

	// The lookup behind booking_id finds the active booking, and nothing once it has ended.
	repo := postgres.NewBookings(f.pool)
	annID := uuid.MustParse(api.do(http.MethodGet, "/v1/me", ann, nil).body["id"].(string))
	if id := must(repo.ActiveBookingID(t.Context(), annID, st.ID))(t); id.String() != third.body["id"] {
		t.Errorf("active booking = %s, want %v", id, third.body["id"])
	}
	if r := api.do(http.MethodDelete, "/v1/bookings/"+third.body["id"].(string), ann, nil); r.status != http.StatusNoContent {
		t.Fatalf("cancel = %d %v", r.status, r.body)
	}
	if id := must(repo.ActiveBookingID(t.Context(), annID, st.ID))(t); id != (uuid.UUID{}) {
		t.Errorf("active booking after cancel = %s, want none", id)
	}

	if n := testCount(api.redis.metrics.BookingAttempts.WithLabelValues(metrics.BookingActiveBookingExists)); n != 1 {
		t.Errorf("active_booking_exists attempts = %v, want 1 (a replay is not an attempt)", n)
	}
}

// TestPolledReadsWithETags: the seat map and the schedule carry an ETag; a client that sends it back gets 304
// until the content changes. The tag depends only on the content, so a cached and a fresh read, on two replicas
// with caches of their own, agree.
func TestPolledReadsWithETags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Noon of a day the admin API accepts showtimes for, so a showtime an hour later is on the same day.
	later := time.Now().UTC().Add(48 * time.Hour)
	start := time.Date(later.Year(), later.Month(), later.Day(), 12, 0, 0, 0, time.UTC)
	day := start.Format(time.DateOnly)
	st := f.showtime(t, f.dune, f.hall, start)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	api, replica := newAPIServer(t, f), newAPIServer(t, f) // each with a Redis key prefix, so a cache, of its own
	ann := api.signUp("ann@example.com")

	seatMapPath := fmt.Sprintf("/v1/showtimes/%d/seats", st.ID)
	schedulePath := "/v1/showtimes?date=" + day
	get := func(c apiClient, path, etag string) apiResponse {
		t.Helper()
		var header http.Header
		if etag != "" {
			header = http.Header{"If-None-Match": {etag}}
		}
		return c.doWith(http.MethodGet, path, "", nil, header)
	}
	wantNotModified := func(step string, r apiResponse, etag string) {
		t.Helper()
		if r.status != http.StatusNotModified || r.body != nil {
			t.Errorf("%s: %d %v, want 304 without a body", step, r.status, r.body)
		}
		if r.header.Get("ETag") != etag || r.header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: ETag %q, Cache-Control %q; want %s and no-cache", step, r.header.Get("ETag"),
				r.header.Get("Cache-Control"), etag)
		}
	}

	for _, path := range []string{seatMapPath, schedulePath} {
		fresh := get(api, path, "") // from PostgreSQL; fills the cache
		etag := fresh.header.Get("ETag")
		if fresh.status != http.StatusOK || !strings.HasPrefix(etag, `W/"`) || fresh.header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s = %d, ETag %q, Cache-Control %q", path, fresh.status, etag, fresh.header.Get("Cache-Control"))
		}
		if cached := get(api, path, ""); cached.header.Get("ETag") != etag {
			t.Errorf("%s from the cache: ETag %q, want %q", path, cached.header.Get("ETag"), etag)
		}
		if other := get(replica, path, ""); other.header.Get("ETag") != etag {
			t.Errorf("%s on another replica: ETag %q, want %q", path, other.header.Get("ETag"), etag)
		}
		wantNotModified(path, get(api, path, etag), etag)
		wantNotModified(path+" on another replica", get(replica, path, etag), etag)
		// The strong form of the tag matches too (weak comparison), and a list of tags is searched.
		wantNotModified(path+" strong", get(api, path, strings.TrimPrefix(etag, "W/")), etag)
		wantNotModified(path+" in a list", get(api, path, `"stale", `+etag), etag)
	}

	// One booking later, the seat map has a new tag, and the old one gets the full answer.
	before := get(api, seatMapPath, "").header.Get("ETag")
	if r := api.do(http.MethodPost, "/v1/bookings", ann, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats["A1"]}}); r.status != http.StatusCreated {
		t.Fatalf("book = %d %v", r.status, r.body)
	}
	changed := get(api, seatMapPath, before)
	after := changed.header.Get("ETag")
	summary, _ := changed.body["summary"].(map[string]any)
	if changed.status != http.StatusOK || after == before || after == "" || summary["held"] != 1.0 {
		t.Fatalf("seat map after a booking = %d, ETag %q (was %q), summary %v", changed.status, after, before, summary)
	}
	wantNotModified("seat map after a booking", get(api, seatMapPath, after), after)

	// A new showtime of the day clears the cached schedule, and changes its tag.
	adminToken := api.signInAdmin(f)
	scheduleTag := get(api, schedulePath, "").header.Get("ETag")
	created := api.do(http.MethodPost, "/v1/admin/showtimes", adminToken, map[string]any{
		"movie_id": f.short.ID, "hall_id": f.hall2.ID, "starts_at": start.Add(time.Hour).Format(time.RFC3339), "base_price_cents": 900,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create showtime = %d %v", created.status, created.body)
	}
	sched := get(api, schedulePath, scheduleTag)
	if items, _ := sched.body["items"].([]any); sched.status != http.StatusOK || len(items) != 2 || sched.header.Get("ETag") == scheduleTag {
		t.Errorf("schedule after a new showtime = %d, ETag %q (was %q), %v", sched.status, sched.header.Get("ETag"), scheduleTag, sched.body)
	}

	// Errors carry no tag.
	if r := get(api, "/v1/showtimes/999999/seats", "*"); r.status != http.StatusNotFound || r.header.Get("ETag") != "" {
		t.Errorf("unknown showtime = %d, ETag %q", r.status, r.header.Get("ETag"))
	}
}

// TestBookingListByStatus pages through the caller's bookings of some statuses, in both spellings of the filter.
func TestBookingListByStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	ann, bob := api.signUp("ann@example.com"), api.signUp("bob@example.com")

	// Six bookings of Ann, one per showtime, oldest first, and one of Bob's.
	statuses := []string{"pending", "paid", "canceled", "processing", "expired", "pending"}
	ids := make([]string, len(statuses))
	for i, status := range statuses {
		st := f.showtime(t, f.short, f.hall2, base.Add(time.Duration(i)*3*time.Hour))
		seat := seatIDsByLabel(t, f.pool, f.hall2.ID)["A1"]
		r := api.do(http.MethodPost, "/v1/bookings", ann, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seat}})
		if r.status != http.StatusCreated {
			t.Fatalf("book %d = %d %v", i, r.status, r.body)
		}
		ids[i] = r.body["id"].(string)
		exec(t, f.pool, `UPDATE bookings SET status = $2::booking_status WHERE id = $1`, ids[i], status)
		if i == 0 {
			seat2 := seatIDsByLabel(t, f.pool, f.hall2.ID)["A2"]
			if r := api.do(http.MethodPost, "/v1/bookings", bob, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seat2}}); r.status != http.StatusCreated {
				t.Fatalf("bob's booking = %d %v", r.status, r.body)
			}
		}
	}

	// pages lists every page of the query and returns the ids, newest first.
	pages := func(query string) (got []string, pageCount int) {
		t.Helper()
		path := "/v1/bookings?limit=1&" + query
		for {
			r := api.do(http.MethodGet, path, ann, nil)
			if r.status != http.StatusOK {
				t.Fatalf("GET %s = %d %v", path, r.status, r.body)
			}
			items, _ := r.body["items"].([]any)
			for _, it := range items {
				got = append(got, it.(map[string]any)["id"].(string))
			}
			pageCount++
			next, _ := r.body["next_cursor"].(string)
			if next == "" {
				return got, pageCount
			}
			path = "/v1/bookings?limit=1&" + query + "&cursor=" + next
		}
	}

	active := []string{ids[5], ids[3], ids[0]}
	for _, query := range []string{"status=pending,processing", "status=processing&status=pending"} {
		if got, n := pages(query); !slices.Equal(got, active) || n != 3 {
			t.Errorf("%s: %v in %d pages, want %v in 3", query, got, n, active)
		}
	}
	if got, _ := pages("status=expired,canceled,paid"); !slices.Equal(got, []string{ids[4], ids[2], ids[1]}) {
		t.Errorf("ended bookings = %v", got)
	}
	if got, _ := pages(""); len(got) != len(statuses) {
		t.Errorf("without a filter: %d bookings, want %d", len(got), len(statuses))
	}
	if r := api.do(http.MethodGet, "/v1/bookings?status=pending", bob, nil); len(r.body["items"].([]any)) != 1 {
		t.Errorf("bob's pending bookings = %v", r.body)
	}

	bad := api.do(http.MethodGet, "/v1/bookings?status=pending,held", ann, nil)
	errs, _ := bad.body["errors"].([]any)
	if bad.status != http.StatusBadRequest || bad.code() != "VALIDATION_FAILED" || len(errs) != 1 ||
		errs[0].(map[string]any)["field"] != "status" {
		t.Errorf("unknown status = %d %v", bad.status, bad.body)
	}
}

// TestSeedWithPosterTemplate: every demo movie gets its own poster URL from the template, and the API shows it.
func TestSeedWithPosterTemplate(t *testing.T) {
	t.Parallel()
	pool := newDB(t)
	stats, err := seed.Run(t.Context(), postgres.NewCatalog(pool), seed.Options{
		Days: 1, FirstDay: base, Location: time.UTC,
		PosterURLTemplate: "https://picsum.photos/seed/{slug}/400/600",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(DISTINCT poster_url) FROM movies WHERE poster_url LIKE 'https://picsum.photos/seed/%/400/600'`); n != stats.Movies {
		t.Errorf("%d movies have a poster of their own, want all %d", n, stats.Movies)
	}

	api := newAPIServer(t, &fixture{pool: pool, catalog: postgres.NewCatalog(pool)})
	r := api.do(http.MethodGet, "/v1/movies?limit=1", "", nil)
	items, _ := r.body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["poster_url"] != "https://picsum.photos/seed/the-last-projectionist/400/600" {
		t.Errorf("movies = %d %v", r.status, r.body)
	}
}
