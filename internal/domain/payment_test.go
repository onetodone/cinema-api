package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"
)

func TestPaymentTransitions(t *testing.T) {
	t.Parallel()

	statuses := []PaymentStatus{PaymentPending, PaymentSucceeded, PaymentFailed, PaymentRefunded}
	allowed := map[[2]PaymentStatus]bool{
		{PaymentPending, PaymentSucceeded}: true,
		{PaymentPending, PaymentFailed}:    true,
		{PaymentPending, PaymentRefunded}:  true,
		{PaymentFailed, PaymentRefunded}:   true, // a charge that landed after the worker gave up on it
	}
	for _, from := range statuses {
		for _, to := range statuses {
			if got := from.CanBecome(to); got != allowed[[2]PaymentStatus{from, to}] {
				t.Errorf("%s.CanBecome(%s) = %t", from, to, got)
			}
		}
	}
}

func TestNewPaymentValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     NewPayment
		fields []string
	}{
		{name: "valid", in: NewPayment{Method: "local", Token: "tok_success"}},
		{name: "longest token", in: NewPayment{Method: "local", Token: strings.Repeat("t", 255)}},
		{name: "nothing", in: NewPayment{}, fields: []string{"payment_method", "payment_token"}},
		{name: "blank", in: NewPayment{Method: " ", Token: "\t"}, fields: []string{"payment_method", "payment_token"}},
		{name: "token too long", in: NewPayment{Method: "local", Token: strings.Repeat("t", 256)}, fields: []string{"payment_token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.in.Validate()
			var got []string
			var ve *ValidationError
			if errors.As(err, &ve) {
				for _, f := range ve.Fields {
					got = append(got, f.Field)
				}
			} else if err != nil {
				t.Fatalf("Validate() = %v, want a *ValidationError", err)
			}
			if !slices.Equal(got, tt.fields) {
				t.Errorf("invalid fields = %v, want %v (%v)", got, tt.fields, err)
			}
		})
	}
}

func TestCheckPayable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status   BookingStatus
		holdOver bool
		kind     error
		code     string
	}{
		{status: BookingPending},
		{status: BookingPending, holdOver: true, kind: ErrGone, code: CodeBookingExpired},
		{status: BookingProcessing, kind: ErrConflict, code: CodePaymentInProgress},
		{status: BookingPaid, kind: ErrConflict, code: CodeBookingAlreadyPaid},
		{status: BookingExpired, kind: ErrGone, code: CodeBookingExpired},
		{status: BookingCanceled, kind: ErrConflict, code: CodeBookingCanceled},
	}
	for _, tt := range tests {
		err := CheckPayable(Booking{ID: uuid.NewV7(), Status: tt.status}, tt.holdOver)
		if tt.kind == nil {
			if err != nil {
				t.Errorf("%s (hold over %t): %v, want payable", tt.status, tt.holdOver, err)
			}
			continue
		}
		var de *Error
		if !errors.Is(err, tt.kind) || !errors.As(err, &de) || de.Code != tt.code {
			t.Errorf("%s (hold over %t): %v, want %s", tt.status, tt.holdOver, err, tt.code)
		}
	}
}

func TestPaymentDeclined(t *testing.T) {
	t.Parallel()

	err := PaymentDeclined("insufficient_funds")
	var declined *PaymentDeclinedError
	if !errors.As(err, &declined) || declined.DeclineCode != "insufficient_funds" {
		t.Fatalf("errors.As(%v) = %+v", err, declined)
	}
	var de *Error
	if !errors.Is(err, ErrPaymentRequired) || !errors.As(err, &de) || de.Code != CodePaymentDeclined {
		t.Errorf("err = %v, want a PAYMENT_DECLINED error of kind ErrPaymentRequired", err)
	}
	if !strings.Contains(err.Error(), "insufficient_funds") {
		t.Errorf("message %q does not name the decline code", err.Error())
	}
}

func TestErrorBuildersForPayments(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		err  error
		kind error
	}{
		{err: Gone("X", "gone %d", 1), kind: ErrGone},
		{err: Unavailable("Y", "down %d", 2), kind: ErrUnavailable},
	} {
		var de *Error
		if !errors.Is(tt.err, tt.kind) || !errors.As(tt.err, &de) || de.Message == "" {
			t.Errorf("%v: want kind %v", tt.err, tt.kind)
		}
	}
}
