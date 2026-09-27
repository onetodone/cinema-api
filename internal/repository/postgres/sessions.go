package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/auth"
)

// Sessions stores login sessions. It implements auth.SessionRepository, and worker.ExpiredSessions for the sweeper.
//
// Every expiry decision uses the database clock. Statements that change several sessions of one user (a login
// that evicts sessions over the cap, logout from every session) lock the user's row first, so they take turns
// instead of deadlocking on each other's session rows, and concurrent logins cannot overshoot the cap.
type Sessions struct {
	pool *pgxpool.Pool
}

// NewSessions returns a Sessions repository backed by pool.
func NewSessions(pool *pgxpool.Pool) *Sessions {
	return &Sessions{pool: pool}
}

const sessionColumns = `id, user_id, user_agent, ip, generation, created_at, last_used_at, expires_at`

func scanSession(row pgx.Row) (domain.Session, error) {
	var s domain.Session
	err := row.Scan(&s.ID, &s.UserID, &s.UserAgent, &s.IP, &s.Generation, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt)
	return s, err
}

// lockUser locks the row of a user against concurrent session changes of the same user. FOR NO KEY UPDATE does
// not block the foreign key checks of rows that reference the user, such as new bookings.
func lockUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(domain.CodeUserNotFound, "user %s not found", userID)
	}
	return err
}

// Create stores a new session and deletes the user's least recently used sessions beyond s.MaxPerUser.
func (r *Sessions) Create(ctx context.Context, s auth.NewSession) (domain.Session, []uuid.UUID, error) {
	var (
		created domain.Session
		evicted []uuid.UUID
	)
	err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, s.UserID); err != nil {
			return err
		}

		var err error
		created, err = scanSession(tx.QueryRow(ctx, `
INSERT INTO sessions (id, user_id, token_hash, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + LEAST($6::interval, $7::interval))
RETURNING `+sessionColumns,
			s.ID, s.UserID, s.TokenHash, s.Client.UserAgent, s.Client.IP, s.IdleTTL, s.MaxAge))
		if err != nil {
			return fmt.Errorf("insert session: %w", err)
		}

		// The new session is the most recently used one, so it is never among the evicted.
		rows, err := tx.Query(ctx, `
DELETE FROM sessions
WHERE id IN (
    SELECT id FROM sessions
    WHERE user_id = $1
    ORDER BY last_used_at DESC, id DESC
    OFFSET $2
)
RETURNING id`, s.UserID, s.MaxPerUser)
		if err != nil {
			return fmt.Errorf("evict sessions over the limit: %w", err)
		}
		evicted, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("evict sessions over the limit: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Session{}, nil, err
	}
	return created, evicted, nil
}

// Get returns a session with its user and its token state.
func (r *Sessions) Get(ctx context.Context, id uuid.UUID, grace time.Duration) (auth.StoredSession, error) {
	var s auth.StoredSession
	err := r.pool.QueryRow(ctx, `
SELECT s.id, s.user_id, s.user_agent, s.ip, s.generation, s.created_at, s.last_used_at, s.expires_at,
       s.token_hash, s.prev_token_hash, s.grace_token,
       s.expires_at <= now(),
       COALESCE(s.rotated_at > now() - $2::interval, false),
       u.email, u.role, u.created_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = $1`, id, grace).Scan(
		&s.ID, &s.UserID, &s.UserAgent, &s.IP, &s.Generation, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt,
		&s.TokenHash, &s.PrevTokenHash, &s.GraceToken,
		&s.Expired, &s.InGrace,
		&s.User.Email, &s.User.Role, &s.User.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.StoredSession{}, domain.SessionNotFound(id)
	}
	if err != nil {
		return auth.StoredSession{}, fmt.Errorf("get session %s: %w", id, err)
	}
	s.User.ID = s.UserID
	return s, nil
}

// Rotate replaces the refresh token of a session, if r.Presented is still its current token and it has not
// expired. The WHERE clause is the compare-and-set: of concurrent rotations with the same token, exactly one
// matches, and the others see ok false.
func (r *Sessions) Rotate(ctx context.Context, rot auth.Rotation) (domain.Session, bool, error) {
	s, err := scanSession(r.pool.QueryRow(ctx, `
UPDATE sessions
SET prev_token_hash = token_hash,
    token_hash      = $3,
    grace_token     = $4,
    rotated_at      = now(),
    generation      = generation + 1,
    last_used_at    = now(),
    user_agent      = $5,
    ip              = COALESCE($6, ip),
    expires_at      = LEAST(now() + $7::interval, created_at + $8::interval)
WHERE id = $1 AND token_hash = $2 AND expires_at > now()
RETURNING `+sessionColumns,
		rot.ID, rot.Presented, rot.TokenHash, rot.GraceToken, rot.Client.UserAgent, rot.Client.IP,
		rot.IdleTTL, rot.MaxAge))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, false, nil
	}
	if err != nil {
		return domain.Session{}, false, fmt.Errorf("rotate session %s: %w", rot.ID, err)
	}
	return s, true, nil
}

// Delete deletes a session.
func (r *Sessions) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// DeleteByToken deletes a session if tokenHash is its current or previous token hash.
func (r *Sessions) DeleteByToken(ctx context.Context, id uuid.UUID, tokenHash []byte) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM sessions WHERE id = $1 AND (token_hash = $2 OR prev_token_hash = $2)`, id, tokenHash)
	if err != nil {
		return false, fmt.Errorf("delete session %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteAllOfUser deletes every session of a user and returns their ids.
func (r *Sessions) DeleteAllOfUser(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	var ended []uuid.UUID
	err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, userID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `DELETE FROM sessions WHERE user_id = $1 RETURNING id`, userID)
		if err != nil {
			return err
		}
		ended, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil // a deleted account has no sessions left: the foreign key cascades
	}
	if err != nil {
		return nil, fmt.Errorf("delete the sessions of user %s: %w", userID, err)
	}
	return ended, nil
}

// ListOfUser returns the live sessions of a user, most recently used first. The user's sessions are at most
// AUTH_MAX_SESSIONS_PER_USER, so the list needs no pages.
func (r *Sessions) ListOfUser(ctx context.Context, userID uuid.UUID) ([]domain.Session, error) {
	rows, err := r.pool.Query(ctx, `
SELECT `+sessionColumns+`
FROM sessions
WHERE user_id = $1 AND expires_at > now()
ORDER BY last_used_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list the sessions of user %s: %w", userID, err)
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Session, error) { return scanSession(row) })
	if err != nil {
		return nil, fmt.Errorf("list the sessions of user %s: %w", userID, err)
	}
	return sessions, nil
}

// DeleteOfUser deletes a session if it belongs to the user.
func (r *Sessions) DeleteOfUser(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, fmt.Errorf("delete session %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteExpired deletes up to limit expired sessions, oldest first, and returns how many it deleted. Sessions
// that another sweeper, a refresh, or a logout has locked are skipped, so several sweepers share the work and
// none of them waits.
func (r *Sessions) DeleteExpired(ctx context.Context, limit int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
WITH due AS (
    SELECT id FROM sessions
    WHERE expires_at <= now()
    ORDER BY expires_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
DELETE FROM sessions s USING due WHERE s.id = due.id`, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
