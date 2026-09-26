package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// DefaultReconcileBatchSize is how many stuck payments one query returns. Each of them costs a call to its
// provider, so a pass works through them in batches.
const DefaultReconcileBatchSize = 100

// PaymentReconciler settles payments that have been in flight for too long. It is implemented by
// booking.Service.
type PaymentReconciler interface {
	ReconcileBatch(ctx context.Context, afterID uuid.UUID, limit int) (booking.ReconciledBatch, error)
}

// ReconcilerConfig configures a Reconciler.
type ReconcilerConfig struct {
	Interval  time.Duration // average pause between two passes
	BatchSize int           // most payments looked at per query
}

// Reconciler settles payments whose outcome the API never recorded: the provider did not answer in time, the
// database failed at the wrong moment, or the API process stopped. Until then such a payment keeps its booking
// in processing, which holds the seats and blocks another payment for the booking. The reconciler asks the
// payment's provider what became of the charge, and settles the payment and its booking: paid, or pending again,
// or expired if the hold has run out.
//
// Any number of reconcilers may run at once. Two of them may ask a provider about the same payment, which is
// harmless, but only the first one settles it.
type Reconciler struct {
	payments PaymentReconciler
	cfg      ReconcilerConfig
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

// NewReconciler returns a Reconciler that counts what it does in m. It panics if the interval or the batch size
// is not positive.
func NewReconciler(payments PaymentReconciler, cfg ReconcilerConfig, m *metrics.Metrics, logger *slog.Logger) *Reconciler {
	if cfg.Interval <= 0 || cfg.BatchSize <= 0 {
		panic(fmt.Sprintf("worker: reconciler interval and batch size must be positive, got %s and %d",
			cfg.Interval, cfg.BatchSize))
	}
	return &Reconciler{payments: payments, cfg: cfg, metrics: m, logger: logger.With(slog.String("job", "reconciler"))}
}

// Run passes over the stuck payments at once and then about every interval until ctx is canceled. A payment
// that cannot be settled is logged and retried by the next pass. Once ctx is canceled, Run lets the payment in
// flight finish, starts no other one, and returns.
func (r *Reconciler) Run(ctx context.Context) {
	every(ctx, r.cfg.Interval, r.pass)
}

// pass works through all stuck payments once, batch by batch. Each batch continues after the last payment of
// the one before, so a payment that cannot be settled now does not hold up the ones behind it.
func (r *Reconciler) pass(ctx context.Context) {
	var after uuid.UUID
	for ctx.Err() == nil {
		start := time.Now()
		batch, err := r.payments.ReconcileBatch(ctx, after, r.cfg.BatchSize)
		r.metrics.PaymentsReconciled.WithLabelValues(metrics.ReconciledPaid).Add(float64(batch.Paid))
		r.metrics.PaymentsReconciled.WithLabelValues(metrics.ReconciledFailed).Add(float64(batch.Failed))
		r.metrics.PaymentsReconciled.WithLabelValues(metrics.ReconciledUnsettled).Add(float64(batch.Unsettled))
		switch {
		case err != nil && batch.Checked == 0:
			if !errors.Is(err, context.Canceled) || ctx.Err() == nil {
				r.logger.ErrorContext(ctx, "listing stuck payments failed; the next pass retries", slog.Any("error", err))
			}
			return
		case err != nil:
			r.logger.WarnContext(ctx, "some stuck payments could not be settled; the next pass retries them",
				slog.Any("error", err))
		}
		if settled := batch.Paid + batch.Failed; settled > 0 {
			r.logger.InfoContext(ctx, "stuck payments settled",
				slog.Int("checked", batch.Checked),
				slog.Int("paid", batch.Paid),
				slog.Int("failed", batch.Failed),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()))
		}
		if batch.Checked < r.cfg.BatchSize {
			return
		}
		after = batch.LastID
	}
}
