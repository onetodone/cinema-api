// Package principal stores the authenticated caller in a request context. The auth middleware writes it and
// handlers read it; like requestid, it sits below both to avoid an import cycle.
package principal

import (
	"context"

	"github.com/onetodone/cinema-api/internal/domain"
)

type key struct{}

// NewContext returns a copy of ctx that carries p.
func NewContext(ctx context.Context, p domain.Principal) context.Context {
	return context.WithValue(ctx, key{}, p)
}

// FromContext returns the caller stored in ctx. ok is false on routes without authentication.
func FromContext(ctx context.Context) (p domain.Principal, ok bool) {
	p, ok = ctx.Value(key{}).(domain.Principal)
	return p, ok
}
