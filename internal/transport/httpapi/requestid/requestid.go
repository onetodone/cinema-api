// Package requestid stores the request ID in a context. It sits below middleware and problem so both can use it.
package requestid

import "context"

type key struct{}

// NewContext returns a copy of ctx that carries id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// FromContext returns the request ID stored in ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
