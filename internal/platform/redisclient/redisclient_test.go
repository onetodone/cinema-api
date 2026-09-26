package redisclient

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/config"
)

// Not parallel: UseLogger sets the process-wide logger of go-redis.
func TestUseLoggerSendsGoRedisLinesToSlog(t *testing.T) {
	var buf bytes.Buffer
	UseLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Nothing listens on port 1, so the dial fails at once and go-redis logs it.
	rdb := New(config.RedisConfig{
		Addr: "127.0.0.1:1", DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		DialAttempts: 1, MaxRetries: -1,
	}, "test")
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(t.Context()).Err(); err == nil {
		t.Fatal("ping of a closed port succeeded")
	}

	out := buf.String()
	if !strings.Contains(out, `"level":"DEBUG"`) || !strings.Contains(out, `"component":"go-redis"`) ||
		!strings.Contains(out, "connection refused") {
		t.Errorf("slog output = %q, want the failed dial at debug level", out)
	}
}
