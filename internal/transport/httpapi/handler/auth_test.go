package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
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

func (s *stubAuth) Authenticate(_ context.Context, email, password string) (domain.User, error) {
	s.email, s.password = email, password
	return s.user, s.err
}

func (s *stubAuth) User(_ context.Context, id uuid.UUID) (domain.User, error) {
	s.userID = id
	return s.user, s.err
}

// stubIssuer returns a fixed token for any principal.
type stubIssuer struct {
	got domain.Principal
	err error
}

func (s *stubIssuer) Issue(p domain.Principal) (auth.AccessToken, error) {
	s.got = p
	return auth.AccessToken{Token: "signed.jwt.token", ExpiresIn: time.Hour}, s.err
}

func postJSON(t *testing.T, h http.HandlerFunc, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func newAuthHandler(svc AuthService, issuer TokenIssuer) *Auth {
	return NewAuth(svc, issuer, slog.New(slog.DiscardHandler))
}

func TestRegister(t *testing.T) {
	t.Parallel()

	svc := &stubAuth{user: sampleUser}
	rec := postJSON(t, newAuthHandler(svc, &stubIssuer{}).Register, "/v1/auth/register",
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
		rec := postJSON(t, newAuthHandler(&stubAuth{err: tt.err}, &stubIssuer{}).Register, "/v1/auth/register",
			`{"email":"ann@example.com","password":"x"}`)
		assertProblem(t, rec, tt.status, tt.code)
	}
}

func TestRegisterRejectsMalformedBodyBeforeTheService(t *testing.T) {
	t.Parallel()

	svc := &stubAuth{user: sampleUser}
	rec := postJSON(t, newAuthHandler(svc, &stubIssuer{}).Register, "/v1/auth/register",
		`{"email":"ann@example.com","password":"correct horse","role":"admin"}`)
	assertProblem(t, rec, http.StatusBadRequest, problem.CodeMalformedBody)
	if svc.email != "" {
		t.Error("the service was called with a body that sets an unknown field")
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	admin := sampleUser
	admin.Role = domain.RoleAdmin
	issuer := &stubIssuer{}
	rec := postJSON(t, newAuthHandler(&stubAuth{user: admin}, issuer).Login, "/v1/auth/login",
		`{"email":"ann@example.com","password":"correct horse"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if issuer.got != (domain.Principal{UserID: admin.ID, Role: domain.RoleAdmin}) {
		t.Errorf("token issued for %+v", issuer.got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := decode[map[string]any](t, rec)
	if body["access_token"] != "signed.jwt.token" || body["token_type"] != "Bearer" || body["expires_in"] != 3600.0 {
		t.Errorf("body = %v", body)
	}
}

func TestLoginErrors(t *testing.T) {
	t.Parallel()

	rec := postJSON(t, newAuthHandler(&stubAuth{err: domain.Unauthenticated(domain.CodeInvalidCredentials, "no")},
		&stubIssuer{}).Login, "/v1/auth/login", `{"email":"ann@example.com","password":"wrong"}`)
	assertProblem(t, rec, http.StatusUnauthorized, domain.CodeInvalidCredentials)
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 must carry a WWW-Authenticate challenge")
	}

	rec = postJSON(t, newAuthHandler(&stubAuth{user: sampleUser}, &stubIssuer{err: errors.New("sign failed")}).Login,
		"/v1/auth/login", `{"email":"ann@example.com","password":"correct horse"}`)
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
	if strings.Contains(rec.Body.String(), "signed.jwt.token") {
		t.Error("a token was sent although issuing failed")
	}
}

func TestMe(t *testing.T) {
	t.Parallel()

	svc := &stubAuth{user: sampleUser}
	h := newAuthHandler(svc, &stubIssuer{})

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
	newAuthHandler(&stubAuth{user: sampleUser}, &stubIssuer{}).Me(rec,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil))
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
}
