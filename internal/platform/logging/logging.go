// Package logging builds the application's slog logger and carries request-scoped attributes in a context.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// New returns a logger that writes to w in the given format ("json" or "text").
// Every record logged with a context also carries the attributes stored in that context by WithAttrs.
func New(w io.Writer, level slog.Level, format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}

	return slog.New(contextHandler{Handler: h}), nil
}

type attrsKey struct{}

// WithAttrs returns a copy of ctx whose log records will include attrs in addition to any attributes already
// stored in ctx.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, attrsKey{}, merged)
}

// contextHandler decorates a slog.Handler so that attributes stored in the context are added to each record.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(attrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
