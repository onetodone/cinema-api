package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"
)

// Session limits.
const (
	// RefreshSecretBytes is the entropy of a refresh token's secret: 256 bits. That is far beyond guessing, so a
	// fast hash is enough to store it, unlike a password.
	RefreshSecretBytes = 32
	// MaxUserAgentBytes bounds the user agent a session records.
	MaxUserAgentBytes = 512
)

// refreshSecretLen is the length of a secret in unpadded base64url.
var refreshSecretLen = base64.RawURLEncoding.EncodedLen(RefreshSecretBytes)

// Session is one login: a browser or device that holds a refresh token. Its id is stable for the session's life
// and is the sid claim of every access token the session issues; its refresh token changes on every rotation.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	UserAgent string
	IP        netip.Addr // the zero Addr when unknown
	// Generation counts the rotations of the refresh token.
	Generation int
	CreatedAt  time.Time
	LastUsedAt time.Time
	// ExpiresAt is when the session ends unless its refresh token is rotated before: the idle lifetime after the
	// last rotation, but never later than the maximum age after the login.
	ExpiresAt time.Time
}

// RefreshToken is the credential of a session, "<session id>.<secret>". The id says which session to look up;
// only the SHA-256 of the secret is stored, so the token cannot be recovered from the database.
type RefreshToken struct {
	SessionID uuid.UUID
	Secret    [RefreshSecretBytes]byte
}

// NewRefreshToken returns a token for the session with a new random secret.
func NewRefreshToken(sessionID uuid.UUID) RefreshToken {
	t := RefreshToken{SessionID: sessionID}
	_, _ = rand.Read(t.Secret[:]) // never fails: crypto/rand crashes the program rather than return an error
	return t
}

// ParseRefreshToken parses a token in exactly the form String writes: a canonical lowercase UUID, a dot, and 43
// characters of unpadded base64url. Anything else is not a token, and is rejected before any database lookup.
func ParseRefreshToken(s string) (RefreshToken, bool) {
	id, secret, ok := strings.Cut(s, ".")
	if !ok || len(secret) != refreshSecretLen {
		return RefreshToken{}, false
	}
	sessionID, err := uuid.Parse(id)
	// uuid.Parse also accepts braces, a urn: prefix, missing hyphens, and upper case; a token has one spelling.
	if err != nil || sessionID.String() != id {
		return RefreshToken{}, false
	}

	t := RefreshToken{SessionID: sessionID}
	// Strict rejects a last character with bits set beyond the 32 bytes, so every secret has exactly one spelling.
	n, err := base64.RawURLEncoding.Strict().Decode(t.Secret[:], []byte(secret))
	if err != nil || n != RefreshSecretBytes {
		return RefreshToken{}, false
	}
	return t, true
}

// String returns the token as clients hold it.
func (t RefreshToken) String() string {
	return t.SessionID.String() + "." + base64.RawURLEncoding.EncodeToString(t.Secret[:])
}

// Hash returns the SHA-256 of the secret, the form in which sessions store their tokens.
func (t RefreshToken) Hash() []byte {
	sum := sha256.Sum256(t.Secret[:])
	return sum[:]
}

// CleanUserAgent makes a User-Agent header fit for storage and display: valid UTF-8 without control characters,
// at most MaxUserAgentBytes long, and cut only between characters.
func CleanUserAgent(ua string) string {
	ua = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(ua, ""))
	ua = strings.TrimSpace(ua)
	if len(ua) <= MaxUserAgentBytes {
		return ua
	}
	cut := MaxUserAgentBytes
	for cut > 0 && !utf8.RuneStart(ua[cut]) {
		cut--
	}
	return ua[:cut]
}
