package payment

import (
	"fmt"
	"sync"
)

// Method is a provider as clients see it when they choose how to pay.
type Method struct {
	ID   string
	Name string
}

// Registry holds the configured payment providers and knows which of them take new payments. It is safe for
// concurrent use.
//
// A provider that is registered but disabled takes no new payments, but payments that are already in flight
// still settle through it: switching a provider off never strands a payment.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	enabled   map[string]bool
	order     []string // registration order, which is the order clients see
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}, enabled: map[string]bool{}}
}

// Register adds a provider, enabled for new payments or not. It fails for an invalid id or an id that is
// already registered.
func (r *Registry) Register(p Provider, enabled bool) error {
	id := p.ID()
	if !ValidID(id) {
		return fmt.Errorf("register payment provider: invalid id %q", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[id]; ok {
		return fmt.Errorf("register payment provider: id %q is already registered", id)
	}
	r.providers[id] = p
	r.enabled[id] = enabled
	r.order = append(r.order, id)
	return nil
}

// Provider returns the provider with this id, whether or not it takes new payments.
func (r *Registry) Provider(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// Enabled returns the provider with this id if it takes new payments.
func (r *Registry) Enabled(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.enabled[id] {
		return nil, false
	}
	return r.providers[id], true
}

// Methods lists the providers that take new payments, in registration order.
func (r *Registry) Methods() []Method {
	r.mu.RLock()
	defer r.mu.RUnlock()
	methods := []Method{}
	for _, id := range r.order {
		if r.enabled[id] {
			methods = append(methods, Method{ID: id, Name: r.providers[id].Name()})
		}
	}
	return methods
}
