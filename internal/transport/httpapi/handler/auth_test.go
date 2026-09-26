package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

var sampleUser = domain.User{
	ID:           uuid.MustParse("01920000-0000-7000-8000-00000000000a"),
	Email:        "Ann@example.com",
	PasswordHash: "$2a$12$secret-hash-that-must-never-leak",
	Role:         domain.RoleCustomer,
	CreatedAt:    time.Date(2026, 9, 26, 14, 0, 0, 0, time.FixedZone("GST", 4*3600)),
}

// stubAuth records the credentials it receives and returns canned results.
type stubAuth struct {
	email, password string
	userID          uuid.UUID
	user            domain.User
	err             error
}

func (s *stubAuth) Register(_ context.Context, email, password string) (domain.User, error) {
	s.email, s.password = email, password
	return s.user, s.err
}

func (s *stubAuth) User(_ context.Context, id uuid.UUID) (domain.User, error) {
	s.userID = id
	return s.user, s.err
}

// stubSessions records what it receives and returns canned results.
type stubSessions struct {
	calls           int
	email, password string
	token           string
	client          auth.Client
	userID          uuid.UUID
	grant           auth.Grant
	result          auth.RefreshResult
	err             error
}

func (s *stubSessions) Login(_ context.Context, email, password string, c auth.Client) (auth.Grant, error) {
	s.calls++
	s.email, s.password, s.client = email, password, c
	return s.grant, s.err
}

func (s *stubSessions) Refresh(_ context.Context, token string, c auth.Client) (auth.Grant, auth.RefreshResult, error) {
	s.calls++
	s.token, s.client = token, c
	return s.grant, s.result, s.err
}

func (s *stubSessions) Logout(_ context.Context, token string) error {
	s.calls++
	s.token = token
	return s.err
}

func (s *stubSessions) LogoutAll(_ context.Context, userID uuid.UUID) error {
	s.calls++
	s.userID = userID
	return s.err
}

// sampleGrant is a session of sampleUser that expires in a week.
func sampleGrant() auth.Grant {
	sessionID := uuid.MustParse("0199a1f0-7c1e-7d2a-9b3e-5f0c2d1e4a77")
	return auth.Grant{
		User:    sampleUser,
		Session: domain.Session{ID: sessionID, UserID: sampleUser.ID, ExpiresAt: time.Now().Add(7 * 24 * time.Hour)},
		Access:  auth.AccessToken{Token: "signed.jwt.token", ExpiresIn: 15 * time.Minute},
		Refresh: domain.NewRefreshToken(sessionID),
	}
}

var testCookie = RefreshCookie{Path: "/v1/auth", Secure: true}

func postJSON(t *testing.T, h http.HandlerFunc, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// postWithCookie sends a JSON body and, unless token is empty, the refresh token cookie.
func postWithCookie(t *testing.T, h http.HandlerFunc, target, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Firefox/140")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: RefreshCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func newAuthHandler(svc AuthService, sessions SessionService) *Auth {
	return NewAuth(svc, sessions, AuthConfig{Cookie: testCookie}, newTestMetrics(), slog.New(slog.DiscardHandler))
}

// refreshCookie returns the refresh token cookie that rec sets, or fails the test.
func refreshCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != RefreshCookieName {
		t.Fatalf("Set-Cookie = %v, want one %s cookie", rec.Header().Values("Set-Cookie"), RefreshCookieName)
	}
	return cookies[0]
}

// wantCookieCleared checks that rec deletes the refresh token cookie with the attributes that set it.
func wantCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	c := refreshCookie(t, rec)
	if c.Value != "" || c.MaxAge != -1 || c.Path != "/v1/auth" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("Set-Cookie = %q, want a clearing cookie (Max-Age=0) on the same path", rec.Header().Get("Set-Cookie"))
	}
}

func TestRegister(t *testing.T) {
	t.Parallel()

	svc := &stubAuth{user: sampleUser}
	rec := postJSON(t, newAuthHandler(svc, &stubSessions{}).Register, "/v1/auth/register",
		`{"email":"Ann@example.com","password":"correct horse"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.email != "Ann@example.com" || svc.password != "correct horse" {
		t.Errorf("service got %q/%q", svc.email, svc.password)
	}
	body := decode[map[string]any](t, rec)
	want := map[string]any{
		"id":         sampleUser.ID.String(),
		"email":      "Ann@example.com",
		"role":       "customer",
		"created_at": "2026-09-26T10:00:00Z",
	}
	if len(body) != len(want) {
		t.Errorf("body = %v, want exactly %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	if strings.Contains(rec.Body.String(), "secret-hash") {
		t.Error("the response leaks the password hash")
	}
}

func TestRegisterErrors(t *testing.T) {
	t.Parallel()

	var v domain.Violations
	v.Add("password", "must be at least 8 characters")
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "invalid input", err: v.Err(), status: http.StatusBadRequest, code: problem.CodeValidationFailed},
		{
			name: "email taken", err: domain.Conflict(domain.CodeEmailTaken, "taken"),
			status: http.StatusConflict, code: domain.CodeEmailTaken,
		},
		{name: "database down", err: errors.New("dial tcp: refused"), status: http.StatusInternalServerError, code: problem.CodeInternal},
	}
	for _, tt := range tests {
		rec := postJSON(t, newAuthHandler(&stubAuth{err: tt.err}, &stubSessions{}).Register, "/v1/auth/register",
			`{"email":"ann@example.com","password":"x"}`)
		assertProblem(t, rec, tt.status, tt.code)
	}
}

func TestRegisterRejectsMalformedBodyBeforeTheService(t *testing.T) {
	t.Parallel()

	svc := &stubAuth{user: sampleUser}
	rec := postJSON(t, newAuthHandler(svc, &stubSessions{}).Register, "/v1/auth/register",
		`{"email":"ann@example.com","password":"correct horse","role":"admin"}`)
	assertProblem(t, rec, http.StatusBadRequest, problem.CodeMalformedBody)
	if svc.email != "" {
		t.Error("the service was called with a body that sets an unknown field")
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{grant: sampleGrant()}
	h := NewAuth(&stubAuth{}, sessions, AuthConfig{
		Cookie:         testCookie,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}, newTestMetrics(), slog.New(slog.DiscardHandler))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/login",
		strings.NewReader(`{"email":"ann@example.com","password":"correct horse"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Firefox/140")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.RemoteAddr = "10.0.0.2:41000" // a trusted proxy
	rec := httptest.NewRecorder()
	h.Login(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if sessions.email != "ann@example.com" || sessions.password != "correct horse" {
		t.Errorf("service got %q/%q", sessions.email, sessions.password)
	}
	if want := (auth.Client{UserAgent: "Firefox/140", IP: netip.MustParseAddr("198.51.100.7")}); sessions.client != want {
		t.Errorf("client = %+v, want %+v", sessions.client, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	body := decode[map[string]any](t, rec)
	if body["access_token"] != "signed.jwt.token" || body["token_type"] != "Bearer" || body["expires_in"] != 900.0 {
		t.Errorf("body = %v", body)
	}
	if user, _ := body["user"].(map[string]any); user["id"] != sampleUser.ID.String() || user["email"] != "Ann@example.com" {
		t.Errorf("user = %v, want the account", body["user"])
	}
	if strings.Contains(rec.Body.String(), sessions.grant.Refresh.String()) {
		t.Error("the refresh token is in the body; it belongs in the HttpOnly cookie only")
	}

	c := refreshCookie(t, rec)
	if c.Value != sessions.grant.Refresh.String() {
		t.Errorf("cookie value = %q, want the refresh token", c.Value)
	}
	if c.Path != "/v1/auth" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Domain != "" {
		t.Errorf("Set-Cookie = %q, want HttpOnly, Secure, SameSite=Strict, Path=/v1/auth, no Domain",
			rec.Header().Get("Set-Cookie"))
	}
	if week := 7 * 24 * 3600; c.MaxAge < week-5 || c.MaxAge > week {
		t.Errorf("Max-Age = %d, want the time until the session expires (%d s)", c.MaxAge, week)
	}
}

func TestLoginErrors(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{err: domain.Unauthenticated(domain.CodeInvalidCredentials, "no"), status: http.StatusUnauthorized, code: domain.CodeInvalidCredentials},
		{err: errors.New("sign failed"), status: http.StatusInternalServerError, code: problem.CodeInternal},
	} {
		sessions := &stubSessions{grant: sampleGrant(), err: tt.err}
		rec := postJSON(t, newAuthHandler(&stubAuth{}, sessions).Login, "/v1/auth/login",
			`{"email":"ann@example.com","password":"wrong"}`)
		assertProblem(t, rec, tt.status, tt.code)
		if tt.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("a 401 must carry a WWW-Authenticate challenge")
		}
		if rec.Header().Get("Set-Cookie") != "" || strings.Contains(rec.Body.String(), "signed.jwt.token") {
			t.Errorf("a failed login handed out tokens: %v %s", rec.Header(), rec.Body.String())
		}
	}
}

func TestLoginWithoutSecureCookies(t *testing.T) {
	t.Parallel()

	h := NewAuth(&stubAuth{}, &stubSessions{grant: sampleGrant()}, AuthConfig{Cookie: RefreshCookie{Path: "/api/auth"}},
		newTestMetrics(), slog.New(slog.DiscardHandler))
	rec := postJSON(t, h.Login, "/v1/auth/login", `{"email":"ann@example.com","password":"correct horse"}`)
	if c := refreshCookie(t, rec); c.Secure || c.Path != "/api/auth" || !c.HttpOnly {
		t.Errorf("Set-Cookie = %q, want HttpOnly without Secure on /api/auth", rec.Header().Get("Set-Cookie"))
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()

	m := newTestMetrics()
	sessions := &stubSessions{grant: sampleGrant(), result: auth.RefreshGrace}
	h := NewAuth(&stubAuth{}, sessions, AuthConfig{Cookie: testCookie}, m, slog.New(slog.DiscardHandler))

	rec := postWithCookie(t, h.Refresh, "/v1/auth/refresh", `{}`, "the-old-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if sessions.token != "the-old-token" || sessions.client.UserAgent != "Firefox/140" {
		t.Errorf("service got token %q and client %+v", sessions.token, sessions.client)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the token response may be cached")
	}
	if body := decode[map[string]any](t, rec); body["access_token"] != "signed.jwt.token" || body["user"] == nil {
		t.Errorf("body = %v", body)
	}
	if c := refreshCookie(t, rec); c.Value != sessions.grant.Refresh.String() || !c.HttpOnly {
		t.Errorf("Set-Cookie = %q, want the current refresh token", rec.Header().Get("Set-Cookie"))
	}
	if got := testutil.ToFloat64(m.AuthRefreshes.WithLabelValues(metrics.RefreshGrace)); got != 1 {
		t.Errorf("grace refreshes counted = %v, want 1", got)
	}
}

func TestRefreshFailures(t *testing.T) {
	t.Parallel()

	refused := domain.Unauthenticated(domain.CodeRefreshInvalid, "log in again")
	for _, tt := range []struct {
		name        string
		token       string
		result      auth.RefreshResult
		err         error
		status      int
		code        string
		clears      bool
		wantCounted string
	}{
		{name: "no cookie", status: http.StatusUnauthorized, code: domain.CodeRefreshInvalid, clears: true,
			wantCounted: metrics.RefreshInvalid},
		{name: "reuse", token: "t", result: auth.RefreshReuseDetected, err: refused, status: http.StatusUnauthorized,
			code: domain.CodeRefreshInvalid, clears: true, wantCounted: metrics.RefreshReuseDetected},
		{name: "expired", token: "t", result: auth.RefreshExpired, err: refused, status: http.StatusUnauthorized,
			code: domain.CodeRefreshInvalid, clears: true, wantCounted: metrics.RefreshExpired},
		// A server error says nothing about the token, so the client keeps its cookie and may retry.
		{name: "database down", token: "t", err: errors.New("connection refused"),
			status: http.StatusInternalServerError, code: problem.CodeInternal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestMetrics()
			sessions := &stubSessions{result: tt.result, err: tt.err}
			h := NewAuth(&stubAuth{}, sessions, AuthConfig{Cookie: testCookie}, m, slog.New(slog.DiscardHandler))
			rec := postWithCookie(t, h.Refresh, "/v1/auth/refresh", `{}`, tt.token)

			assertProblem(t, rec, tt.status, tt.code)
			if tt.clears {
				wantCookieCleared(t, rec)
			} else if rec.Header().Get("Set-Cookie") != "" {
				t.Errorf("Set-Cookie = %q, want the cookie left alone", rec.Header().Get("Set-Cookie"))
			}
			if tt.token == "" && sessions.calls != 0 {
				t.Error("the service was called without a token")
			}
			var counted float64
			for _, label := range refreshResults {
				counted += testutil.ToFloat64(m.AuthRefreshes.WithLabelValues(label))
			}
			if tt.wantCounted == "" && counted != 0 {
				t.Errorf("%v refreshes counted, want none for a server error", counted)
			}
			if tt.wantCounted != "" && (counted != 1 || testutil.ToFloat64(m.AuthRefreshes.WithLabelValues(tt.wantCounted)) != 1) {
				t.Errorf("counted %v refreshes, want one %s", counted, tt.wantCounted)
			}
		})
	}
}

// TestCookieRoutesRequireJSON: refresh and logout are authenticated by a cookie that the browser attaches by
// itself, so they must not be reachable by a cross-site form, which cannot send application/json.
func TestCookieRoutesRequireJSON(t *testing.T) {
	t.Parallel()

	for name, h := range map[string]func(*Auth) http.HandlerFunc{
		"refresh": func(a *Auth) http.HandlerFunc { return a.Refresh },
		"logout":  func(a *Auth) http.HandlerFunc { return a.Logout },
	} {
		for _, tt := range []struct {
			contentType, body string
			status            int
			code              string
		}{
			{contentType: "application/x-www-form-urlencoded", body: "a=1", status: http.StatusUnsupportedMediaType, code: problem.CodeUnsupportedMediaType},
			{contentType: "text/plain", body: "{}", status: http.StatusUnsupportedMediaType, code: problem.CodeUnsupportedMediaType},
			{contentType: "application/json", body: "", status: http.StatusBadRequest, code: problem.CodeMalformedBody},
			{contentType: "application/json", body: `{"token":"x"}`, status: http.StatusBadRequest, code: problem.CodeMalformedBody},
		} {
			sessions := &stubSessions{grant: sampleGrant()}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/"+name, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			req.AddCookie(&http.Cookie{Name: RefreshCookieName, Value: "token"})
			rec := httptest.NewRecorder()
			h(newAuthHandler(&stubAuth{}, sessions))(rec, req)

			assertProblem(t, rec, tt.status, tt.code)
			if sessions.calls != 0 {
				t.Errorf("%s with %s %q reached the service", name, tt.contentType, tt.body)
			}
		}
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	rec := postWithCookie(t, newAuthHandler(&stubAuth{}, sessions).Logout, "/v1/auth/logout", `{}`, "the-token")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if sessions.token != "the-token" {
		t.Errorf("service got %q, want the cookie's token", sessions.token)
	}
	wantCookieCleared(t, rec)

	// Without a cookie there is nothing to end, and the answer is the same.
	sessions = &stubSessions{}
	rec = postWithCookie(t, newAuthHandler(&stubAuth{}, sessions).Logout, "/v1/auth/logout", `{}`, "")
	if rec.Code != http.StatusNoContent || sessions.calls != 0 {
		t.Errorf("logout without a cookie = %d with %d service calls, want 204 and none", rec.Code, sessions.calls)
	}
	wantCookieCleared(t, rec)

	// The session may still exist, so the client learns of the failure, but its cookie is deleted anyway.
	rec = postWithCookie(t, newAuthHandler(&stubAuth{}, &stubSessions{err: errors.New("connection refused")}).Logout,
		"/v1/auth/logout", `{}`, "the-token")
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
	wantCookieCleared(t, rec)
}

func TestLogoutAll(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{}
	h := newAuthHandler(&stubAuth{}, sessions)
	rec := httptest.NewRecorder()
	h.LogoutAll(rec, asSampleUser(t, http.MethodPost, "/v1/auth/logout-all", ""))
	if rec.Code != http.StatusNoContent || sessions.userID != sampleUser.ID {
		t.Fatalf("logout-all = %d for user %s, want 204 for %s", rec.Code, sessions.userID, sampleUser.ID)
	}
	wantCookieCleared(t, rec)

	rec = httptest.NewRecorder()
	newAuthHandler(&stubAuth{}, &stubSessions{err: errors.New("connection refused")}).
		LogoutAll(rec, asSampleUser(t, http.MethodPost, "/v1/auth/logout-all", ""))
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)

	rec = httptest.NewRecorder()
	h.LogoutAll(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/logout-all", nil))
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal) // routed without Authenticate
}

func TestEveryRefreshResultHasAMetricLabel(t *testing.T) {
	t.Parallel()

	for _, r := range []auth.RefreshResult{
		auth.RefreshRotated, auth.RefreshReissued, auth.RefreshGrace,
		auth.RefreshReuseDetected, auth.RefreshExpired, auth.RefreshInvalid,
	} {
		if refreshResults[r] != string(r) {
			t.Errorf("result %s is counted as %q", r, refreshResults[r])
		}
	}
}

func TestMe(t *testing.T) {
	t.Parallel()

	svc := &stubAuth{user: sampleUser}
	h := newAuthHandler(svc, &stubSessions{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil)
	req = req.WithContext(principal.NewContext(req.Context(),
		domain.Principal{UserID: sampleUser.ID, Role: domain.RoleCustomer}))
	rec := httptest.NewRecorder()
	h.Me(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.userID != sampleUser.ID {
		t.Errorf("looked up user %s, want %s", svc.userID, sampleUser.ID)
	}
	if body := decode[map[string]any](t, rec); body["email"] != "Ann@example.com" {
		t.Errorf("body = %v", body)
	}
}

func TestMeWithoutPrincipalIsAServerError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newAuthHandler(&stubAuth{user: sampleUser}, &stubSessions{}).Me(rec,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil))
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
}
