package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/onetodone/cinema-api/internal/domain"
)

// inflightPaymentIndex allows one pending payment per booking.
const inflightPaymentIndex = "payments_one_inflight_uq"

const paymentColumns = `id, booking_id, provider, amount_cents, status, provider_ref, failure_reason, created_at, updated_at`

func scanPayment(row pgx.Row) (domain.Payment, error) {
	var (
		p           domain.Payment
		ref, reason *string
	)
	err := row.Scan(&p.ID, &p.BookingID, &p.Provider, &p.AmountCents, &p.Status, &ref, &reason, &p.CreatedAt, &p.UpdatedAt)
	p.ProviderRef, p.FailureReason = deref(ref), deref(reason)
	return p, err
}

// paymentStore changes payments inside a transaction. It implements booking.PaymentRepo.
type paymentStore struct {
	q querier
}

func (s paymentStore) Create(ctx context.Context, p domain.Payment) (domain.Payment, error) {
	created, err := scanPayment(s.q.QueryRow(ctx, `
INSERT INTO payments (id, booking_id, provider, amount_cents, status)
VALUES ($1, $2, $3, $4, 'pending')
RETURNING `+paymentColumns, p.ID, p.BookingID, p.Provider, p.AmountCents))
	switch {
	case isUniqueViolation(err, inflightPaymentIndex):
		// The booking lock and its processing status keep a second payment out already; the index backs them up.
		return domain.Payment{}, domain.Conflict(domain.CodePaymentInProgress,
			"a payment for booking %s is already in progress", p.BookingID)
	case err != nil:
		return domain.Payment{}, fmt.Errorf("insert payment %s: %w", p.ID, err)
	}
	return created, nil
}

func (s paymentStore) Get(ctx context.Context, id uuid.UUID) (domain.Payment, error) {
	p, err := scanPayment(s.q.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE id = $1`, id))
	if err != nil {
		return domain.Payment{}, fmt.Errorf("get payment %s: %w", id, err)
	}
	return p, nil
}

// Finish ends a payment with a compare-and-set on its status, so a payment that another transaction has ended
// already is not overwritten.
func (s paymentStore) Finish(ctx context.Context, id uuid.UUID, from domain.PaymentStatus, out domain.PaymentOutcome) (domain.Payment, error) {
	p, err := scanPayment(s.q.QueryRow(ctx, `
UPDATE payments
SET status = $3::payment_status, provider_ref = $4, failure_reason = $5, updated_at = now()
WHERE id = $1 AND status = $2::payment_status
RETURNING `+paymentColumns,
		id, string(from), string(out.Status), nullable(out.ProviderRef), nullable(out.FailureReason)))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Payment{}, fmt.Errorf("finish payment %s as %s: it is no longer %s", id, out.Status, from)
	case err != nil:
		return domain.Payment{}, fmt.Errorf("finish payment %s as %s: %w", id, out.Status, err)
	}
	return p, nil
}

// ListStuckPayments returns pending payments that started at least age ago. The pending payments are few, the
// ones in flight right now plus the stuck ones, and the partial index payments_one_inflight_uq holds exactly
// them, so the query reads that small index instead of the whole payments table.
func (r *Bookings) ListStuckPayments(ctx context.Context, age time.Duration, afterID uuid.UUID, limit int) ([]domain.Payment, error) {
	rows, err := r.pool.Query(ctx, `
SELECT `+paymentColumns+`
FROM payments
WHERE status = 'pending' AND created_at <= now() - $1::interval AND id > $2
ORDER BY id
LIMIT $3`, age, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list stuck payments: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("list stuck payments: %w", err)
	}
	return list, nil
}
