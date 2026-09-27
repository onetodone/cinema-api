package auth

import (
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/golang-jwt/jwt/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

const (
	// MinSecretBytes is the shortest HS256 key RFC 7518 §3.2 allows: as long as the hash output.
	MinSecretBytes = 32

	// tokenIssuer is both the issuer and the audience: the API issues tokens for itself.
	tokenIssuer = "cinema-api"
	// clockLeeway absorbs clock differences between API replicas when exp and iat are checked.
	clockLeeway = 30 * time.Second
)

// signingMethod is the only algorithm the verifier accepts. Pinning it blocks "alg: none" and key-confusion
// attacks, where a forged header picks a weaker or different algorithm.
var signingMethod = jwt.SigningMethodHS256

// claims is the payload of an access token.
type claims struct {
	Role domain.Role `json:"role"`
	// SessionID is the session that issued the token. Every token has one; a token without it is rejected.
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// AccessToken is a signed bearer token.
type AccessToken struct {
	Token     string
	ExpiresIn time.Duration
}

// Tokens issues and verifies stateless access tokens (JWT, HS256). A token carries the user id, the role, and the
// session that issued it, so verifying it needs no database access. The price is that a role change applies to
// new tokens only; old ones keep the old role until they expire (JWT_TTL). The tokens of a session that ended are
// stopped by the revocation list (see Sessions), which the auth middleware checks after Verify.
type Tokens struct {
	key    []byte
	ttl    time.Duration
	now    func() time.Time
	parser *jwt.Parser
}

// TokenOption customizes Tokens.
type TokenOption func(*Tokens)

// WithTokenClock replaces time.Now, for tests.
func WithTokenClock(now func() time.Time) TokenOption {
	return func(t *Tokens) { t.now = now }
}

// NewTokens returns Tokens that sign with secret and issue tokens valid for ttl.
func NewTokens(secret string, ttl time.Duration, opts ...TokenOption) (*Tokens, error) {
	switch {
	case secret == "":
		return nil, errors.New("signing secret is empty")
	case len(secret) < MinSecretBytes:
		return nil, fmt.Errorf("signing secret must be at least %d bytes, got %d", MinSecretBytes, len(secret))
	case ttl <= 0:
		return nil, fmt.Errorf("token lifetime must be positive, got %s", ttl)
	}

	t := &Tokens{key: []byte(secret), ttl: ttl, now: time.Now}
	for _, opt := range opts {
		opt(t)
	}
	t.parser = jwt.NewParser(
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithAudience(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockLeeway),
		jwt.WithTimeFunc(t.now),
		jwt.WithStrictDecoding(),
	)
	return t, nil
}

// Issue signs a token for p, which must name the session that the token belongs to.
func (t *Tokens) Issue(p domain.Principal) (AccessToken, error) {
	if p.SessionID == uuid.Nil() {
		return AccessToken{}, errors.New("sign access token: the principal has no session")
	}
	now := t.now()
	c := claims{
		Role:      p.Role,
		SessionID: p.SessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   p.UserID.String(),
			Audience:  jwt.ClaimStrings{tokenIssuer},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
			ID:        uuid.NewV7().String(), // makes every token unique; a revocation list could key on it
		},
	}
	signed, err := jwt.NewWithClaims(signingMethod, c).SignedString(t.key)
	if err != nil {
		return AccessToken{}, fmt.Errorf("sign access token: %w", err)
	}
	return AccessToken{Token: signed, ExpiresIn: t.ttl}, nil
}

// Verify checks the signature, algorithm, issuer, audience, and lifetime of raw and returns the caller it
// identifies. It fails with TOKEN_EXPIRED for an expired token and INVALID_TOKEN for anything else; the
// returned error also wraps the underlying reason, for logs.
func (t *Tokens) Verify(raw string) (domain.Principal, error) {
	var c claims
	if _, err := t.parser.ParseWithClaims(raw, &c, t.signingKey); err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return domain.Principal{}, fmt.Errorf("%w: %w",
				domain.Unauthenticated(domain.CodeTokenExpired, "the access token has expired"), err)
		}
		return domain.Principal{}, invalidToken(err)
	}

	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return domain.Principal{}, invalidToken(fmt.Errorf("subject %q is not a user id: %w", c.Subject, err))
	}
	if !c.Role.Valid() {
		return domain.Principal{}, invalidToken(fmt.Errorf("unknown role %q", c.Role))
	}
	sid, err := uuid.Parse(c.SessionID)
	if err != nil || sid == uuid.Nil() {
		// Tokens issued before sessions existed have no sid. They are refused rather than grandfathered: they
		// cannot be revoked, and they expire within JWT_TTL anyway.
		return domain.Principal{}, invalidToken(fmt.Errorf("sid %q is not a session id", c.SessionID))
	}
	return domain.Principal{UserID: id, Role: c.Role, SessionID: sid}, nil
}

// signingKey is the jwt.Keyfunc. The parser has already pinned the algorithm, so the HMAC key is always right.
func (t *Tokens) signingKey(*jwt.Token) (any, error) {
	return t.key, nil
}

func invalidToken(cause error) error {
	return fmt.Errorf("%w: %w", domain.Unauthenticated(domain.CodeInvalidToken, "the access token is invalid"), cause)
}
