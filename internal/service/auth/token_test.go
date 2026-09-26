package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// testSessionID is the session of the tokens the tests sign by hand.
var testSessionID = uuid.MustParse("0199a1f0-7c1e-7d2a-9b3e-5f0c2d1e4a77")

// clock is a settable time source.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newTokens(t *testing.T, c *clock) *Tokens {
	t.Helper()
	tokens, err := NewTokens(testSecret, time.Hour, WithTokenClock(c.Now))
	if err != nil {
		t.Fatalf("NewTokens: %v", err)
	}
	return tokens
}

// validClaims are the claims Issue would produce at t0 for a customer.
func validClaims(userID uuid.UUID) claims {
	return claims{
		Role:      domain.RoleCustomer,
		SessionID: testSessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{tokenIssuer},
			IssuedAt:  jwt.NewNumericDate(t0),
			ExpiresAt: jwt.NewNumericDate(t0.Add(time.Hour)),
		},
	}
}

func sign(t *testing.T, method jwt.SigningMethod, key any, c jwt.Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, c).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func TestNewTokensRejectsWeakSettings(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		secret string
		ttl    time.Duration
	}{
		"empty secret": {secret: "", ttl: time.Hour},
		"short secret": {secret: strings.Repeat("s", MinSecretBytes-1), ttl: time.Hour},
		"zero ttl":     {secret: testSecret, ttl: 0},
	}
	for name, tt := range tests {
		if _, err := NewTokens(tt.secret, tt.ttl); err == nil {
			t.Errorf("%s: NewTokens succeeded", name)
		}
	}
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	tokens := newTokens(t, &clock{now: t0})
	want := domain.Principal{UserID: uuid.NewV7(), Role: domain.RoleAdmin, SessionID: uuid.NewV7()}

	tok, err := tokens.Issue(want)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.ExpiresIn != time.Hour {
		t.Errorf("ExpiresIn = %s, want 1h", tok.ExpiresIn)
	}

	got, err := tokens.Verify(tok.Token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != want {
		t.Errorf("principal = %+v, want %+v", got, want)
	}
}

func TestIssuedTokenContents(t *testing.T) {
	t.Parallel()

	tokens := newTokens(t, &clock{now: t0})
	userID := uuid.NewV7()
	tok, err := tokens.Issue(domain.Principal{UserID: userID, Role: domain.RoleCustomer, SessionID: testSessionID})
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(tok.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	decodePart := func(s string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	if header := decodePart(parts[0]); header["alg"] != "HS256" || header["typ"] != "JWT" {
		t.Errorf("header = %v", header)
	}
	payload := decodePart(parts[1])
	want := map[string]any{
		"iss":  "cinema-api",
		"sub":  userID.String(),
		"role": "customer",
		"sid":  testSessionID.String(),
		"iat":  float64(t0.Unix()),
		"exp":  float64(t0.Add(time.Hour).Unix()),
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("claim %s = %v, want %v", k, payload[k], v)
		}
	}
	if aud, _ := payload["aud"].([]any); len(aud) != 1 || aud[0] != "cinema-api" {
		t.Errorf("aud = %v, want [cinema-api]", payload["aud"])
	}
	if jti, _ := payload["jti"].(string); jti == "" {
		t.Error("jti is missing")
	}
	if _, ok := payload["email"]; ok {
		t.Error("the token must not carry personal data such as the email")
	}
}

func TestEveryTokenIsUnique(t *testing.T) {
	t.Parallel()

	tokens := newTokens(t, &clock{now: t0})
	p := domain.Principal{UserID: uuid.NewV7(), Role: domain.RoleCustomer, SessionID: uuid.NewV7()}
	a, _ := tokens.Issue(p)
	b, _ := tokens.Issue(p)
	if a.Token == b.Token {
		t.Error("two tokens for the same caller in the same second are identical")
	}
}

func TestVerifyExpiry(t *testing.T) {
	t.Parallel()

	c := &clock{now: t0}
	tokens := newTokens(t, c)
	tok, err := tokens.Issue(domain.Principal{UserID: uuid.NewV7(), Role: domain.RoleCustomer, SessionID: uuid.NewV7()})
	if err != nil {
		t.Fatal(err)
	}

	c.now = t0.Add(time.Hour + clockLeeway - time.Second)
	if _, err := tokens.Verify(tok.Token); err != nil {
		t.Errorf("within the clock leeway after expiry: %v", err)
	}

	c.now = t0.Add(time.Hour + clockLeeway + time.Second)
	_, err = tokens.Verify(tok.Token)
	if code(err) != domain.CodeTokenExpired || !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("after expiry: error = %v, want TOKEN_EXPIRED", err)
	}
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Errorf("the error should wrap the underlying reason for logs, got %v", err)
	}
}

func TestVerifyRejectsForgedAndMalformedTokens(t *testing.T) {
	t.Parallel()

	tokens := newTokens(t, &clock{now: t0})
	userID := uuid.NewV7()
	key := []byte(testSecret)

	withClaims := func(edit func(*claims)) string {
		c := validClaims(userID)
		edit(&c)
		return sign(t, jwt.SigningMethodHS256, key, c)
	}

	valid := withClaims(func(*claims) {})
	if _, err := tokens.Verify(valid); err != nil {
		t.Fatalf("the unmodified test token must verify: %v", err)
	}

	// Promote the payload to admin but keep the customer token's signature.
	parts := strings.Split(valid, ".")
	adminClaims := validClaims(userID)
	adminClaims.Role = domain.RoleAdmin
	adminPayload, _ := json.Marshal(adminClaims)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(adminPayload) + "." + parts[2]

	unsigned := sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims(userID))

	tests := map[string]string{
		"empty":                   "",
		"garbage":                 "not.a.jwt",
		"tampered payload":        tampered,
		"alg none":                unsigned,
		"other hmac algorithm":    sign(t, jwt.SigningMethodHS512, key, validClaims(userID)),
		"other secret":            sign(t, jwt.SigningMethodHS256, []byte(strings.Repeat("x", 32)), validClaims(userID)),
		"other issuer":            withClaims(func(c *claims) { c.Issuer = "someone-else" }),
		"other audience":          withClaims(func(c *claims) { c.Audience = jwt.ClaimStrings{"another-api"} }),
		"no expiry":               withClaims(func(c *claims) { c.ExpiresAt = nil }),
		"issued in the future":    withClaims(func(c *claims) { c.IssuedAt = jwt.NewNumericDate(t0.Add(time.Hour)) }),
		"subject is not a uuid":   withClaims(func(c *claims) { c.Subject = "42" }),
		"unknown role":            withClaims(func(c *claims) { c.Role = "root" }),
		"no session":              withClaims(func(c *claims) { c.SessionID = "" }),
		"session is not a uuid":   withClaims(func(c *claims) { c.SessionID = "session-1" }),
		"nil session":             withClaims(func(c *claims) { c.SessionID = uuid.Nil().String() }),
		"signature padded base64": valid + "=",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, err := tokens.Verify(raw)
			if code(err) != domain.CodeInvalidToken || !errors.Is(err, domain.ErrUnauthenticated) {
				t.Errorf("Verify = %+v, %v; want INVALID_TOKEN", p, err)
			}
		})
	}
}

func TestTokensBelongToASession(t *testing.T) {
	t.Parallel()

	tokens := newTokens(t, &clock{now: t0})
	if _, err := tokens.Issue(domain.Principal{UserID: uuid.NewV7(), Role: domain.RoleCustomer}); err == nil {
		t.Error("Issue signed a token without a session")
	}

	// A token as the API issued it before sessions existed: every claim but sid.
	userID := uuid.NewV7()
	legacy := struct {
		Role domain.Role `json:"role"`
		jwt.RegisteredClaims
	}{Role: domain.RoleCustomer, RegisteredClaims: validClaims(userID).RegisteredClaims}
	_, err := tokens.Verify(sign(t, jwt.SigningMethodHS256, []byte(testSecret), legacy))
	if code(err) != domain.CodeInvalidToken {
		t.Errorf("Verify of a token without sid = %v, want INVALID_TOKEN", err)
	}
}
