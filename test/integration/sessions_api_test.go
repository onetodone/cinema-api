//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
)

// session is what a client holds after a login or refresh.
type session struct {
	access  string
	refresh string // the cookie's value
	cookie  *http.Cookie
}

func (s session) id() uuid.UUID {
	token, _ := domain.ParseRefreshToken(s.refresh)
	return token.SessionID
}

// sessionOf reads the access token and the refresh token cookie of a login or refresh answer.
func sessionOf(t *testing.T, r apiResponse) session {
	t.Helper()
	access, _ := r.body["access_token"].(string)
	if r.status != http.StatusOK || access == "" {
		t.Fatalf("answer = %d %v, want 200 with an access token", r.status, r.body)
	}
	c := refreshCookieIn(r)
	if c == nil || c.Value == "" {
		t.Fatalf("answer sets no refresh token cookie: %v", r.header.Values("Set-Cookie"))
	}
	return session{access: access, refresh: c.Value, cookie: c}
}

func refreshCookieIn(r apiResponse) *http.Cookie {
	for _, c := range r.cookies {
		if c.Name == handler.RefreshCookieName {
			return c
		}
	}
	return nil
}

// wantCleared checks that r refused the refresh token and deleted its cookie.
func wantCleared(t *testing.T, r apiResponse, status int) {
	t.Helper()
	if r.status != status {
		t.Errorf("status = %d %v, want %d", r.status, r.body, status)
	}
	if c := refreshCookieIn(r); c == nil || c.Value != "" || c.MaxAge != -1 || c.Path != "/v1/auth" {
		t.Errorf("Set-Cookie = %v, want the cookie deleted", r.header.Values("Set-Cookie"))
	}
}

// login logs an account in and returns its new session.
func (c apiClient) login(email string) session {
	c.t.Helper()
	return sessionOf(c.t, c.do(http.MethodPost, "/v1/auth/login", "",
		map[string]string{"email": email, "password": "correct horse"}))
}

// withCookie posts {} to a session route with a refresh token cookie, or without one if token is empty.
func (c apiClient) withCookie(path, token string) apiResponse {
	c.t.Helper()
	var header http.Header
	if token != "" {
		header = http.Header{"Cookie": {handler.RefreshCookieName + "=" + token}}
	}
	return c.doWith(http.MethodPost, path, "", map[string]any{}, header)
}

func sessionCount(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM sessions WHERE `+where, args...)
}

func generation(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT generation FROM sessions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("generation of session %s: %v", id, err)
	}
	return n
}

// TestSessionsAPI walks one account through login, refresh, the grace window, reuse detection, and logout.
func TestSessionsAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	api.signUp("ann@example.com")

	// Login: an access token for the session, and the refresh token in a hardened cookie.
	login := api.do(http.MethodPost, "/v1/auth/login", "", map[string]string{"email": "ann@example.com", "password": "correct horse"})
	s0 := sessionOf(t, login)
	c := s0.cookie
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/v1/auth" || c.Domain != "" {
		t.Errorf("Set-Cookie = %q, want HttpOnly; Secure; SameSite=Strict; Path=/v1/auth; no Domain", login.header.Get("Set-Cookie"))
	}
	if week := int(testSessionConfig.IdleTTL / time.Second); c.MaxAge < week-60 || c.MaxAge > week {
		t.Errorf("Max-Age = %d, want about %d", c.MaxAge, week)
	}
	if user, _ := login.body["user"].(map[string]any); user["email"] != "ann@example.com" || user["role"] != "customer" {
		t.Errorf("login user = %v", login.body["user"])
	}
	p := must(newTestTokens(t).Verify(s0.access))(t)
	if p.SessionID != s0.id() {
		t.Errorf("the access token names session %s, the cookie %s", p.SessionID, s0.id())
	}
	if r := api.do(http.MethodGet, "/v1/me", s0.access, nil); r.status != http.StatusOK {
		t.Fatalf("me = %d %v", r.status, r.body)
	}

	// Refresh: a new access token and a rotated refresh token.
	r := api.withCookie("/v1/auth/refresh", s0.refresh)
	s1 := sessionOf(t, r)
	if s1.refresh == s0.refresh || s1.id() != s0.id() || s1.access == s0.access {
		t.Fatalf("refresh gave %+v, want new tokens of the same session", s1)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Error("the refresh answer may be cached")
	}
	if g := generation(t, f.pool, s0.id()); g != 1 {
		t.Errorf("generation = %d, want 1", g)
	}
	if r := api.do(http.MethodGet, "/v1/me", s1.access, nil); r.status != http.StatusOK {
		t.Errorf("me with the refreshed token = %d %v", r.status, r.body)
	}

	// The previous token within the grace window: the current one, without another rotation.
	if again := sessionOf(t, api.withCookie("/v1/auth/refresh", s0.refresh)); again.refresh != s1.refresh {
		t.Errorf("the previous token got %q, want the current %q", again.refresh, s1.refresh)
	}
	if g := generation(t, f.pool, s0.id()); g != 1 {
		t.Errorf("generation = %d after a grace answer, want 1", g)
	}

	// Refresh needs JSON, so a cross-site form cannot trigger it.
	form := api.doWith(http.MethodPost, "/v1/auth/refresh", "", nil, http.Header{
		"Content-Type": {"application/x-www-form-urlencoded"},
		"Cookie":       {handler.RefreshCookieName + "=" + s1.refresh},
	})
	if form.status != http.StatusUnsupportedMediaType {
		t.Errorf("refresh without JSON = %d %v, want 415", form.status, form.body)
	}

	// The grace window ends; the previous token is now proof of a copy: the session is revoked.
	exec(t, f.pool, `UPDATE sessions SET rotated_at = rotated_at - interval '31 seconds' WHERE id = $1`, s0.id())
	reuse := api.withCookie("/v1/auth/refresh", s0.refresh)
	wantCleared(t, reuse, http.StatusUnauthorized)
	if reuse.code() != domain.CodeRefreshInvalid {
		t.Errorf("reuse = %v, want REFRESH_INVALID", reuse.body)
	}
	if n := sessionCount(t, f.pool, `id = $1`, s0.id()); n != 0 {
		t.Fatal("the session survived the reuse of a rotated token")
	}
	wantCleared(t, api.withCookie("/v1/auth/refresh", s1.refresh), http.StatusUnauthorized)
	wantCleared(t, api.withCookie("/v1/auth/refresh", ""), http.StatusUnauthorized)
	if n := testCount(api.redis.metrics.AuthRefreshes.WithLabelValues(metrics.RefreshReuseDetected)); n != 1 {
		t.Errorf("reuse detections counted = %v, want 1", n)
	}

	// Logout ends the session and can be repeated.
	s := api.login("ann@example.com")
	for range 2 {
		wantCleared(t, api.withCookie("/v1/auth/logout", s.refresh), http.StatusNoContent)
	}
	if n := sessionCount(t, f.pool, `id = $1`, s.id()); n != 0 {
		t.Error("logout kept the session")
	}
	wantCleared(t, api.withCookie("/v1/auth/refresh", s.refresh), http.StatusUnauthorized)
	wantCleared(t, api.withCookie("/v1/auth/logout", ""), http.StatusNoContent)

	// Logout from every session, with the access token of one of them.
	a, b, other := api.login("ann@example.com"), api.login("ann@example.com"), api.signUp("bob@example.com")
	if r := api.do(http.MethodPost, "/v1/auth/logout-all", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("logout-all without a token = %d", r.status)
	}
	wantCleared(t, api.do(http.MethodPost, "/v1/auth/logout-all", a.access, nil), http.StatusNoContent)
	if n := sessionCount(t, f.pool, `user_id = $1`, p.UserID); n != 0 {
		t.Errorf("%d sessions of Ann left after logout-all", n)
	}
	for _, s := range []session{a, b} {
		wantCleared(t, api.withCookie("/v1/auth/refresh", s.refresh), http.StatusUnauthorized)
	}
	if r := api.do(http.MethodGet, "/v1/me", other, nil); r.status != http.StatusOK {
		t.Error("Ann's logout-all ended Bob's session")
	}
}

func TestSessionsExpire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	api.signUp("ann@example.com")

	// Past its idle lifetime.
	s := api.login("ann@example.com")
	exec(t, f.pool, `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.id())
	wantCleared(t, api.withCookie("/v1/auth/refresh", s.refresh), http.StatusUnauthorized)
	if n := sessionCount(t, f.pool, `id = $1`, s.id()); n != 0 {
		t.Error("the refresh left the expired session in place")
	}

	// A refresh never extends a session beyond its maximum age.
	s = api.login("ann@example.com")
	exec(t, f.pool, `UPDATE sessions SET created_at = now() - interval '29 days' WHERE id = $1`, s.id())
	sessionOf(t, api.withCookie("/v1/auth/refresh", s.refresh))
	var capped bool
	err := f.pool.QueryRow(t.Context(), `SELECT expires_at = created_at + interval '30 days' FROM sessions WHERE id = $1`,
		s.id()).Scan(&capped)
	if err != nil || !capped {
		t.Errorf("expires_at is not capped at created_at + 30 days (%v)", err)
	}
}

func TestSessionCookieWithoutSecure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServerWith(t, f, apiOptions{cookie: &handler.RefreshCookie{Path: "/v1/auth"}})
	api.signUp("ann@example.com")

	if c := api.login("ann@example.com").cookie; c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v, want HttpOnly and SameSite=Strict without Secure", c)
	}
}

func TestAccessTokensWithoutSessionAreRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	api.signUp("ann@example.com")
	userID := must(postgres.NewUsers(f.pool).GetUserByEmail(t.Context(), "ann@example.com"))(t).ID

	// Signed with the API's key and valid in every other way, as the API issued tokens before sessions existed.
	now := time.Now()
	legacy, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "cinema-api", "aud": "cinema-api", "sub": userID.String(), "role": "customer",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "jti": uuid.NewV7().String(),
	}).SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	if r := api.do(http.MethodGet, "/v1/me", legacy, nil); r.status != http.StatusUnauthorized || r.code() != "INVALID_TOKEN" {
		t.Errorf("me with a token without sid = %d %v, want 401 INVALID_TOKEN", r.status, r.body)
	}
}

// TestConcurrentRefreshesAcrossReplicas is the multi-tab case on a multi-replica API: 20 tabs refresh with the
// same token at once, through two API instances that share only the database. Every tab must keep its session,
// and the session must rotate once.
func TestConcurrentRefreshesAcrossReplicas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	replicas := []apiClient{newAPIServer(t, f), newAPIServer(t, f)}
	replicas[0].signUp("ann@example.com")
	s := replicas[1].login("ann@example.com")

	const tabs = 20
	answers := make([]apiResponse, tabs)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range tabs {
		wg.Go(func() {
			<-start
			answers[i] = replicas[i%2].withCookie("/v1/auth/refresh", s.refresh)
		})
	}
	close(start)
	wg.Wait()

	var current string
	for i, r := range answers {
		got := sessionOf(t, r)
		if current == "" {
			current = got.refresh
		}
		if got.refresh != current {
			t.Errorf("tab %d got %q, tab 0 %q: want one current token for every tab", i, got.refresh, current)
		}
		if me := replicas[(i+1)%2].do(http.MethodGet, "/v1/me", got.access, nil); me.status != http.StatusOK {
			t.Errorf("tab %d: its access token = %d on the other replica", i, me.status)
		}
	}
	if current == s.refresh {
		t.Fatal("no refresh rotated the token")
	}
	if g := generation(t, f.pool, s.id()); g != 1 {
		t.Errorf("generation = %d, want exactly one rotation", g)
	}
	var stored []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT token_hash FROM sessions WHERE id = $1`, s.id()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	token, _ := domain.ParseRefreshToken(current)
	if string(stored) != string(token.Hash()) {
		t.Error("the token every tab got is not the session's current token")
	}
}

// TestConcurrentLoginsKeepTheSessionCap: logins of one user take turns on the user's row, so none of them counts
// the sessions before another one's insert, and the cap holds exactly.
func TestConcurrentLoginsKeepTheSessionCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := testSessionConfig
	cfg.MaxPerUser = 3
	api := newAPIServerWith(t, f, apiOptions{sessions: cfg})
	api.signUp("ann@example.com")
	exec(t, f.pool, `DELETE FROM sessions`) // the session of signUp

	const logins = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, logins)
	for i := range logins {
		wg.Go(func() {
			<-start
			statuses[i] = api.do(http.MethodPost, "/v1/auth/login", "",
				map[string]string{"email": "ann@example.com", "password": "correct horse"}).status
		})
	}
	close(start)
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("login %d = %d, want 200", i, status)
		}
	}
	if n := sessionCount(t, f.pool, `true`); n != cfg.MaxPerUser {
		t.Errorf("%d sessions after %d concurrent logins, want the cap of %d", n, logins, cfg.MaxPerUser)
	}

	// The same race without HTTP and password hashes in between, so the inserts really overlap.
	repo := postgres.NewSessions(f.pool)
	userID := must(postgres.NewUsers(f.pool).GetUserByEmail(t.Context(), "ann@example.com"))(t).ID
	const creates = 50
	start = make(chan struct{})
	errs := make([]error, creates)
	for i := range creates {
		wg.Go(func() {
			token := domain.NewRefreshToken(uuid.NewV7())
			<-start
			_, _, errs[i] = repo.Create(context.Background(), auth.NewSession{
				ID: token.SessionID, UserID: userID, TokenHash: token.Hash(),
				IdleTTL: time.Hour, MaxAge: time.Hour, MaxPerUser: cfg.MaxPerUser,
			})
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if n := sessionCount(t, f.pool, `true`); n != cfg.MaxPerUser {
		t.Errorf("%d sessions after %d concurrent creates, want the cap of %d", n, creates, cfg.MaxPerUser)
	}
}

// TestRefreshIsLimitedPerSession: every session has a budget of its own, so users behind one address do not share
// one.
func TestRefreshIsLimitedPerSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServerWith(t, f, apiOptions{limits: config.RateLimitConfig{RefreshPerMin: 2}})
	api.signUp("ann@example.com")
	a, b := api.login("ann@example.com"), api.login("ann@example.com")

	for range 2 {
		sessionOf(t, api.withCookie("/v1/auth/refresh", a.refresh)) // the second one is a grace answer
	}
	limited := api.withCookie("/v1/auth/refresh", a.refresh)
	if limited.status != http.StatusTooManyRequests || limited.code() != "RATE_LIMITED" || limited.header.Get("Retry-After") == "" {
		t.Errorf("third refresh of a session = %d %v, want 429 with Retry-After", limited.status, limited.body)
	}
	if refreshCookieIn(limited) != nil {
		t.Error("a rate-limited refresh touched the cookie")
	}
	sessionOf(t, api.withCookie("/v1/auth/refresh", b.refresh)) // another session of the same address
	if n := testCount(api.redis.metrics.RateLimitRejections.WithLabelValues(metrics.LimitRefresh)); n != 1 {
		t.Errorf("refresh rejections = %v, want 1", n)
	}
}

// TestSessionSweepersShareTheWork: two sweepers delete every expired session once between them, never wait for a
// session another transaction holds, and leave live sessions alone.
func TestSessionSweepersShareTheWork(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	api := newAPIServer(t, f)
	api.signUp("ann@example.com")
	for range 5 {
		api.login("ann@example.com")
	}
	userID := must(postgres.NewUsers(f.pool).GetUserByEmail(ctx, "ann@example.com"))(t).ID
	live := sessionCount(t, f.pool, `true`)
	exec(t, f.pool, `
INSERT INTO sessions (id, user_id, token_hash, expires_at)
SELECT gen_random_uuid(), $1, sha256(g::text::bytea), now() - make_interval(secs => g)
FROM generate_series(1, 200) AS g`, userID)

	// A transaction holds one expired session, as a refresh or a logout would for a moment.
	held, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(context.Background()) }()
	var heldID uuid.UUID
	if err := held.QueryRow(ctx, `SELECT id FROM sessions WHERE expires_at <= now() ORDER BY expires_at LIMIT 1 FOR UPDATE`).
		Scan(&heldID); err != nil {
		t.Fatal(err)
	}

	sweepers := []*postgres.Sessions{postgres.NewSessions(f.pool), postgres.NewSessions(f.pool)}
	deleted := make([]int, len(sweepers))
	errs := make([]error, len(sweepers))
	var wg sync.WaitGroup
	for i, s := range sweepers {
		wg.Go(func() {
			for {
				n, err := s.DeleteExpired(ctx, 7)
				deleted[i] += n
				if err != nil || n == 0 {
					errs[i] = err
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a sweeper waited for the session another transaction holds")
	}
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	if total := deleted[0] + deleted[1]; total != 199 {
		t.Errorf("sweepers deleted %d and %d sessions, want 199 between them (one is held)", deleted[0], deleted[1])
	}
	if n := sessionCount(t, f.pool, `expires_at > now()`); n != live {
		t.Errorf("%d live sessions left, want all %d", n, live)
	}

	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := must(sweepers[0].DeleteExpired(ctx, 7))(t); n != 1 {
		t.Errorf("after the commit the sweeper deleted %d sessions, want the one that was held", n)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1`, heldID).Scan(new(uuid.UUID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the held session is still there (%v)", err)
	}
}
