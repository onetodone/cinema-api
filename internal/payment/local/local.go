// Package local is a payment provider for development and tests. It moves no money: the payment token alone
// decides each outcome, so every path of the payment flow can be driven by hand or from tests.
//
// Test tokens:
//
//	tok_success             charged at once
//	tok_slow                charged after a delay (5 s by default), to show a payment that finishes after the
//	                        booking's hold has run out
//	tok_declined            declined: card_declined
//	tok_insufficient_funds  declined: insufficient_funds
//	tok_expired_card        declined: expired_card
//	tok_unavailable         the provider refuses the request; nothing is charged
//	tok_error               the connection breaks; nothing is charged, but the caller cannot know that
//	tok_timeout             no answer until the caller gives up; nothing is charged
//
// Any other token is declined with invalid_token.
//
// The ledger of charges lives in the memory of the process, so each process that builds a provider has its own.
// The tokens that leave a payment's outcome unknown (tok_error, tok_timeout, and tok_slow cut short) never
// charge, so a provider in another process, such as the worker's, rightly answers "not found" for them.
package local

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/payment"
)

// ID is the provider id of the local provider.
const ID = "local"

// DefaultSlowDelay is how long tok_slow takes when Config leaves it unset.
const DefaultSlowDelay = 5 * time.Second

// Test tokens.
const (
	TokenSuccess           = "tok_success"
	TokenSlow              = "tok_slow"
	TokenDeclined          = "tok_declined"
	TokenInsufficientFunds = "tok_insufficient_funds"
	TokenExpiredCard       = "tok_expired_card"
	TokenUnavailable       = "tok_unavailable"
	TokenError             = "tok_error"
	TokenTimeout           = "tok_timeout"
)

// declines maps the tokens that are declined to their decline codes.
var declines = map[string]string{
	TokenDeclined:          payment.DeclineCardDeclined,
	TokenInsufficientFunds: payment.DeclineInsufficientFunds,
	TokenExpiredCard:       payment.DeclineExpiredCard,
}

// Config configures a Provider.
type Config struct {
	SlowDelay time.Duration // how long tok_slow takes; DefaultSlowDelay when zero
}

// Provider is the local payment provider. It implements payment.Provider.
type Provider struct {
	slowDelay time.Duration

	mu      sync.Mutex
	charges map[uuid.UUID]payment.Charge // by payment id, the idempotency key
}

// New returns a local Provider with an empty ledger.
func New(cfg Config) *Provider {
	if cfg.SlowDelay <= 0 {
		cfg.SlowDelay = DefaultSlowDelay
	}
	return &Provider{slowDelay: cfg.SlowDelay, charges: map[uuid.UUID]payment.Charge{}}
}

// ID returns "local".
func (p *Provider) ID() string { return ID }

// Name returns the display name.
func (p *Provider) Name() string { return "Test card" }

// Pay charges according to the test token. A payment id that was charged or declined before gets the same answer
// again, whatever its token.
func (p *Provider) Pay(ctx context.Context, req payment.PayRequest) (payment.Charge, error) {
	if c, ok := p.lookup(req.PaymentID); ok {
		return c, nil
	}

	switch req.Token {
	case TokenSuccess:
		return p.record(req.PaymentID, payment.Charge{Status: payment.ChargeSucceeded}), nil
	case TokenSlow:
		timer := time.NewTimer(p.slowDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return payment.Charge{}, fmt.Errorf("local provider: no answer: %w", ctx.Err())
		case <-timer.C:
			return p.record(req.PaymentID, payment.Charge{Status: payment.ChargeSucceeded}), nil
		}
	case TokenUnavailable:
		return payment.Charge{}, fmt.Errorf("local provider: service unavailable (simulated): %w", payment.ErrUnavailable)
	case TokenError:
		return payment.Charge{}, errors.New("local provider: connection reset (simulated)")
	case TokenTimeout:
		<-ctx.Done()
		return payment.Charge{}, fmt.Errorf("local provider: no answer: %w", ctx.Err())
	}

	code, ok := declines[req.Token]
	if !ok {
		code = payment.DeclineInvalidToken
	}
	return p.record(req.PaymentID, payment.Charge{Status: payment.ChargeDeclined, DeclineCode: code}), nil
}

// Refund marks a succeeded charge as refunded.
func (p *Provider) Refund(_ context.Context, req payment.RefundRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.charges[req.PaymentID]
	switch {
	case !ok:
		return fmt.Errorf("local provider: refund of payment %s: no charge", req.PaymentID)
	case c.Status == payment.ChargeRefunded:
		return nil
	case c.Status != payment.ChargeSucceeded:
		return fmt.Errorf("local provider: refund of payment %s: the charge is %s", req.PaymentID, c.Status)
	}
	c.Status = payment.ChargeRefunded
	p.charges[req.PaymentID] = c
	return nil
}

// Status reports the ledger entry of a payment.
func (p *Provider) Status(_ context.Context, req payment.StatusRequest) (payment.Charge, error) {
	if c, ok := p.lookup(req.PaymentID); ok {
		return c, nil
	}
	return payment.Charge{Status: payment.ChargeNotFound}, nil
}

func (p *Provider) lookup(id uuid.UUID) (payment.Charge, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.charges[id]
	return c, ok
}

// record stores the outcome of a new charge under a fresh reference and returns it. If a concurrent call with the
// same payment id recorded first, its outcome wins and is returned instead.
func (p *Provider) record(id uuid.UUID, c payment.Charge) payment.Charge {
	p.mu.Lock()
	defer p.mu.Unlock()
	if first, ok := p.charges[id]; ok {
		return first
	}
	c.Ref = "local_" + uuid.NewV4().String()
	p.charges[id] = c
	return c
}
