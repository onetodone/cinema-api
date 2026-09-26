// Package postgres implements the service repositories on PostgreSQL with pgx and hand-written SQL.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE codes this package maps to domain errors or retries.
const (
	sqlstateForeignKeyViolation  = "23503"
	sqlstateUniqueViolation      = "23505"
	sqlstateExclusionViolation   = "23P01"
	sqlstateSerializationFailure = "40001"
	sqlstateDeadlockDetected     = "40P01"
	sqlstateLockNotAvailable     = "55P03" // lock_timeout expired
)

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so queries can run inside or outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pgErrorCode returns the SQLSTATE of err, or "" if err does not come from PostgreSQL.
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// isViolation reports whether err is a PostgreSQL error with the given SQLSTATE on the named constraint or index.
func isViolation(err error, sqlstate, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstate && pgErr.ConstraintName == constraint
}

// deref returns the string behind a nullable column, or "" for NULL.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nullable maps "" to NULL for optional text columns.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
