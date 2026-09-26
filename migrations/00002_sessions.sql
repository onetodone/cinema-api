-- +goose Up

-- One row per login. The id is stable for the session's life: it is the sid claim of every access token the
-- session issues, and the first part of its refresh token "<id>.<secret>". Only hashes of refresh secrets are
-- stored, so a copy of this table cannot be used to refresh.
CREATE TABLE sessions (
    id              uuid PRIMARY KEY,                  -- UUIDv7 generated in Go
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      bytea NOT NULL,                    -- SHA-256 of the current secret
    prev_token_hash bytea,                             -- SHA-256 of the previous secret, for reuse detection
    -- The current secret, sealed with AES-256-GCM under a key derived from JWT_SECRET. It is read only within the
    -- grace window after a rotation, to hand the current token to a request that presented the previous one.
    grace_token     bytea,
    rotated_at      timestamptz,
    generation      int  NOT NULL DEFAULT 0 CHECK (generation >= 0),
    user_agent      text NOT NULL DEFAULT '',          -- at most 512 bytes
    ip              inet,                              -- the client address, as the rate limiter resolves it
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,              -- LEAST(last rotation + idle TTL, created_at + max age)
    CONSTRAINT sessions_token_hash_uq UNIQUE (token_hash),
    -- A rotation sets all three at once; a session that was never rotated has none of them.
    CHECK ((rotated_at IS NULL) = (prev_token_hash IS NULL) AND (rotated_at IS NULL) = (grace_token IS NULL))
);
-- a user's sessions, most recently used first (eviction over the cap, the sessions list)
CREATE INDEX sessions_user_idx ON sessions (user_id, last_used_at DESC);
-- the worker's sweep of expired sessions
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- +goose Down

DROP TABLE sessions;
