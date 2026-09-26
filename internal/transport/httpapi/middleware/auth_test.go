package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

var (
	customer = domain.Principal{UserID: uuid.MustParse("01920000-0000-7000-8000-000000000001"), Role: domain.RoleCustomer}
	admin    = domain.Principal{UserID: uuid.MustParse("01920000-0000-7000-8000-000000000002"), Role: domain.RoleAdmin}
)

// stubVerifier accepts the tokens it knows and rejects everything else like auth.Tokens would.
type stubVerifier map[string]domain.Principal

func (s stubVerifier) Verify(token string) (domain.Principal, error) {
	if token == "expired" {
		return domain.Principal{}, fmt.Errorf("%w: token is expired",
			domain.Unauthenticated(domain.CodeTokenExpired, "the access token has expired"))
	}
	if p, ok := s[token]; ok {
		return p, nil
	}
	return domain.Principal{}, fmt.Errorf("%w: token signature is invalid",
		domain.Unauthenticated(domain.CodeInvalidToken, "the access token is invalid"))
}

var verifier = stubVerifier{"customer-token": customer, "admin-token": admin}

// whoAmI answers 200 with the principal it finds in the context.
var whoAmI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	_, _ = w.Write([]byte(p.UserID.String() + " " + string(p.Role)))
})

func request(t *testing.T, h http.Handler, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Fatalf("Content-Type = %q, want problem details (body %s)", ct, rec.Body.String())
	}
	var p problem.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.Code
}

func TestAuthenticateAcceptsValidBearerToken(t *testing.T) {
	t.Parallel()

	h := Authenticate(verifier, slog.New(slog.DiscardHandler))(whoAmI)
	for _, header := range []string{"Bearer customer-token", "bearer customer-token", "BEARER  customer-token "} {
		rec := request(t, h, header)
		if rec.Code != http.StatusOK || rec.Body.String() != customer.UserID.String()+" customer" {
			t.Errorf("%q: got %d %q, want the customer principal in the context", header, rec.Code, rec.Body.String())
		}
	}
}

func TestAuthenticateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		code      string
		challenge string
	}{
		{name: "no header", header: "", code: domain.CodeUnauthenticated, challenge: problem.BearerChallenge},
		{
			name: "basic auth", header: "Basic YWRtaW46YWRtaW4=",
			code: domain.CodeUnauthenticated, challenge: problem.BearerChallenge,
		},
		{
			name: "scheme glued to token", header: "Bearercustomer-token",
			code: domain.CodeUnauthenticated, challenge: problem.BearerChallenge,
		},
		{name: "empty token", header: "Bearer ", code: domain.CodeInvalidToken, challenge: invalidTokenChallenge},
		{name: "unknown token", header: "Bearer forged", code: domain.CodeInvalidToken, challenge: invalidTokenChallenge},
		{name: "expired token", header: "Bearer expired", code: domain.CodeTokenExpired, challenge: invalidTokenChallenge},
	}

	var logs bytes.Buffer
	h := Authenticate(verifier, newJSONLogger(t, &logs))(whoAmI)
	for _, tt := range tests {
		rec := request(t, h, tt.header)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tt.name, rec.Code)
			continue
		}
		if got := problemCode(t, rec); got != tt.code {
			t.Errorf("%s: code = %s, want %s", tt.name, got, tt.code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != tt.challenge {
			t.Errorf("%s: WWW-Authenticate = %q, want %q", tt.name, got, tt.challenge)
		}
	}

	if !bytes.Contains(logs.Bytes(), []byte("token signature is invalid")) {
		t.Errorf("the rejection reason was not logged: %s", logs.String())
	}
}

func TestAuthenticateTagsLogsWithUserID(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := newJSONLogger(t, &logs)
	h := Authenticate(verifier, logger)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "inside handler")
	}))
	request(t, h, "Bearer customer-token")

	if rec := decodeLogLine(t, &logs); rec["user_id"] != customer.UserID.String() {
		t.Errorf("log record = %v, want user_id %s", rec, customer.UserID)
	}
}

func TestRequireRole(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	adminOnly := Chain(whoAmI, Authenticate(verifier, logger), RequireRole(domain.RoleAdmin))

	rec := request(t, adminOnly, "Bearer admin-token")
	if rec.Code != http.StatusOK {
		t.Errorf("admin: status = %d, want 200", rec.Code)
	}

	rec = request(t, adminOnly, "Bearer customer-token")
	if rec.Code != http.StatusForbidden || problemCode(t, rec) != domain.CodeForbidden {
		t.Errorf("customer: got %d %s, want 403 FORBIDDEN", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != insufficientScopeChallenge {
		t.Errorf("customer: WWW-Authenticate = %q, want %q", got, insufficientScopeChallenge)
	}

	rec = request(t, adminOnly, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401 before any role check", rec.Code)
	}

	anyRole := Chain(whoAmI, Authenticate(verifier, logger), RequireRole(domain.RoleCustomer, domain.RoleAdmin))
	if rec := request(t, anyRole, "Bearer customer-token"); rec.Code != http.StatusOK {
		t.Errorf("customer on a route for customers and admins: status = %d, want 200", rec.Code)
	}
}

func TestRequireRoleWithoutAuthenticateFailsClosed(t *testing.T) {
	t.Parallel()

	rec := request(t, RequireRole(domain.RoleAdmin)(whoAmI), "Bearer admin-token")
	if rec.Code != http.StatusUnauthorized || problemCode(t, rec) != domain.CodeUnauthenticated {
		t.Errorf("got %d %s, want 401: a miswired route must not open up", rec.Code, rec.Body.String())
	}
}
