package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

const testDSN = "postgres://postgres:root@localhost:5432/cinema?sslmode=disable"

func loadFrom(t *testing.T, vars map[string]string) (Config, error) {
	t.Helper()
	return load(env.Options{Environment: vars})
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{"DATABASE_URL": testDSN})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.DB.URL != testDSN {
		t.Errorf("DB.URL = %q, want %q", cfg.DB.URL, testDSN)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ShutdownTimeout != 15*time.Second {
		t.Errorf("HTTP.ShutdownTimeout = %s, want 15s", cfg.HTTP.ShutdownTimeout)
	}
	if cfg.DB.MaxConns != 20 || cfg.DB.MinConns != 2 {
		t.Errorf("DB pool = %d/%d, want 20/2", cfg.DB.MaxConns, cfg.DB.MinConns)
	}
	if cfg.Redis.Addr != "localhost:6379" {
		t.Errorf("Redis.Addr = %q, want localhost:6379", cfg.Redis.Addr)
	}
	if cfg.Redis.DialAttempts != 1 || cfg.Redis.MaxRetries != 1 || cfg.Redis.ReadTimeout != 500*time.Millisecond {
		t.Errorf("Redis dial attempts/retries/read timeout = %d/%d/%s, want 1/1/500ms (fail fast)",
			cfg.Redis.DialAttempts, cfg.Redis.MaxRetries, cfg.Redis.ReadTimeout)
	}
	if cfg.Log.Level != slog.LevelInfo || cfg.Log.Format != "json" {
		t.Errorf("Log = %v/%q, want INFO/json", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Cinema.Location.String() != "UTC" || cfg.Cinema.Currency != "USD" {
		t.Errorf("Cinema = %v/%q, want UTC/USD", cfg.Cinema.Location, cfg.Cinema.Currency)
	}
}

func TestLoadReadsCinemaTimeZone(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":    testDSN,
		"CINEMA_TIMEZONE": "Asia/Dubai",
		"CINEMA_CURRENCY": "AED",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Cinema.Location.String() != "Asia/Dubai" || cfg.Cinema.Currency != "AED" {
		t.Errorf("Cinema = %v/%q, want Asia/Dubai/AED", cfg.Cinema.Location, cfg.Cinema.Currency)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":          testDSN,
		"HTTP_ADDR":             ":9090",
		"HTTP_SHUTDOWN_TIMEOUT": "3s",
		"DB_MAX_CONNS":          "50",
		"REDIS_ADDR":            "redis:6379",
		"REDIS_DB":              "3",
		"LOG_LEVEL":             "debug",
		"LOG_FORMAT":            "text",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.HTTP.Addr != ":9090" || cfg.HTTP.ShutdownTimeout != 3*time.Second {
		t.Errorf("HTTP = %q/%s, want :9090/3s", cfg.HTTP.Addr, cfg.HTTP.ShutdownTimeout)
	}
	if cfg.DB.MaxConns != 50 {
		t.Errorf("DB.MaxConns = %d, want 50", cfg.DB.MaxConns)
	}
	if cfg.Redis.Addr != "redis:6379" || cfg.Redis.DB != 3 {
		t.Errorf("Redis = %q/%d, want redis:6379/3", cfg.Redis.Addr, cfg.Redis.DB)
	}
	if cfg.Log.Level != slog.LevelDebug || cfg.Log.Format != "text" {
		t.Errorf("Log = %v/%q, want DEBUG/text", cfg.Log.Level, cfg.Log.Format)
	}
}

func TestLoadRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{
			name:    "missing database url",
			vars:    map[string]string{},
			wantErr: "DATABASE_URL",
		},
		{
			name:    "empty database url",
			vars:    map[string]string{"DATABASE_URL": ""},
			wantErr: "DATABASE_URL",
		},
		{
			name:    "malformed duration",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HTTP_READ_TIMEOUT": "soon"},
			wantErr: "HTTP_READ_TIMEOUT",
		},
		{
			name:    "zero timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HTTP_SHUTDOWN_TIMEOUT": "0s"},
			wantErr: "HTTP_SHUTDOWN_TIMEOUT must be positive",
		},
		{
			name:    "min conns above max conns",
			vars:    map[string]string{"DATABASE_URL": testDSN, "DB_MAX_CONNS": "4", "DB_MIN_CONNS": "5"},
			wantErr: "DB_MIN_CONNS",
		},
		{
			name:    "zero max conns",
			vars:    map[string]string{"DATABASE_URL": testDSN, "DB_MAX_CONNS": "0", "DB_MIN_CONNS": "0"},
			wantErr: "DB_MAX_CONNS must be at least 1",
		},
		{
			name:    "negative redis db",
			vars:    map[string]string{"DATABASE_URL": testDSN, "REDIS_DB": "-1"},
			wantErr: "REDIS_DB",
		},
		{
			name:    "zero redis dial attempts",
			vars:    map[string]string{"DATABASE_URL": testDSN, "REDIS_DIAL_ATTEMPTS": "0"},
			wantErr: "REDIS_DIAL_ATTEMPTS",
		},
		{
			name:    "redis retries below -1",
			vars:    map[string]string{"DATABASE_URL": testDSN, "REDIS_MAX_RETRIES": "-2"},
			wantErr: "REDIS_MAX_RETRIES",
		},
		{
			name:    "unknown log format",
			vars:    map[string]string{"DATABASE_URL": testDSN, "LOG_FORMAT": "xml"},
			wantErr: "LOG_FORMAT",
		},
		{
			name:    "unknown time zone",
			vars:    map[string]string{"DATABASE_URL": testDSN, "CINEMA_TIMEZONE": "Mars/Olympus_Mons"},
			wantErr: "CINEMA_TIMEZONE",
		},
		{
			name:    "lowercase currency",
			vars:    map[string]string{"DATABASE_URL": testDSN, "CINEMA_CURRENCY": "usd"},
			wantErr: "CINEMA_CURRENCY",
		},
		{
			name:    "unknown log level",
			vars:    map[string]string{"DATABASE_URL": testDSN, "LOG_LEVEL": "loud"},
			wantErr: "LOG_LEVEL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadFrom(t, tt.vars)
			if err == nil {
				t.Fatalf("load succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{"DATABASE_URL": testDSN})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DB.MaxConns = 0
	cfg.Log.Format = "xml"

	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded, want error")
	}
	for _, want := range []string{"DB_MAX_CONNS", "LOG_FORMAT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %s", err, want)
		}
	}
}
