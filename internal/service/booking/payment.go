package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
)

// settleTimeout bounds the transaction that records how a payment ended. It runs after the provider has
// answered, on a context that the client cannot cancel.
const settleTimeout = 10 * time.Second

// Failure reasons of payments that the provider did not decline. Declined payments record the provider's
// decline code instead.
const (
	ReasonProviderUnavailable = "provider_unavailable"      // the provider refused the request; nothing was charged
	ReasonNotFoundAtProvider  = "not_found_at_provider"     // the provider never received the charge
	ReasonRefundedAtProvider  = "refunded_at_provider"      // the provider reports the charge as refunded
	ReasonBookingNotPayable   = "booking_no_longer_payable" // charged after the payment was given up, and refunded
)

// PayResult is a payment and its booking as Pay leaves them. A pending payment has an outcome that is not known
// yet: its booking stays processing until ReconcileBatch learns the outcome from the provider.
type PayResult struct {
	Payment domain.Payment
	Booking domain.Booking
}

// Pay charges a booking of userID through the payment provider np.Method names and, if the charge succeeds,
// sells the booking's seats. It runs in three steps, so that no row lock is held while the provider works:
//
//  1. A transaction locks the booking, checks that it is pending and that its hold has not run out by the
//     database clock, moves it to processing, and records a pending payment. From then on the expiry worker
//     leaves the booking and its seats alone, so a charge that completes after the deadline still finds them.
//  2. The provider charges the payment, with the payment id as idempotency key, within PaymentTimeout.
//  3. A second transaction records the outcome (see settle).
//
// Steps 2 and 3 ignore the cancellation of ctx: once money may move, the outcome must be recorded.
//
// When the provider does not answer, or the outcome cannot be recorded, Pay returns the pending payment and the
// processing booking without an error, and ReconcileBatch settles the payment later.
//
// Errors: a *domain.ValidationError; PAYMENT_METHOD_UNAVAILABLE for a provider that is unknown or disabled;
// BOOKING_NOT_FOUND; BOOKING_EXPIRED; PAYMENT_IN_PROGRESS; BOOKING_ALREADY_PAID; BOOKING_CANCELED;
// BOOKING_BUSY; a *domain.PaymentDeclinedError (PAYMENT_DECLINED) when the provider declined the charge;
// PAYMENT_PROVIDER_UNAVAILABLE when the provider refused the request without charging; PAYMENT_REFUNDED when the
// charge went through only after the payment had been given up, and was refunded. After a decline or an
// unavailable provider the booking is pending again, or expired if its hold has run out meanwhile.
func (s *Service) Pay(ctx context.Context, userID, bookingID uuid.UUID, np domain.NewPayment) (PayResult, error) {
	if err := np.Validate(); err != nil {
		return PayResult{}, err
	}
	provider, ok := s.providers.Enabled(np.Method)
	if !ok {
		return PayResult{}, domain.Invalid(domain.CodePaymentMethodUnavailable,
			"payment method %q is not available; GET /v1/payment-methods lists the available ones", np.Method)
	}

	p, err := s.startPayment(ctx, userID, bookingID, provider.ID())
	if err != nil {
		return PayResult{}, err
	}

	ctx = context.WithoutCancel(ctx)
	charge, err := s.charge(ctx, provider, p, np.Token)
	switch {
	case errors.Is(err, payment.ErrUnavailable):
		out := domain.PaymentOutcome{Status: domain.PaymentFailed, FailureReason: ReasonProviderUnavailable}
		if _, _, err := s.settle(ctx, p, out); err != nil {
			return s.inFlight(ctx, userID, p, slog.LevelError, "recording a payment failed; the worker will settle it", err)
		}
		return PayResult{}, domain.Unavailable(domain.CodePaymentProviderUnavailable,
			"payment method %q cannot take payments right now; nothing was charged; try again later", np.Method)
	case err != nil:
		return s.inFlight(ctx, userID, p, slog.LevelWarn, "payment outcome unknown; the worker will settle it", err)
	}

	settled, _, err := s.settle(ctx, p, outcomeOf(charge))
	if err != nil {
		return s.inFlight(ctx, userID, p, slog.LevelError, "recording a payment failed; the worker will settle it", err)
	}
	switch {
	case settled.Status == domain.PaymentSucceeded:
		b, err := s.reader.GetBooking(ctx, bookingID, userID)
		if err != nil {
			return PayResult{}, err
		}
		s.keepSoldSeatsClaimed(ctx, b)
		return PayResult{Payment: settled, Booking: s.localize(b)}, nil
	case charge.Status == payment.ChargeSucceeded:
		// Charged, but the worker had settled the payment as not charged before this call could record the
		// charge. The booking no longer waits for this payment, so the money goes back.
		return PayResult{}, s.compensate(ctx, provider, settled, charge)
	case charge.Status == payment.ChargeDeclined:
		return PayResult{}, domain.PaymentDeclined(charge.DeclineCode)
	default:
		return PayResult{}, fmt.Errorf("pay %s: provider %s answered the charge with status %q",
			p.ID, p.Provider, charge.Status)
	}
}

// keepSoldSeatsClaimed extends the hold gate's claim on the seats of a paid booking until the showtime starts, so
// that the gate keeps turning requests for them away. Booking a started showtime fails before any seat is locked,
// so the claim is not needed after that. A booking that was paid only after its deadline has no claim left, and
// the database answers for its seats.
func (s *Service) keepSoldSeatsClaimed(ctx context.Context, b domain.Booking) {
	if s.gate == nil {
		return
	}
	seatIDs := make([]int64, len(b.Seats))
	for i, seat := range b.Seats {
		seatIDs[i] = seat.SeatID
	}
	_ = s.gate.ExtendUntil(ctx, b.Showtime.ID, seatIDs, b.ID, b.Showtime.StartsAt) // on failure, the claim expires at the deadline
}

// startPayment is step 1 of Pay: it moves a payable booking to processing and records a pending payment.
func (s *Service) startPayment(ctx context.Context, userID, bookingID uuid.UUID, providerID string) (domain.Payment, error) {
	// The id exists before the transaction does, so a retried transaction records the same idempotency key.
	id := uuid.NewV7()
	var started domain.Payment
	err := s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		b, holdOver, err := r.Bookings().LockForUser(ctx, bookingID, userID)
		if err != nil {
			return err
		}
		if err := domain.CheckPayable(b, holdOver); err != nil {
			return err
		}
		if err := r.Bookings().SetStatus(ctx, domain.BookingPending, domain.BookingProcessing, b.ID); err != nil {
			return err
		}
		started, err = r.Payments().Create(ctx, domain.Payment{
			ID: id, BookingID: b.ID, Provider: providerID, AmountCents: b.TotalCents, Status: domain.PaymentPending,
		})
		return err
	})
	return started, err
}

// charge is step 2 of Pay.
func (s *Service) charge(ctx context.Context, provider payment.Provider, p domain.Payment, token string) (payment.Charge, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.PaymentTimeout)
	defer cancel()
	return provider.Pay(ctx, payment.PayRequest{
		PaymentID: p.ID, BookingID: p.BookingID, AmountCents: p.AmountCents, Currency: s.cfg.Currency, Token: token,
	})
}

// inFlight logs why a payment's outcome is not recorded yet and returns the pending payment with its
// processing booking.
func (s *Service) inFlight(ctx context.Context, userID uuid.UUID, p domain.Payment, level slog.Level, msg string, cause error) (PayResult, error) {
	s.logger.Log(ctx, level, msg,
		slog.String("payment_id", p.ID.String()), slog.String("booking_id", p.BookingID.String()),
		slog.String("provider", p.Provider), slog.Any("error", cause))
	b, err := s.reader.GetBooking(ctx, p.BookingID, userID)
	if err != nil {
		return PayResult{}, err
	}
	return PayResult{Payment: p, Booking: s.localize(b)}, nil
}

// compensate refunds a charge that went through after its payment had been settled as not charged, and records
// the refund.
func (s *Service) compensate(ctx context.Context, provider payment.Provider, p domain.Payment, charge payment.Charge) error {
	refundCtx, cancel := context.WithTimeout(ctx, s.cfg.PaymentTimeout)
	err := provider.Refund(refundCtx, payment.RefundRequest{
		PaymentID: p.ID, ProviderRef: charge.Ref, AmountCents: p.AmountCents, Currency: s.cfg.Currency,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("refund charge %s of payment %s at %s, which landed after the payment was settled as %s; "+
			"it must be refunded by hand: %w", charge.Ref, p.ID, p.Provider, p.Status, err)
	}

	ctx, cancel = context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	err = s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		// Payments change only under the lock of their booking.
		if _, _, err := r.Bookings().Lock(ctx, p.BookingID); err != nil {
			return err
		}
		_, err := r.Payments().Finish(ctx, p.ID, p.Status, domain.PaymentOutcome{
			Status: domain.PaymentRefunded, ProviderRef: charge.Ref, FailureReason: ReasonBookingNotPayable,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("record the refund of charge %s of payment %s: %w", charge.Ref, p.ID, err)
	}
	return domain.Conflict(domain.CodePaymentRefunded,
		"the payment went through only after booking %s had stopped waiting for it; the charge was refunded", p.BookingID)
}

// outcomeOf translates a provider's answer into how the payment ends.
func outcomeOf(c payment.Charge) domain.PaymentOutcome {
	switch c.Status {
	case payment.ChargeSucceeded:
		return domain.PaymentOutcome{Status: domain.PaymentSucceeded, ProviderRef: c.Ref}
	case payment.ChargeRefunded:
		return domain.PaymentOutcome{Status: domain.PaymentRefunded, ProviderRef: c.Ref, FailureReason: ReasonRefundedAtProvider}
	case payment.ChargeDeclined:
		return domain.PaymentOutcome{Status: domain.PaymentFailed, ProviderRef: c.Ref, FailureReason: c.DeclineCode}
	default:
		return domain.PaymentOutcome{Status: domain.PaymentFailed, FailureReason: ReasonNotFoundAtProvider}
	}
}

// settle is step 3 of Pay, and the last step of reconciliation. In one transaction it ends a pending payment as
// out says, and moves its processing booking along:
//   - succeeded: the seats are sold, and the booking is paid;
//   - failed or refunded: the booking is pending again, so its owner can pay another way, or, if its hold has
//     run out by the database clock, expired, and its seats are released.
//
// It returns the payment as stored afterwards, and whether this call settled it. A payment that is already
// settled is returned as it is: the API and the worker may both try to settle the same payment, and the first
// one wins.
func (s *Service) settle(ctx context.Context, p domain.Payment, out domain.PaymentOutcome) (domain.Payment, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	var (
		settled  domain.Payment
		changed  bool
		affected domain.Booking        // the booking, if this call sold or released its seats
		released []domain.ShowtimeSeat // the seats this call released
	)
	err := s.uow.Do(ctx, func(ctx context.Context, r TxRepos) error {
		changed, affected, released = false, domain.Booking{}, nil // a retried attempt starts from scratch
		// Lock order: the booking first, then its seats. The booking lock also guards its payments.
		b, holdOver, err := r.Bookings().Lock(ctx, p.BookingID)
		if err != nil {
			return err
		}
		if settled, err = r.Payments().Get(ctx, p.ID); err != nil || settled.Status != domain.PaymentPending {
			return err
		}
		if b.Status != domain.BookingProcessing {
			return fmt.Errorf("payment %s is pending, but its booking %s is %s", p.ID, b.ID, b.Status)
		}

		next := domain.BookingPending
		switch {
		case out.Status == domain.PaymentSucceeded:
			next = domain.BookingPaid
			err = sellSeats(ctx, r, b.ID)
		case holdOver:
			next = domain.BookingExpired
			released, err = releaseSeats(ctx, r, b.ID)
		}
		if err != nil {
			return err
		}
		if err := r.Bookings().SetStatus(ctx, domain.BookingProcessing, next, b.ID); err != nil {
			return err
		}

		if settled, err = r.Payments().Finish(ctx, p.ID, domain.PaymentPending, out); err != nil {
			return err
		}
		changed = true
		if next != domain.BookingPending {
			affected = b
		}
		return nil
	})
	if err != nil {
		return domain.Payment{}, false, err
	}
	if affected.ID != (uuid.UUID{}) {
		s.releaseClaims(ctx, []domain.Booking{affected}, released)
		s.seatsChanged(ctx, affected.Showtime.ID)
	}
	return settled, changed, nil
}

// sellSeats locks the seats of a booking whose row the caller has locked, and sells them. Every seat of a
// processing booking is held, so the sale must change every seat it locked, and there is at least one.
func sellSeats(ctx context.Context, r TxRepos, bookingID uuid.UUID) error {
	seats, err := r.Seats().LockByBookings(ctx, bookingID)
	if err != nil {
		return err
	}
	sold, err := r.Seats().Sell(ctx, bookingID)
	if err != nil {
		return err
	}
	if len(seats) == 0 || sold != int64(len(seats)) {
		return fmt.Errorf("sell seats of booking %s: %d of %d locked seats changed", bookingID, sold, len(seats))
	}
	return nil
}

// ReconciledBatch reports what one ReconcileBatch call did.
type ReconciledBatch struct {
	Checked   int       // stuck payments looked at
	LastID    uuid.UUID // the last payment looked at; the next batch continues after it
	Paid      int       // payments this call settled as succeeded: their bookings are paid
	Failed    int       // payments this call settled as failed or refunded
	Unsettled int       // payments that could not be settled now; the returned error names each of them
}

// ReconcileBatch settles payments that have been in flight for longer than PaymentGrace. The API lost track of
// them: the provider did not answer in time, the outcome could not be recorded, or the process stopped. For each
// one, ReconcileBatch asks the provider what became of the charge, outside any transaction, and then settles the
// payment and its booking the way Pay would have (see settle).
//
// It looks at up to limit such payments with an id above afterID, in id order. A payment that cannot be settled
// now, because its provider cannot be asked or the database fails, stays pending for a later pass; the returned
// error names every such payment. Once ctx is canceled, no further payment is started, but the one in flight is
// finished.
//
// Several callers may run at once. A payment that another caller, or a late API call, settles first is left as
// it is.
func (s *Service) ReconcileBatch(ctx context.Context, afterID uuid.UUID, limit int) (ReconciledBatch, error) {
	if limit < 1 {
		return ReconciledBatch{}, fmt.Errorf("reconcile payments: limit must be positive, got %d", limit)
	}
	stuck, err := s.reader.ListStuckPayments(ctx, s.cfg.PaymentGrace, afterID, limit)
	if err != nil {
		return ReconciledBatch{}, err
	}

	batch := ReconciledBatch{LastID: afterID}
	var errs []error
	for _, p := range stuck {
		if ctx.Err() != nil {
			break
		}
		batch.Checked++
		batch.LastID = p.ID
		settled, changed, err := s.reconcile(context.WithoutCancel(ctx), p)
		switch {
		case err != nil:
			batch.Unsettled++
			errs = append(errs, fmt.Errorf("payment %s: %w", p.ID, err))
		case !changed:
		case settled.Status == domain.PaymentSucceeded:
			batch.Paid++
		default:
			batch.Failed++
		}
	}
	return batch, errors.Join(errs...)
}

// reconcile asks the provider of a stuck payment about its charge and settles the payment accordingly.
func (s *Service) reconcile(ctx context.Context, p domain.Payment) (domain.Payment, bool, error) {
	provider, ok := s.providers.Provider(p.Provider)
	if !ok {
		return domain.Payment{}, false, fmt.Errorf("payment provider %q is not configured", p.Provider)
	}
	statusCtx, cancel := context.WithTimeout(ctx, s.cfg.PaymentTimeout)
	charge, err := provider.Status(statusCtx, payment.StatusRequest{PaymentID: p.ID})
	cancel()
	if err != nil {
		return domain.Payment{}, false, fmt.Errorf("ask payment provider %s: %w", p.Provider, err)
	}
	return s.settle(ctx, p, outcomeOf(charge))
}
