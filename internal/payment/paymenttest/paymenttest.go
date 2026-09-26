// Package paymenttest checks that a payment.Provider keeps the contract the booking flow relies on. Every
// provider adapter runs Run in its own tests, against the provider's test mode.
package paymenttest

import (
	"testing"
	"uuid"

	"github.com/onetodone/cinema-api/internal/payment"
)

// Tokens are payment tokens that the provider under test answers in a known way.
type Tokens struct {
	Success  string // charged
	Declined string // declined
}

// Run checks the contract of p:
//   - a valid id and a display name;
//   - Pay charges a payment id at most once, and a repeated call answers the first result, whatever its token;
//   - Status reports charged, declined, and refunded payments, and ChargeNotFound for unknown ones;
//   - Refund returns a charge, and refunding it again succeeds.
func Run(t *testing.T, p payment.Provider, tokens Tokens) {
	t.Helper()

	t.Run("identity", func(t *testing.T) {
		if !payment.ValidID(p.ID()) {
			t.Errorf("ID() = %q is not a valid provider id", p.ID())
		}
		if p.Name() == "" {
			t.Error("Name() is empty")
		}
	})

	t.Run("pay is idempotent", func(t *testing.T) {
		req := payRequest(tokens.Success)
		first := pay(t, p, req)
		if first.Status != payment.ChargeSucceeded || first.Ref == "" {
			t.Fatalf("Pay(%q) = %+v, want succeeded with a reference", tokens.Success, first)
		}
		// Same key, different token: the answer is still the first charge.
		req.Token = tokens.Declined
		if again := pay(t, p, req); again != first {
			t.Errorf("repeated Pay = %+v, want the first result %+v", again, first)
		}
		if got := status(t, p, req.PaymentID); got != first {
			t.Errorf("Status = %+v, want %+v", got, first)
		}
	})

	t.Run("declined", func(t *testing.T) {
		req := payRequest(tokens.Declined)
		c := pay(t, p, req)
		if c.Status != payment.ChargeDeclined || c.DeclineCode == "" {
			t.Fatalf("Pay(%q) = %+v, want declined with a code", tokens.Declined, c)
		}
		if got := status(t, p, req.PaymentID); got.Status != payment.ChargeDeclined {
			t.Errorf("Status = %+v, want declined", got)
		}
	})

	t.Run("unknown payment", func(t *testing.T) {
		if got := status(t, p, uuid.NewV7()); got.Status != payment.ChargeNotFound {
			t.Errorf("Status of an unknown payment = %+v, want not found", got)
		}
	})

	t.Run("refund", func(t *testing.T) {
		req := payRequest(tokens.Success)
		c := pay(t, p, req)
		refund := payment.RefundRequest{
			PaymentID: req.PaymentID, ProviderRef: c.Ref, AmountCents: req.AmountCents, Currency: req.Currency,
		}
		for range 2 { // the second refund finds the charge refunded and succeeds as well
			if err := p.Refund(t.Context(), refund); err != nil {
				t.Fatalf("Refund: %v", err)
			}
		}
		if got := status(t, p, req.PaymentID); got.Status != payment.ChargeRefunded || got.Ref != c.Ref {
			t.Errorf("Status after refund = %+v, want refunded %s", got, c.Ref)
		}
	})
}

func payRequest(token string) payment.PayRequest {
	return payment.PayRequest{
		PaymentID: uuid.NewV7(), BookingID: uuid.NewV7(), AmountCents: 2500, Currency: "USD", Token: token,
	}
}

func pay(t *testing.T, p payment.Provider, req payment.PayRequest) payment.Charge {
	t.Helper()
	c, err := p.Pay(t.Context(), req)
	if err != nil {
		t.Fatalf("Pay(%q): %v", req.Token, err)
	}
	return c
}

func status(t *testing.T, p payment.Provider, id uuid.UUID) payment.Charge {
	t.Helper()
	c, err := p.Status(t.Context(), payment.StatusRequest{PaymentID: id})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return c
}
