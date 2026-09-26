package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

var (
	idempotencyClaim    = script("idempotency_claim.lua")
	idempotencyComplete = script("idempotency_complete.lua")
	idempotencyRelease  = script("idempotency_release.lua")
)

// Idempotency implements middleware.IdempotencyStore: records that have an owner and expire. The middleware
// decides what a record contains; the store only guarantees that a key is claimed once, and that only its owner
// can complete or release it. Each key is a hash with the fields owner and record, named
// <prefix>:idem:<key>.
type Idempotency struct {
	s *Store
}

// Idempotency returns the idempotency store.
func (s *Store) Idempotency() *Idempotency {
	return &Idempotency{s: s}
}

// Claim stores record for owner under key, which then expires after ttl, unless the key exists. It returns the
// stored record when the key exists; claimed reports whether this call stored its record.
func (i *Idempotency) Claim(ctx context.Context, key, owner string, record []byte, ttl time.Duration) ([]byte, bool, error) {
	existing, err := idempotencyClaim.Run(ctx, i.s.rdb, []string{i.s.key("idem", key)}, owner, record, ttl.Milliseconds()).Text()
	switch {
	case errors.Is(err, goredis.Nil):
		return nil, true, nil
	case err != nil:
		i.s.failed(ctx, metrics.OpIdempotency, err)
		return nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return []byte(existing), false, nil
}

// Complete replaces the record under key, which then expires after ttl, if owner still owns the key. It reports
// whether it did: a key whose claim expired may have been claimed by someone else.
func (i *Idempotency) Complete(ctx context.Context, key, owner string, record []byte, ttl time.Duration) (bool, error) {
	done, err := idempotencyComplete.Run(ctx, i.s.rdb, []string{i.s.key("idem", key)}, owner, record, ttl.Milliseconds()).Bool()
	if err != nil {
		i.s.failed(ctx, metrics.OpIdempotency, err)
		return false, fmt.Errorf("complete idempotency key: %w", err)
	}
	return done, nil
}

// Release deletes the record under key if owner still owns the key, so that the request can be sent again.
func (i *Idempotency) Release(ctx context.Context, key, owner string) error {
	if err := idempotencyRelease.Run(ctx, i.s.rdb, []string{i.s.key("idem", key)}, owner).Err(); err != nil {
		i.s.failed(ctx, metrics.OpIdempotency, err)
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}
