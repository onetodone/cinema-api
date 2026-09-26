package app

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/payment/local"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// newPaymentProviders registers every payment provider this build knows, each enabled or not by its own
// configuration block. The API offers the enabled ones to clients; the worker needs all of them, to settle
// payments that started before a provider was switched off.
//
// Adding a provider, such as Stripe, takes an adapter in internal/payment/<name>, a configuration block in
// config.PaymentConfig, and one Register call here.
func newPaymentProviders(cfg config.PaymentConfig) (*payment.Registry, error) {
	providers := payment.NewRegistry()
	if err := providers.Register(local.New(local.Config{}), cfg.Local.Enabled); err != nil {
		return nil, err
	}
	return providers, nil
}

// logPaymentMethods reports at startup which payment methods clients can use, and warns about setups that are
// fine for development only.
func logPaymentMethods(logger *slog.Logger, providers *payment.Registry) {
	methods := providers.Methods()
	ids := make([]string, len(methods))
	for i, m := range methods {
		ids[i] = m.ID
	}
	if len(ids) == 0 {
		logger.Warn("no payment provider is enabled; bookings cannot be paid")
		return
	}
	logger.Info("payment methods enabled", slog.Any("providers", ids))
	if _, ok := providers.Enabled(local.ID); ok {
		logger.Warn("the local test payment provider is enabled: test tokens pay for bookings without moving " +
			"money; never enable it in production")
	}
}

// newBookingService builds the booking use cases on db, the same way for the API and the worker.
func newBookingService(db *pgxpool.Pool, cfg config.Config, providers *payment.Registry, m *metrics.Metrics,
	logger *slog.Logger, opts ...booking.Option,
) *booking.Service {
	return booking.New(
		postgres.NewUnitOfWork(db, cfg.DB.LockTimeout, m, logger),
		postgres.NewBookings(db),
		providers,
		booking.Config{
			Location:       cfg.Cinema.Location,
			Currency:       cfg.Cinema.Currency,
			HoldTTL:        cfg.Booking.HoldTTL,
			MaxSeats:       cfg.Booking.MaxSeats,
			HoldClaimTTL:   cfg.Booking.HoldClaimTTL,
			PaymentTimeout: cfg.Payment.Timeout,
			PaymentGrace:   cfg.Payment.Grace,
		},
		logger,
		opts...,
	)
}
