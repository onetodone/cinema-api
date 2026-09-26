//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/admin"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
)

// apiClient sends JSON requests to a test server.
type apiClient struct {
	t   *testing.T
	srv *httptest.Server
}

type apiResponse struct {
	status int
	header http.Header
	body   map[string]any
}

func (c apiClient) do(method, path, token string, body any) apiResponse {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := apiResponse{status: resp.StatusCode, header: resp.Header}
	if err := json.NewDecoder(resp.Body).Decode(&out.body); err != nil && err != io.EOF {
		c.t.Fatalf("%s %s: decode body: %v", method, path, err)
	}
	return out
}

func (r apiResponse) code() string {
	s, _ := r.body["code"].(string)
	return s
}

// TestAuthAPI drives registration, login, and role checks through the real router, services, and database.
func TestAuthAPI(t *testing.T) {
	t.Parallel()
	pool := newDB(t)
	logger := slog.New(slog.DiscardHandler)

	tokens, err := auth.NewTokens(strings.Repeat("k", auth.MinSecretBytes), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := auth.New(postgres.NewUsers(pool), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	catalogRepo := postgres.NewCatalog(pool)
	router := httpapi.NewRouter(httpapi.RouterDeps{
		Logger:  logger,
		Tokens:  tokens,
		Health:  handler.NewHealth(logger, time.Second),
		Catalog: handler.NewCatalog(catalog.New(catalogRepo, time.UTC), "USD", logger),
		Auth:    handler.NewAuth(authSvc, tokens, logger),
		Admin:   handler.NewAdmin(admin.New(catalogRepo), logger),
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	api := apiClient{t: t, srv: srv}

	creds := map[string]string{"email": "Ann@Example.com", "password": "correct horse"}

	// Register, then the same address in another case is taken.
	reg := api.do(http.MethodPost, "/v1/auth/register", "", creds)
	if reg.status != http.StatusCreated || reg.body["role"] != "customer" || reg.body["email"] != "Ann@Example.com" {
		t.Fatalf("register = %d %v", reg.status, reg.body)
	}
	if _, leaked := reg.body["password_hash"]; leaked {
		t.Error("register response contains the password hash")
	}
	dup := api.do(http.MethodPost, "/v1/auth/register", "",
		map[string]string{"email": "ann@example.COM", "password": "other password"})
	if dup.status != http.StatusConflict || dup.code() != "EMAIL_TAKEN" {
		t.Errorf("duplicate register = %d %v", dup.status, dup.body)
	}
	weak := api.do(http.MethodPost, "/v1/auth/register", "", map[string]string{"email": "bad", "password": "short"})
	if weak.status != http.StatusBadRequest || weak.code() != "VALIDATION_FAILED" {
		t.Errorf("invalid register = %d %v", weak.status, weak.body)
	}

	// Log in with the address in another case.
	login := api.do(http.MethodPost, "/v1/auth/login", "",
		map[string]string{"email": "ann@example.com", "password": "correct horse"})
	token, _ := login.body["access_token"].(string)
	if login.status != http.StatusOK || token == "" || login.body["expires_in"] != 3600.0 {
		t.Fatalf("login = %d %v", login.status, login.body)
	}
	if login.header.Get("Cache-Control") != "no-store" {
		t.Error("the token response may be cached")
	}

	for name, body := range map[string]map[string]string{
		"wrong password": {"email": "ann@example.com", "password": "wrong horse"},
		"unknown email":  {"email": "bob@example.com", "password": "correct horse"},
	} {
		r := api.do(http.MethodPost, "/v1/auth/login", "", body)
		if r.status != http.StatusUnauthorized || r.code() != "INVALID_CREDENTIALS" || r.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: login = %d %v", name, r.status, r.body)
		}
	}

	// The token opens GET /v1/me, but not the admin routes.
	me := api.do(http.MethodGet, "/v1/me", token, nil)
	if me.status != http.StatusOK || me.body["id"] != reg.body["id"] {
		t.Errorf("me = %d %v", me.status, me.body)
	}
	if r := api.do(http.MethodGet, "/v1/me", "", nil); r.status != http.StatusUnauthorized || r.code() != "UNAUTHENTICATED" {
		t.Errorf("me without a token = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodGet, "/v1/me", token+"x", nil); r.status != http.StatusUnauthorized || r.code() != "INVALID_TOKEN" {
		t.Errorf("me with a tampered token = %d %v", r.status, r.body)
	}

	movie := map[string]any{"title": "Dune", "duration_min": 155, "age_rating": "PG-13"}
	if r := api.do(http.MethodPost, "/v1/admin/movies", token, movie); r.status != http.StatusForbidden || r.code() != "FORBIDDEN" {
		t.Errorf("admin route as a customer = %d %v", r.status, r.body)
	}

	// The seed's admin setup promotes nobody else; the admin logs in and creates a movie.
	if _, _, err := authSvc.EnsureAdmin(t.Context(), "admin@cinema.local", "admin password"); err != nil {
		t.Fatal(err)
	}
	adminLogin := api.do(http.MethodPost, "/v1/auth/login", "",
		map[string]string{"email": "admin@cinema.local", "password": "admin password"})
	adminToken, _ := adminLogin.body["access_token"].(string)
	if adminLogin.status != http.StatusOK || adminToken == "" {
		t.Fatalf("admin login = %d %v", adminLogin.status, adminLogin.body)
	}

	created := api.do(http.MethodPost, "/v1/admin/movies", adminToken, movie)
	if created.status != http.StatusCreated || created.body["title"] != "Dune" {
		t.Fatalf("create movie = %d %v", created.status, created.body)
	}
	location := created.header.Get("Location")
	if got := api.do(http.MethodGet, location, "", nil); got.status != http.StatusOK || got.body["age_rating"] != "PG-13" {
		t.Errorf("GET %s = %d %v", location, got.status, got.body)
	}

	invalid := api.do(http.MethodPost, "/v1/admin/movies", adminToken, map[string]any{"title": " ", "duration_min": 0})
	if invalid.status != http.StatusBadRequest || invalid.code() != "VALIDATION_FAILED" {
		t.Errorf("invalid movie = %d %v", invalid.status, invalid.body)
	}
	if errs, _ := invalid.body["errors"].([]any); len(errs) != 2 {
		t.Errorf("invalid movie errors = %v, want title and duration_min", invalid.body["errors"])
	}
}
