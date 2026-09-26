// Package middleware contains the HTTP middlewares shared by all routes.
package middleware

import "net/http"

// Middleware wraps an http.Handler with extra behavior.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with mws so that the first middleware listed is the outermost one.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
