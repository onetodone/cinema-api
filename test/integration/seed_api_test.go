//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/seed"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
)

func TestSeedFillsTheDatabase(t *testing.T) {
	t.Parallel()
	pool := newDB(t)
	ctx := t.Context()

	loc, err := time.LoadLocation("Asia/Dubai")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := seed.Run(ctx, postgres.NewCatalog(pool), seed.Options{
		Days: 2, FirstDay: time.Date(2030, 3, 1, 0, 0, 0, 0, loc), Location: loc,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	count := func(table string) int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count("movies"); got != stats.Movies {
		t.Errorf("movies = %d, stats say %d", got, stats.Movies)
	}
	if got := count("hall_seats"); got != stats.Seats {
		t.Errorf("hall seats = %d, stats say %d", got, stats.Seats)
	}
	if got := count("showtimes"); got != stats.Showtimes || got == 0 {
		t.Errorf("showtimes = %d, stats say %d", got, stats.Showtimes)
	}

	// Every showtime got exactly one inventory row per seat of its hall.
	var mismatched int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM showtimes s
WHERE (SELECT count(*) FROM showtime_seats ss WHERE ss.showtime_id = s.id)
   <> (SELECT count(*) FROM hall_seats hs WHERE hs.hall_id = s.hall_id)`).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Errorf("%d showtimes have an incomplete seat inventory", mismatched)
	}
}

// TestCatalogAPI drives the real router, service, and repository over HTTP.
func TestCatalogAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	f.showtime(t, f.short, f.hall2, base.Add(time.Hour))

	logger := slog.New(slog.DiscardHandler)
	svc := catalog.New(f.catalog, time.UTC)
	router := httpapi.NewRouter(httpapi.RouterDeps{
		Logger: logger,
		Health: handler.NewHealth(logger, time.Second,
			handler.Check{Name: "postgres", Critical: true, Probe: f.pool.Ping}),
		Catalog: handler.NewCatalog(svc, "USD", logger),
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	get := func(path string, out any) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
		}
		return resp.StatusCode
	}

	var sched struct {
		Date  string `json:"date"`
		Items []struct {
			ID    int64 `json:"id"`
			Movie struct {
				Title string `json:"title"`
			} `json:"movie"`
			SeatsAvailable int `json:"seats_available"`
		} `json:"items"`
	}
	if code := get("/v1/showtimes?date=2030-01-10", &sched); code != http.StatusOK {
		t.Fatalf("schedule status = %d", code)
	}
	if sched.Date != "2030-01-10" || len(sched.Items) != 2 || sched.Items[0].Movie.Title != "Dune" {
		t.Fatalf("schedule = %+v", sched)
	}

	var seatMap struct {
		ShowtimeID int64 `json:"showtime_id"`
		Summary    struct {
			Available int `json:"available"`
			Total     int `json:"total"`
		} `json:"summary"`
		Seats []struct {
			Row    string `json:"row"`
			Status string `json:"status"`
		} `json:"seats"`
	}
	if code := get(fmt.Sprintf("/v1/showtimes/%d/seats", st.ID), &seatMap); code != http.StatusOK {
		t.Fatalf("seat map status = %d", code)
	}
	if seatMap.ShowtimeID != st.ID || seatMap.Summary.Total != 5 || len(seatMap.Seats) != 5 {
		t.Errorf("seat map = %+v", seatMap)
	}

	type moviePage struct {
		Items      []struct{ ID int64 } `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	var page1, page2 moviePage
	if code := get("/v1/movies?limit=1", &page1); code != http.StatusOK || len(page1.Items) != 1 || page1.NextCursor == "" {
		t.Fatalf("movies page 1 = %d %+v", code, page1)
	}
	if code := get("/v1/movies?limit=1&cursor="+page1.NextCursor, &page2); code != http.StatusOK ||
		len(page2.Items) != 1 || page2.Items[0].ID == page1.Items[0].ID || page2.NextCursor != "" {
		t.Fatalf("movies page 2 = %d %+v", code, page2)
	}

	var problem struct {
		Code string `json:"code"`
	}
	if code := get("/v1/showtimes/999999/seats", &problem); code != http.StatusNotFound || problem.Code != "SHOWTIME_NOT_FOUND" {
		t.Errorf("unknown showtime = %d %s", code, problem.Code)
	}
	if code := get("/readyz", nil); code != http.StatusOK {
		t.Errorf("readyz = %d", code)
	}
}
