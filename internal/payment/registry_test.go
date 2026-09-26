package payment

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// named is a Provider that only has an identity.
type named struct{ id, name string }

func (n named) ID() string   { return n.id }
func (n named) Name() string { return n.name }

func (named) Pay(context.Context, PayRequest) (Charge, error)       { return Charge{}, nil }
func (named) Refund(context.Context, RefundRequest) error           { return nil }
func (named) Status(context.Context, StatusRequest) (Charge, error) { return Charge{}, nil }

func TestRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if got := r.Methods(); got == nil || len(got) != 0 {
		t.Errorf("Methods of an empty registry = %#v, want an empty, non-nil list", got)
	}

	for _, reg := range []struct {
		p       named
		enabled bool
	}{
		{p: named{"stripe", "Card"}, enabled: true},
		{p: named{"local", "Test card"}, enabled: false},
		{p: named{"paypal", "PayPal"}, enabled: true},
	} {
		if err := r.Register(reg.p, reg.enabled); err != nil {
			t.Fatalf("Register(%s): %v", reg.p.id, err)
		}
	}

	want := []Method{{ID: "stripe", Name: "Card"}, {ID: "paypal", Name: "PayPal"}}
	if got := r.Methods(); !slices.Equal(got, want) {
		t.Errorf("Methods = %v, want the enabled providers in registration order %v", got, want)
	}

	if p, ok := r.Enabled("stripe"); !ok || p.ID() != "stripe" {
		t.Errorf("Enabled(stripe) = %v, %t", p, ok)
	}
	if p, ok := r.Enabled("local"); ok || p != nil {
		t.Errorf("Enabled(local) = %v, %t; a disabled provider takes no new payments", p, ok)
	}
	if p, ok := r.Provider("local"); !ok || p.ID() != "local" {
		t.Errorf("Provider(local) = %v, %t; a disabled provider still settles its payments", p, ok)
	}
	for _, id := range []string{"unknown", ""} {
		if _, ok := r.Provider(id); ok {
			t.Errorf("Provider(%q) found a provider", id)
		}
		if _, ok := r.Enabled(id); ok {
			t.Errorf("Enabled(%q) found a provider", id)
		}
	}
}

func TestRegistryRejects(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(named{"local", "Test card"}, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(named{"local", "Another"}, false); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate id: err = %v", err)
	}
	if p, _ := r.Enabled("local"); p.Name() != "Test card" {
		t.Errorf("the duplicate replaced the first provider")
	}

	for _, id := range []string{"", "Local", "1pay", "pay-pal", "pay pal", strings.Repeat("a", 33)} {
		if err := r.Register(named{id, "x"}, true); err == nil {
			t.Errorf("Register accepted the invalid id %q", id)
		}
	}
	if !ValidID("pay_pal2") || !ValidID(strings.Repeat("a", 32)) {
		t.Error("ValidID rejects a valid id")
	}
}
