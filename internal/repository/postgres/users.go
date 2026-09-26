package postgres

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/domain"
)

// emailUniqueIndex enforces one account per address, compared case-insensitively.
const emailUniqueIndex = "users_email_uq"

// Users stores accounts. Email lookups are case-insensitive and use the unique index on lower(email).
type Users struct {
	pool *pgxpool.Pool
}

// NewUsers returns a Users repository backed by pool.
func NewUsers(pool *pgxpool.Pool) *Users {
	return &Users{pool: pool}
}

const userColumns = `id, email, password_hash, role, created_at`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt)
	return u, err
}

// CreateUser inserts u. An address that differs from an existing one only in letter case fails with
// EMAIL_TAKEN.
func (r *Users) CreateUser(ctx context.Context, u domain.User) (domain.User, error) {
	created, err := scanUser(r.pool.QueryRow(ctx, `
INSERT INTO users (id, email, password_hash, role)
VALUES ($1, $2, $3, $4::user_role)
RETURNING `+userColumns,
		u.ID, u.Email, u.PasswordHash, string(u.Role)))
	if isUniqueViolation(err, emailUniqueIndex) {
		return domain.User{}, domain.Conflict(domain.CodeEmailTaken, "an account with this email already exists")
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	return created, nil
}

// GetUserByEmail returns the account with this address in any letter case, or a USER_NOT_FOUND error.
func (r *Users) GetUserByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1)`, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.NotFound(domain.CodeUserNotFound, "user not found")
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

// GetUserByID returns one account or a USER_NOT_FOUND error.
func (r *Users) GetUserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.NotFound(domain.CodeUserNotFound, "user %s not found", id)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user %s: %w", id, err)
	}
	return u, nil
}

// UpsertAdmin makes sure an admin account with u's address exists and has u's password hash. An existing
// account with that address, in any letter case, is promoted to admin and gets the new hash; its id and
// stored address stay. created reports whether a new row was inserted.
//
// Replacing the hash matters: if somebody registered the admin address first, promoting the account without
// resetting its password would hand them admin rights.
func (r *Users) UpsertAdmin(ctx context.Context, u domain.User) (user domain.User, created bool, err error) {
	user, err = scanUser(r.pool.QueryRow(ctx, `
INSERT INTO users (id, email, password_hash, role)
VALUES ($1, $2, $3, 'admin')
ON CONFLICT ((lower(email))) DO UPDATE
SET password_hash = EXCLUDED.password_hash, role = 'admin'
RETURNING `+userColumns,
		u.ID, u.Email, u.PasswordHash))
	if err != nil {
		return domain.User{}, false, fmt.Errorf("upsert admin: %w", err)
	}
	return user, user.ID == u.ID, nil
}

// isUniqueViolation reports whether err is a unique violation of the named constraint or index.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation && pgErr.ConstraintName == constraint
}
