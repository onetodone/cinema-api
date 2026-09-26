package local

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/payment/paymenttest"
)

func TestContract(t *testing.T) {
	t.Parallel()
	paymenttest.Run(t, New(Config{}), paymenttest.Tokens{Success: TokenSuccess, Declined: TokenDeclined})
}

func request(token string) payment.PayRequest {
	return payment.PayRequest{PaymentID: uuid.NewV7(), BookingID: uuid.NewV7(), AmountCents: 1000, Currency: "USD", Token: token}
}

func TestTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		token   string
		status  payment.ChargeStatus
		decline string
	}{
		{token: TokenSuccess, status: payment.ChargeSucceeded},
		{token: TokenDeclined, status: payment.ChargeDeclined, decline: payment.DeclineCardDeclined},
		{token: TokenInsufficientFunds, status: payment.ChargeDeclined, decline: payment.DeclineInsufficientFunds},
		{token: TokenExpiredCard, status: payment.ChargeDeclined, decline: payment.DeclineExpiredCard},
		{token: "tok_nonsense", status: payment.ChargeDeclined, decline: payment.DeclineInvalidToken},
		{token: "", status: payment.ChargeDeclined, decline: payment.DeclineInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			t.Parallel()
			p := New(Config{})
			req := request(tt.token)

			c, err := p.Pay(t.Context(), req)
			if err != nil {
				t.Fatalf("Pay: %v", err)
			}
			if c.Status != tt.status || c.DeclineCode != tt.decline || c.Ref == "" {
				t.Errorf("Pay = %+v, want status %s, decline %q, and a reference", c, tt.status, tt.decline)
			}
			if got, _ := p.Status(t.Context(), payment.StatusRequest{PaymentID: req.PaymentID}); got != c {
				t.Errorf("Status = %+v, want %+v", got, c)
			}
		})
	}
}

// TestTokensWithoutCharge covers the tokens that end in an error. None of them charges, so Status finds nothing,
// and a later Pay with the same payment id and a good token goes through.
func TestTokensWithoutCharge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		token       string
		unavailable bool // the error guarantees that nothing was charged
		deadline    bool // the call ran until its context ended
	}{
		{token: TokenUnavailable, unavailable: true},
		{token: TokenError},
		{token: TokenTimeout, deadline: true},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				p := New(Config{})
				req := request(tt.token)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()

				start := time.Now()
				_, err := p.Pay(ctx, req)
				if err == nil {
					t.Fatal("Pay succeeded")
				}
				if errors.Is(err, payment.ErrUnavailable) != tt.unavailable {
					t.Errorf("errors.Is(%v, ErrUnavailable) = %t, want %t", err, !tt.unavailable, tt.unavailable)
				}
				if waited := time.Since(start); errors.Is(err, context.DeadlineExceeded) != tt.deadline ||
					(waited == 10*time.Second) != tt.deadline {
					t.Errorf("err = %v after %s; want a deadline error after 10s: %t", err, waited, tt.deadline)
				}

				if c, _ := p.Status(t.Context(), payment.StatusRequest{PaymentID: req.PaymentID}); c.Status != payment.ChargeNotFound {
					t.Errorf("Status = %+v, want not found", c)
				}
				req.Token = TokenSuccess
				if c, err := p.Pay(t.Context(), req); err != nil || c.Status != payment.ChargeSucceeded {
					t.Errorf("Pay again with a good token = %+v, %v", c, err)
				}
			})
		})
	}
}

func TestSlowToken(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := New(Config{})
		start := time.Now()
		c, err := p.Pay(t.Context(), request(TokenSlow))
		if err != nil || c.Status != payment.ChargeSucceeded {
			t.Fatalf("Pay = %+v, %v", c, err)
		}
		if took := time.Since(start); took != DefaultSlowDelay {
			t.Errorf("tok_slow took %s, want %s", took, DefaultSlowDelay)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		p := New(Config{SlowDelay: time.Minute})
		start := time.Now()
		if _, err := p.Pay(t.Context(), request(TokenSlow)); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took != time.Minute {
			t.Errorf("tok_slow took %s, want the configured minute", took)
		}
	})

	// Cut short by the caller: no answer, and nothing charged.
	synctest.Test(t, func(t *testing.T) {
		p := New(Config{})
		req := request(TokenSlow)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := p.Pay(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Pay = %v, want a deadline error", err)
		}
		if c, _ := p.Status(t.Context(), payment.StatusRequest{PaymentID: req.PaymentID}); c.Status != payment.ChargeNotFound {
			t.Errorf("Status = %+v, want not found", c)
		}
	})
}

// TestConcurrentPayChargesOnce sends the same payment id many times at once. Every call gets the same charge.
func TestConcurrentPayChargesOnce(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := New(Config{SlowDelay: time.Second})
		req := request(TokenSlow)
		charges := make([]payment.Charge, 50)
		var wg sync.WaitGroup
		for i := range charges {
			wg.Go(func() {
				c, err := p.Pay(t.Context(), req)
				if err != nil {
					t.Error(err)
				}
				charges[i] = c
			})
		}
		wg.Wait()
		for _, c := range charges {
			if c != charges[0] || c.Status != payment.ChargeSucceeded {
				t.Fatalf("charges differ: %+v and %+v", c, charges[0])
			}
		}
	})
}

func TestRefundErrors(t *testing.T) {
	t.Parallel()
	p := New(Config{})

	if err := p.Refund(t.Context(), payment.RefundRequest{PaymentID: uuid.NewV7()}); err == nil {
		t.Error("refund of an unknown payment succeeded")
	}

	declined := request(TokenDeclined)
	if _, err := p.Pay(t.Context(), declined); err != nil {
		t.Fatal(err)
	}
	if err := p.Refund(t.Context(), payment.RefundRequest{PaymentID: declined.PaymentID}); err == nil {
		t.Error("refund of a declined charge succeeded")
	}
}
