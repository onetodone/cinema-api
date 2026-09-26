package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	return rec
}

func TestContextAttrsAreAddedToRecords(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, slog.LevelInfo, "json")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := WithAttrs(context.Background(), slog.String("request_id", "req-1"))
	ctx = WithAttrs(ctx, slog.String("user_id", "u-1"))
	logger.InfoContext(ctx, "hello", slog.Int("n", 1))

	rec := decodeLine(t, &buf)
	for key, want := range map[string]any{"msg": "hello", "request_id": "req-1", "user_id": "u-1", "n": 1.0} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
		}
	}
}

func TestWithAttrsDoesNotMutateParentContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, slog.LevelInfo, "json")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	parent := WithAttrs(context.Background(), slog.String("request_id", "req-1"))
	_ = WithAttrs(parent, slog.String("user_id", "u-1"))
	logger.InfoContext(parent, "parent only")

	rec := decodeLine(t, &buf)
	if _, ok := rec["user_id"]; ok {
		t.Errorf("parent context leaked child attribute: %v", rec)
	}
}

func TestDerivedLoggersKeepContextAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, slog.LevelInfo, "json")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := WithAttrs(context.Background(), slog.String("request_id", "req-1"))
	logger.With(slog.String("component", "worker")).InfoContext(ctx, "tick")

	rec := decodeLine(t, &buf)
	if rec["request_id"] != "req-1" || rec["component"] != "worker" {
		t.Errorf("record = %v, want request_id and component", rec)
	}
}

func TestLevelFiltersRecords(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := New(&buf, slog.LevelWarn, "text")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Info("dropped")
	logger.Warn("kept")

	out := buf.String()
	if strings.Contains(out, "dropped") || !strings.Contains(out, "kept") {
		t.Errorf("output = %q, want only the warning", out)
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	t.Parallel()

	if _, err := New(&bytes.Buffer{}, slog.LevelInfo, "xml"); err == nil {
		t.Fatal("New succeeded, want error for unknown format")
	}
}
