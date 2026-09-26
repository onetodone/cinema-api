-- +goose Up

-- Needed for the exclusion constraint that combines "=" on hall_id with "&&" on a time range.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE user_role       AS ENUM ('customer', 'admin');
CREATE TYPE seat_type       AS ENUM ('standard', 'vip', 'accessible');
CREATE TYPE showtime_status AS ENUM ('scheduled', 'canceled');
CREATE TYPE seat_status     AS ENUM ('available', 'held', 'sold');
CREATE TYPE booking_status  AS ENUM ('pending', 'processing', 'paid', 'expired', 'canceled');
CREATE TYPE payment_status  AS ENUM ('pending', 'succeeded', 'failed', 'refunded');

CREATE TABLE users (
    id            uuid PRIMARY KEY,                 -- UUIDv7 generated in Go
    email         text NOT NULL,
    password_hash text NOT NULL,
    role          user_role NOT NULL DEFAULT 'customer',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_uq ON users (lower(email));

CREATE TABLE movies (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title        text NOT NULL CHECK (title <> ''),
    description  text NOT NULL DEFAULT '',
    duration_min int  NOT NULL CHECK (duration_min > 0),
    age_rating   text,
    poster_url   text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE halls (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text NOT NULL UNIQUE CHECK (name <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Physical seat layout of a hall.
CREATE TABLE hall_seats (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    hall_id     bigint NOT NULL REFERENCES halls (id) ON DELETE CASCADE,
    row_label   text   NOT NULL CHECK (row_label <> ''),
    seat_number int    NOT NULL CHECK (seat_number > 0),
    seat_type   seat_type NOT NULL DEFAULT 'standard',
    UNIQUE (hall_id, row_label, seat_number)
);

-- A screening of a movie in a hall (a "session" in business terms).
CREATE TABLE showtimes (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    movie_id         bigint NOT NULL REFERENCES movies (id),
    hall_id          bigint NOT NULL REFERENCES halls (id),
    starts_at        timestamptz NOT NULL,
    ends_at          timestamptz NOT NULL,          -- starts_at + duration + cleaning buffer
    base_price_cents int NOT NULL CHECK (base_price_cents >= 0),
    status           showtime_status NOT NULL DEFAULT 'scheduled',
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at),
    -- A hall cannot run two scheduled showtimes at the same time. Ranges are half-open, so back-to-back is fine.
    CONSTRAINT showtimes_no_hall_overlap
        EXCLUDE USING gist (hall_id WITH =, tstzrange(starts_at, ends_at) WITH &&) WHERE (status = 'scheduled')
);
CREATE INDEX showtimes_starts_at_idx   ON showtimes (starts_at) WHERE status = 'scheduled';
CREATE INDEX showtimes_movie_start_idx ON showtimes (movie_id, starts_at);

CREATE TABLE bookings (
    id          uuid PRIMARY KEY,                   -- UUIDv7 from Go; doubles as the Redis hold token
    user_id     uuid   NOT NULL REFERENCES users (id),
    showtime_id bigint NOT NULL REFERENCES showtimes (id),
    status      booking_status NOT NULL DEFAULT 'pending',
    total_cents int    NOT NULL CHECK (total_cents >= 0),
    expires_at  timestamptz NOT NULL,               -- hold deadline, computed with the DB clock
    paid_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
-- Anti-hoarding: at most one active booking per user per showtime.
CREATE UNIQUE INDEX bookings_one_active_uq ON bookings (user_id, showtime_id)
    WHERE status IN ('pending', 'processing');
-- The expiry worker scans only live bookings, oldest deadline first.
CREATE INDEX bookings_expiry_idx ON bookings (expires_at) WHERE status IN ('pending', 'processing');
-- "My bookings": UUIDv7 ids are time-ordered, so keyset pagination on id works.
CREATE INDEX bookings_user_idx ON bookings (user_id, id DESC);

-- Per-showtime seat inventory: one row per (showtime, seat). This row is what booking transactions lock.
CREATE TABLE showtime_seats (
    showtime_id bigint NOT NULL REFERENCES showtimes (id) ON DELETE CASCADE,
    seat_id     bigint NOT NULL REFERENCES hall_seats (id),
    price_cents int    NOT NULL CHECK (price_cents >= 0),
    status      seat_status NOT NULL DEFAULT 'available',
    booking_id  uuid REFERENCES bookings (id),      -- current holder or buyer
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (showtime_id, seat_id),
    -- A seat has a booking exactly when it is not available.
    CONSTRAINT showtime_seats_booking_matches_status CHECK ((status = 'available') = (booking_id IS NULL))
);
CREATE INDEX showtime_seats_booking_idx ON showtime_seats (booking_id) WHERE booking_id IS NOT NULL;

-- What a booking contained. Survives expiry, when showtime_seats.booking_id is cleared.
CREATE TABLE booking_seats (
    booking_id  uuid   NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    showtime_id bigint NOT NULL,
    seat_id     bigint NOT NULL,
    price_cents int    NOT NULL CHECK (price_cents >= 0),
    PRIMARY KEY (booking_id, seat_id),
    FOREIGN KEY (showtime_id, seat_id) REFERENCES showtime_seats (showtime_id, seat_id)
);

CREATE TABLE payments (
    id             uuid PRIMARY KEY,                -- UUIDv7; also the payment gateway idempotency key
    booking_id     uuid NOT NULL REFERENCES bookings (id),
    amount_cents   int  NOT NULL CHECK (amount_cents >= 0),
    status         payment_status NOT NULL DEFAULT 'pending',
    provider       text NOT NULL,
    provider_ref   text,
    failure_reason text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_booking_idx ON payments (booking_id);
CREATE UNIQUE INDEX payments_one_inflight_uq ON payments (booking_id) WHERE status = 'pending';
CREATE UNIQUE INDEX payments_one_success_uq  ON payments (booking_id) WHERE status = 'succeeded';

-- +goose Down

DROP TABLE payments;
DROP TABLE booking_seats;
DROP TABLE showtime_seats;
DROP TABLE bookings;
DROP TABLE showtimes;
DROP TABLE hall_seats;
DROP TABLE halls;
DROP TABLE movies;
DROP TABLE users;

DROP TYPE payment_status;
DROP TYPE booking_status;
DROP TYPE seat_status;
DROP TYPE showtime_status;
DROP TYPE seat_type;
DROP TYPE user_role;

DROP EXTENSION IF EXISTS btree_gist;
