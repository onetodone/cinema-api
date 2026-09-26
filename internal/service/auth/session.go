package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
)

// errRefreshInvalid is the single answer to every failed refresh. Whether the token was unknown, expired, revoked,
// or reused is logged, never sent: the client must log in again in every case.
var errRefreshInvalid = domain.Unauthenticated(domain.CodeRefreshInvalid,
	"the refresh token is invalid or its session has ended; log in again")

// SessionRepository stores sessions. It is implemented by repository/postgres.Sessions. Every time it compares
// comes from the database clock, the one clock of all expiry decisions, so API replicas whose clocks disagree
// still agree on which sessions are over.
type SessionRepository interface {
	// Create stores s and deletes the user's least recently used sessions beyond s.MaxPerUser, in one
	// transaction that also locks the user's row, so concurrent logins cannot overshoot the cap. It returns the
	// stored session and the ids of the evicted ones. A user that no longer exists fails with ErrNotFound.
	Create(ctx context.Context, s NewSession) (created domain.Session, evicted []uuid.UUID, err error)
	// Get returns a session with its user, or ErrNotFound. inGrace is judged against the given grace window.
	Get(ctx context.Context, id uuid.UUID, grace time.Duration) (StoredSession, error)
	// Rotate replaces the refresh token of a session whose current token hash is still r.Presented and which has
	// not expired, as one compare-and-set. ok is false when another request rotated it first, or it is gone.
	Rotate(ctx context.Context, r Rotation) (s domain.Session, ok bool, err error)
	// Delete deletes a session. A session that does not exist is no error.
	Delete(ctx context.Context, id uuid.UUID) error
	// DeleteByToken deletes the session id if tokenHash is its current or its previous token hash, and reports
	// whether it did.
	DeleteByToken(ctx context.Context, id uuid.UUID, tokenHash []byte) (bool, error)
	// DeleteAllOfUser deletes every session of a user and returns their ids.
	DeleteAllOfUser(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// NewSession is a session to store at login.
type NewSession struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte
	Client    Client
	// IdleTTL and MaxAge set expires_at: the idle lifetime from now, but at most the maximum age from the login.
	IdleTTL time.Duration
	MaxAge  time.Duration
	// MaxPerUser is how many sessions the user may have, this one included.
	MaxPerUser int
}

// StoredSession is a session as a refresh finds it: with its user, its token hashes, and its state by the
// database clock.
type StoredSession struct {
	domain.Session
	User          domain.User
	TokenHash     []byte
	PrevTokenHash []byte // nil until the first rotation
	GraceToken    []byte // the current secret, sealed; nil until the first rotation
	Expired       bool   // expires_at has passed
	InGrace       bool   // the last rotation is less than the grace window ago
}

// Rotation replaces the refresh token of a session.
type Rotation struct {
	ID         uuid.UUID
	Presented  []byte // the hash that must still be current
	TokenHash  []byte // the hash of the new secret
	GraceToken []byte // the new secret, sealed
	Client     Client
	IdleTTL    time.Duration
	MaxAge     time.Duration
}

// SessionConfig holds the session rules that come from configuration.
type SessionConfig struct {
	// IdleTTL is how long a session lasts after its last rotation (REFRESH_TOKEN_TTL).
	IdleTTL time.Duration
	// MaxAge is how long a session lasts after its login at most, however often it is rotated (SESSION_MAX_AGE).
	MaxAge time.Duration
	// Grace is how long after a rotation the previous token still gets the current one (REFRESH_GRACE).
	Grace time.Duration
	// MaxPerUser is how many sessions a user may have at once (AUTH_MAX_SESSIONS_PER_USER).
	MaxPerUser int
}

// Client describes where a request comes from. A session records the latest one.
type Client struct {
	UserAgent string
	IP        netip.Addr // the zero Addr when unknown
}

// Grant is what a login or a refresh hands the client: an access token for the session, and the session's current
// refresh token.
type Grant struct {
	User    domain.User
	Session domain.Session
	Access  AccessToken
	Refresh domain.RefreshToken
}

// RefreshResult tells how a refresh was answered.
type RefreshResult string

// Refresh results. The first three answer with tokens, the others with REFRESH_INVALID.
const (
	RefreshRotated       RefreshResult = "rotated"        // the current token, rotated: a new refresh token
	RefreshReissued      RefreshResult = "reissued"       // the current token within the grace window: the same one again
	RefreshGrace         RefreshResult = "grace"          // the previous token within the grace window: the current one
	RefreshReuseDetected RefreshResult = "reuse_detected" // the previous token after the grace window: the session is revoked
	RefreshExpired       RefreshResult = "expired"        // the session is over; it is deleted
	RefreshInvalid       RefreshResult = "invalid"        // malformed, unknown, or of an older generation
)

// Sessions runs logins, refreshes, and logouts. A login starts a server-side session; the client gets a
// short-lived access token and a refresh token, and trades the refresh token for new tokens until the session
// ends. Every refresh rotates the refresh token, so a stolen token is useful only until its owner refreshes next,
// and a token presented again after the grace window gives the theft away: the session is revoked.
type Sessions struct {
	accounts *Service
	repo     SessionRepository
	tokens   *Tokens
	sealer   sealer
	cfg      SessionConfig
	logger   *slog.Logger
}

// NewSessions returns the session use cases. The keys that seal refresh secrets for the grace window are derived
// from the signing key of tokens.
func NewSessions(accounts *Service, repo SessionRepository, tokens *Tokens, cfg SessionConfig,
	logger *slog.Logger,
) (*Sessions, error) {
	switch {
	case cfg.IdleTTL <= 0 || cfg.Grace <= 0:
		return nil, fmt.Errorf("session idle lifetime and grace window must be positive, got %s and %s",
			cfg.IdleTTL, cfg.Grace)
	case cfg.MaxAge < cfg.IdleTTL:
		return nil, fmt.Errorf("session maximum age (%s) must be at least its idle lifetime (%s)", cfg.MaxAge, cfg.IdleTTL)
	case cfg.MaxPerUser < 1:
		return nil, fmt.Errorf("sessions per user must be at least 1, got %d", cfg.MaxPerUser)
	}
	return &Sessions{
		accounts: accounts,
		repo:     repo,
		tokens:   tokens,
		sealer:   sealer{secret: tokens.key},
		cfg:      cfg,
		logger:   logger,
	}, nil
}

// Login checks the credentials and starts a session for the client. If the user then has more sessions than
// allowed, the least recently used ones end. Failures are those of Service.Authenticate.
func (s *Sessions) Login(ctx context.Context, email, password string, c Client) (Grant, error) {
	u, err := s.accounts.Authenticate(ctx, email, password)
	if err != nil {
		return Grant{}, err
	}

	token := domain.NewRefreshToken(uuid.NewV7())
	session, evicted, err := s.repo.Create(ctx, NewSession{
		ID:         token.SessionID,
		UserID:     u.ID,
		TokenHash:  token.Hash(),
		Client:     cleanClient(c),
		IdleTTL:    s.cfg.IdleTTL,
		MaxAge:     s.cfg.MaxAge,
		MaxPerUser: s.cfg.MaxPerUser,
	})
	if errors.Is(err, domain.ErrNotFound) {
		return Grant{}, errInvalidCredentials // the account was deleted since the password check
	}
	if err != nil {
		return Grant{}, fmt.Errorf("start a session for user %s: %w", u.ID, err)
	}
	if len(evicted) > 0 {
		s.logger.InfoContext(ctx, "least recently used sessions ended over the per-user limit",
			slog.String("user_id", u.ID.String()), slog.Int("sessions", len(evicted)))
	}
	return s.grant(u, session, token)
}

// Refresh trades a refresh token for an access token and the session's current refresh token. With the current
// token it rotates the refresh token, unless the last rotation is less than the grace window ago: then it hands out
// the current token again. With the previous token it hands out the current one within the grace window, so that
// concurrent refreshes from several tabs and a client whose previous answer got lost keep their session; after the
// window the session is revoked, because only a copy of a token that was already used can be presented then.
//
// Every failure the client can cause is REFRESH_INVALID; the result says which one it was.
func (s *Sessions) Refresh(ctx context.Context, raw string, c Client) (Grant, RefreshResult, error) {
	token, ok := domain.ParseRefreshToken(raw)
	if !ok {
		return Grant{}, RefreshInvalid, errRefreshInvalid
	}
	presented := token.Hash()

	// The second pass runs only when a concurrent request rotated the session between the read and the
	// compare-and-set. The presented token is then the previous one, and the grace rule answers it.
	for range 2 {
		stored, err := s.repo.Get(ctx, token.SessionID, s.cfg.Grace)
		if errors.Is(err, domain.ErrNotFound) {
			return Grant{}, RefreshInvalid, errRefreshInvalid
		}
		if err != nil {
			return Grant{}, "", fmt.Errorf("read session %s: %w", token.SessionID, err)
		}
		log := s.logger.With(slog.String("session_id", stored.ID.String()), slog.String("user_id", stored.UserID.String()))

		current := subtle.ConstantTimeCompare(presented, stored.TokenHash) == 1
		previous := stored.PrevTokenHash != nil && subtle.ConstantTimeCompare(presented, stored.PrevTokenHash) == 1
		switch {
		case stored.Expired:
			// The session cannot refresh any more either way, so a failed delete is left to the sweeper.
			if err := s.repo.Delete(ctx, stored.ID); err != nil {
				log.WarnContext(ctx, "deleting an expired session failed", slog.Any("error", err))
			}
			return Grant{}, RefreshExpired, errRefreshInvalid

		case current && stored.InGrace:
			g, err := s.grant(stored.User, stored.Session, token)
			if err != nil {
				return Grant{}, "", err
			}
			return g, RefreshReissued, nil

		case current:
			g, rotated, err := s.rotate(ctx, stored, presented, c)
			if err != nil {
				return Grant{}, "", err
			}
			if rotated {
				return g, RefreshRotated, nil
			}
			continue // another request rotated first

		case previous && stored.InGrace:
			g, err := s.graceGrant(stored)
			if err != nil {
				// Only a changed signing key or a damaged row gets here. A 500 would make the client retry with
				// the same token until the grace window is over, and then revoke the session.
				log.ErrorContext(ctx, "cannot open the current refresh token of a session", slog.Any("error", err))
				return Grant{}, RefreshInvalid, errRefreshInvalid
			}
			return g, RefreshGrace, nil

		case previous:
			// Whoever holds the current token must not keep the session, so a failed delete fails the request:
			// the client's retry presents the same token and tries again.
			if err := s.repo.Delete(ctx, stored.ID); err != nil {
				return Grant{}, "", fmt.Errorf("revoke session %s after a reused refresh token: %w", stored.ID, err)
			}
			log.WarnContext(ctx, "refresh token reuse detected; session revoked", slog.Int("generation", stored.Generation))
			return Grant{}, RefreshReuseDetected, errRefreshInvalid
		}
		// An older generation. Only one previous token is remembered, so this one proves nothing about the
		// session, and revoking the session for it would let a stale token log its owner out.
		return Grant{}, RefreshInvalid, errRefreshInvalid
	}
	return Grant{}, RefreshInvalid, errRefreshInvalid
}

// rotate gives the session a new refresh token if presented is still its current one. rotated is false when
// another request rotated it first or the session ended meanwhile.
func (s *Sessions) rotate(ctx context.Context, stored StoredSession, presented []byte, c Client) (g Grant, rotated bool, err error) {
	next := domain.NewRefreshToken(stored.ID)
	sealed, err := s.sealer.seal(stored.ID, next.Secret[:])
	if err != nil {
		return Grant{}, false, err
	}
	session, ok, err := s.repo.Rotate(ctx, Rotation{
		ID:         stored.ID,
		Presented:  presented,
		TokenHash:  next.Hash(),
		GraceToken: sealed,
		Client:     cleanClient(c),
		IdleTTL:    s.cfg.IdleTTL,
		MaxAge:     s.cfg.MaxAge,
	})
	if err != nil {
		return Grant{}, false, fmt.Errorf("rotate session %s: %w", stored.ID, err)
	}
	if !ok {
		return Grant{}, false, nil
	}
	g, err = s.grant(stored.User, session, next)
	return g, true, err
}

// graceGrant answers with the session's current refresh token, recovered from its sealed copy.
func (s *Sessions) graceGrant(stored StoredSession) (Grant, error) {
	secret, err := s.sealer.open(stored.ID, stored.GraceToken)
	if err != nil {
		return Grant{}, err
	}
	if len(secret) != domain.RefreshSecretBytes {
		return Grant{}, fmt.Errorf("the sealed secret has %d bytes, want %d", len(secret), domain.RefreshSecretBytes)
	}
	current := domain.RefreshToken{SessionID: stored.ID}
	copy(current.Secret[:], secret)
	if subtle.ConstantTimeCompare(current.Hash(), stored.TokenHash) != 1 {
		return Grant{}, errors.New("the sealed secret does not match the current token hash")
	}
	return s.grant(stored.User, stored.Session, current)
}

// Logout ends the session of a refresh token, whether it is the current or the previous one. Anything else,
// including a malformed or unknown token, changes nothing and is no error, so that logging out can always be
// repeated.
func (s *Sessions) Logout(ctx context.Context, raw string) error {
	token, ok := domain.ParseRefreshToken(raw)
	if !ok {
		return nil
	}
	deleted, err := s.repo.DeleteByToken(ctx, token.SessionID, token.Hash())
	if err != nil {
		return fmt.Errorf("end session %s: %w", token.SessionID, err)
	}
	if deleted {
		s.logger.InfoContext(ctx, "session ended by logout", slog.String("session_id", token.SessionID.String()))
	}
	return nil
}

// LogoutAll ends every session of the user.
func (s *Sessions) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	ended, err := s.repo.DeleteAllOfUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("end the sessions of user %s: %w", userID, err)
	}
	s.logger.InfoContext(ctx, "all sessions of the user ended", slog.Int("sessions", len(ended)))
	return nil
}

// grant issues an access token for the session and bundles it with the refresh token.
func (s *Sessions) grant(u domain.User, session domain.Session, refresh domain.RefreshToken) (Grant, error) {
	access, err := s.tokens.Issue(domain.Principal{UserID: u.ID, Role: u.Role, SessionID: session.ID})
	if err != nil {
		return Grant{}, err
	}
	return Grant{User: u, Session: session, Access: access, Refresh: refresh}, nil
}

func cleanClient(c Client) Client {
	return Client{UserAgent: domain.CleanUserAgent(c.UserAgent), IP: c.IP.Unmap()}
}
