package redis

import (
	"context"
	"fmt"
	"time"
	"uuid"

	goredis "github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

// Revocations is the list of sessions that ended before they expired. Access tokens are stateless, so without it
// the tokens of an ended session would work until they expire. The session use cases put every session they end
// on the list (auth.RevocationList), and the auth middleware looks up the session of every access token there
// (middleware.RevocationList), on every API replica.
//
// Keys are named <prefix>:revoked-sid:<session id> and hold "1". Each expires once the last access token of its
// session has, so the list holds only the sessions that ended within the last access-token lifetime.
type Revocations struct {
	s *Store
}

// Revocations returns the revocation list.
func (s *Store) Revocations() *Revocations {
	return &Revocations{s: s}
}

func (r *Revocations) key(sessionID uuid.UUID) string {
	return r.s.key("revoked-sid", sessionID.String())
}

// Revoke puts the sessions on the list for ttl, in one round trip.
func (r *Revocations) Revoke(ctx context.Context, ttl time.Duration, sessionIDs ...uuid.UUID) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	if ttl < time.Millisecond {
		return fmt.Errorf("revoke sessions: ttl must be at least 1ms, got %s", ttl)
	}
	_, err := r.s.rdb.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for _, id := range sessionIDs {
			p.Set(ctx, r.key(id), "1", ttl)
		}
		return nil
	})
	if err != nil {
		r.s.failed(ctx, metrics.OpRevocationWrite, err)
		return fmt.Errorf("revoke %d sessions: %w", len(sessionIDs), err)
	}
	return nil
}

// Revoked reports whether the session is on the list.
func (r *Revocations) Revoked(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	n, err := r.s.rdb.Exists(ctx, r.key(sessionID)).Result()
	if err != nil {
		r.s.failed(ctx, metrics.OpRevocationCheck, err)
		return false, fmt.Errorf("look up session %s on the revocation list: %w", sessionID, err)
	}
	return n == 1, nil
}
