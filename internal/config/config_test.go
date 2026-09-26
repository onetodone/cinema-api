package config

import (
	"log/slog"
	"net/netip"
	"slices"
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
	if cfg.Auth.JWTSecret != "" || cfg.Auth.JWTTTL != time.Hour || cfg.Auth.BcryptCost != 12 {
		t.Errorf("Auth secret/ttl/cost = %q/%s/%d, want empty/1h/12",
			cfg.Auth.JWTSecret, cfg.Auth.JWTTTL, cfg.Auth.BcryptCost)
	}
	if cfg.Auth.AdminEmail != "admin@cinema.local" || cfg.Auth.AdminPassword != "" {
		t.Errorf("Auth admin = %q/%q, want admin@cinema.local and no password",
			cfg.Auth.AdminEmail, cfg.Auth.AdminPassword)
	}
	if cfg.Booking.HoldTTL != 15*time.Minute || cfg.Booking.MaxSeats != 10 || cfg.DB.LockTimeout != 3*time.Second {
		t.Errorf("Booking hold/max seats/lock timeout = %s/%d/%s, want 15m/10/3s",
			cfg.Booking.HoldTTL, cfg.Booking.MaxSeats, cfg.DB.LockTimeout)
	}
	if cfg.Expirer.Interval != 5*time.Second || cfg.Expirer.BatchSize != 500 {
		t.Errorf("Expirer interval/batch size = %s/%d, want 5s/500", cfg.Expirer.Interval, cfg.Expirer.BatchSize)
	}
	if cfg.Payment.Timeout != 10*time.Second || cfg.Payment.Grace != 2*time.Minute || cfg.Payment.Local.Enabled {
		t.Errorf("Payment = %+v, want 10s/2m with the local provider off", cfg.Payment)
	}
	if cfg.Reconciler.Interval != 30*time.Second {
		t.Errorf("Reconciler interval = %s, want 30s", cfg.Reconciler.Interval)
	}
	if cfg.Redis.KeyPrefix != "cinema" || cfg.Booking.HoldClaimTTL != 15*time.Second {
		t.Errorf("Redis key prefix/hold claim TTL = %q/%s, want cinema/15s", cfg.Redis.KeyPrefix, cfg.Booking.HoldClaimTTL)
	}
	if cfg.Cache.SeatMapTTL != 5*time.Second || cfg.Cache.ScheduleTTL != 10*time.Second {
		t.Errorf("Cache = %+v, want 5s/10s", cfg.Cache)
	}
	if want := (RateLimitConfig{BookingPerMin: 20, AuthIPPerMin: 30, LoginEmailPerMin: 10}); cfg.RateLimit != want {
		t.Errorf("RateLimit = %+v, want %+v", cfg.RateLimit, want)
	}
	if len(cfg.HTTP.TrustedProxies) != 0 {
		t.Errorf("HTTP.TrustedProxies = %v, want none", cfg.HTTP.TrustedProxies)
	}
	if want := (MetricsConfig{Addr: ":9090", WorkerAddr: ":9091"}); cfg.Metrics != want {
		t.Errorf("Metrics = %+v, want %+v", cfg.Metrics, want)
	}
}

func TestLoadReadsMetricsListeners(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":        testDSN,
		"METRICS_ADDR":        "127.0.0.1:9100",
		"WORKER_METRICS_ADDR": ":0",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := (MetricsConfig{Addr: "127.0.0.1:9100", WorkerAddr: ":0"}); cfg.Metrics != want {
		t.Errorf("Metrics = %+v, want %+v", cfg.Metrics, want)
	}
}

func TestLoadReadsRedisLayerSettings(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":                   testDSN,
		"REDIS_KEY_PREFIX":               "staging:cinema",
		"HOLD_CLAIM_TTL":                 "4s",
		"SEATMAP_CACHE_TTL":              "0s",
		"SCHEDULE_CACHE_TTL":             "1m",
		"BOOKING_RATE_LIMIT_PER_MIN":     "0",
		"AUTH_IP_RATE_LIMIT_PER_MIN":     "100",
		"LOGIN_EMAIL_RATE_LIMIT_PER_MIN": "5",
		"HTTP_TRUSTED_PROXIES":           "10.0.0.0/8,2001:db8::/32",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Redis.KeyPrefix != "staging:cinema" || cfg.Booking.HoldClaimTTL != 4*time.Second {
		t.Errorf("Redis key prefix/hold claim TTL = %q/%s", cfg.Redis.KeyPrefix, cfg.Booking.HoldClaimTTL)
	}
	if cfg.Cache.SeatMapTTL != 0 || cfg.Cache.ScheduleTTL != time.Minute {
		t.Errorf("Cache = %+v", cfg.Cache)
	}
	if want := (RateLimitConfig{BookingPerMin: 0, AuthIPPerMin: 100, LoginEmailPerMin: 5}); cfg.RateLimit != want {
		t.Errorf("RateLimit = %+v, want %+v", cfg.RateLimit, want)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8::/32")}
	if !slices.Equal(cfg.HTTP.TrustedProxies, want) {
		t.Errorf("HTTP.TrustedProxies = %v, want %v", cfg.HTTP.TrustedProxies, want)
	}
}

func TestLoadReadsPaymentSettings(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":          testDSN,
		"PAYMENT_TIMEOUT":       "3s",
		"PAYMENT_GRACE":         "4s",
		"PAYMENT_LOCAL_ENABLED": "true",
		"RECONCILER_INTERVAL":   "1s",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Payment.Timeout != 3*time.Second || cfg.Payment.Grace != 4*time.Second || !cfg.Payment.Local.Enabled {
		t.Errorf("Payment = %+v", cfg.Payment)
	}
	if cfg.Reconciler.Interval != time.Second {
		t.Errorf("Reconciler = %+v", cfg.Reconciler)
	}
}

func TestLoadReadsBookingSettings(t *testing.T) {
	t.Parallel()

	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":       testDSN,
		"BOOKING_HOLD_TTL":   "30s",
		"BOOKING_MAX_SEATS":  "4",
		"DB_LOCK_TIMEOUT":    "250ms",
		"EXPIRER_INTERVAL":   "1s",
		"EXPIRER_BATCH_SIZE": "5000",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Booking.HoldTTL != 30*time.Second || cfg.Booking.MaxSeats != 4 || cfg.DB.LockTimeout != 250*time.Millisecond {
		t.Errorf("Booking = %+v, lock timeout %s", cfg.Booking, cfg.DB.LockTimeout)
	}
	if cfg.Expirer.Interval != time.Second || cfg.Expirer.BatchSize != 5000 {
		t.Errorf("Expirer = %+v, want 1s/5000", cfg.Expirer)
	}
}

func TestLoadReadsAuthSettings(t *testing.T) {
	t.Parallel()

	secret := strings.Repeat("s", 32)
	cfg, err := loadFrom(t, map[string]string{
		"DATABASE_URL":   testDSN,
		"JWT_SECRET":     secret,
		"JWT_TTL":        "15m",
		"BCRYPT_COST":    "10",
		"ADMIN_EMAIL":    "root@example.com",
		"ADMIN_PASSWORD": "long enough",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.JWTSecret != secret || cfg.Auth.JWTTTL != 15*time.Minute || cfg.Auth.BcryptCost != 10 {
		t.Errorf("Auth = %+v", cfg.Auth)
	}
	if cfg.Auth.AdminEmail != "root@example.com" || cfg.Auth.AdminPassword != "long enough" {
		t.Errorf("Auth admin = %q/%q", cfg.Auth.AdminEmail, cfg.Auth.AdminPassword)
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
		"HTTP_ADDR":             ":9000",
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

	if cfg.HTTP.Addr != ":9000" || cfg.HTTP.ShutdownTimeout != 3*time.Second {
		t.Errorf("HTTP = %q/%s, want :9000/3s", cfg.HTTP.Addr, cfg.HTTP.ShutdownTimeout)
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
			name:    "short jwt secret",
			vars:    map[string]string{"DATABASE_URL": testDSN, "JWT_SECRET": "change-me"},
			wantErr: "JWT_SECRET must be at least 32 bytes, got 9",
		},
		{
			name:    "zero jwt ttl",
			vars:    map[string]string{"DATABASE_URL": testDSN, "JWT_TTL": "0s"},
			wantErr: "JWT_TTL",
		},
		{
			name:    "jwt ttl above a day",
			vars:    map[string]string{"DATABASE_URL": testDSN, "JWT_TTL": "25h"},
			wantErr: "JWT_TTL",
		},
		{
			name:    "weak bcrypt cost",
			vars:    map[string]string{"DATABASE_URL": testDSN, "BCRYPT_COST": "4"},
			wantErr: "BCRYPT_COST must be between 10 and 14, got 4",
		},
		{
			name:    "slow bcrypt cost",
			vars:    map[string]string{"DATABASE_URL": testDSN, "BCRYPT_COST": "15"},
			wantErr: "BCRYPT_COST",
		},
		{
			name:    "sub-millisecond lock timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "DB_LOCK_TIMEOUT": "500us"},
			wantErr: "DB_LOCK_TIMEOUT",
		},
		{
			name:    "lock timeout not below the write timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "DB_LOCK_TIMEOUT": "15s"},
			wantErr: "DB_LOCK_TIMEOUT must be at least 1ms and shorter than HTTP_WRITE_TIMEOUT (15s), got 15s",
		},
		{
			name:    "hold ttl too short",
			vars:    map[string]string{"DATABASE_URL": testDSN, "BOOKING_HOLD_TTL": "5s"},
			wantErr: "BOOKING_HOLD_TTL",
		},
		{
			name:    "hold ttl above a day",
			vars:    map[string]string{"DATABASE_URL": testDSN, "BOOKING_HOLD_TTL": "25h"},
			wantErr: "BOOKING_HOLD_TTL",
		},
		{
			name:    "zero max seats",
			vars:    map[string]string{"DATABASE_URL": testDSN, "BOOKING_MAX_SEATS": "0"},
			wantErr: "BOOKING_MAX_SEATS must be between 1 and 50, got 0",
		},
		{
			name:    "sub-second expirer interval",
			vars:    map[string]string{"DATABASE_URL": testDSN, "EXPIRER_INTERVAL": "500ms"},
			wantErr: "EXPIRER_INTERVAL must be between 1s and 10m0s, got 500ms",
		},
		{
			name:    "expirer interval above ten minutes",
			vars:    map[string]string{"DATABASE_URL": testDSN, "EXPIRER_INTERVAL": "11m"},
			wantErr: "EXPIRER_INTERVAL",
		},
		{
			name:    "zero expirer batch size",
			vars:    map[string]string{"DATABASE_URL": testDSN, "EXPIRER_BATCH_SIZE": "0"},
			wantErr: "EXPIRER_BATCH_SIZE must be between 1 and 5000, got 0",
		},
		{
			name:    "huge expirer batch size",
			vars:    map[string]string{"DATABASE_URL": testDSN, "EXPIRER_BATCH_SIZE": "5001"},
			wantErr: "EXPIRER_BATCH_SIZE",
		},
		{
			name:    "sub-second payment timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "PAYMENT_TIMEOUT": "500ms"},
			wantErr: "PAYMENT_TIMEOUT must be at least 1s and shorter than HTTP_WRITE_TIMEOUT (15s), got 500ms",
		},
		{
			name:    "payment timeout not below the write timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "PAYMENT_TIMEOUT": "15s", "PAYMENT_GRACE": "1m"},
			wantErr: "PAYMENT_TIMEOUT",
		},
		{
			name:    "payment grace not above the payment timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "PAYMENT_TIMEOUT": "10s", "PAYMENT_GRACE": "10s"},
			wantErr: "PAYMENT_GRACE must be longer than PAYMENT_TIMEOUT (10s) and at most 1h0m0s, got 10s",
		},
		{
			name:    "payment grace above an hour",
			vars:    map[string]string{"DATABASE_URL": testDSN, "PAYMENT_GRACE": "61m"},
			wantErr: "PAYMENT_GRACE",
		},
		{
			name:    "malformed local provider switch",
			vars:    map[string]string{"DATABASE_URL": testDSN, "PAYMENT_LOCAL_ENABLED": "sure"},
			wantErr: "PAYMENT_LOCAL_ENABLED",
		},
		{
			name:    "sub-second reconciler interval",
			vars:    map[string]string{"DATABASE_URL": testDSN, "RECONCILER_INTERVAL": "100ms"},
			wantErr: "RECONCILER_INTERVAL must be between 1s and 10m0s, got 100ms",
		},
		{
			name:    "reconciler interval above ten minutes",
			vars:    map[string]string{"DATABASE_URL": testDSN, "RECONCILER_INTERVAL": "11m"},
			wantErr: "RECONCILER_INTERVAL",
		},
		{
			name:    "unknown log level",
			vars:    map[string]string{"DATABASE_URL": testDSN, "LOG_LEVEL": "loud"},
			wantErr: "LOG_LEVEL",
		},
		{
			name:    "redis key prefix with a space",
			vars:    map[string]string{"DATABASE_URL": testDSN, "REDIS_KEY_PREFIX": "my cinema"},
			wantErr: "REDIS_KEY_PREFIX",
		},
		{
			name:    "redis key prefix with a hash tag",
			vars:    map[string]string{"DATABASE_URL": testDSN, "REDIS_KEY_PREFIX": "cinema{1}"},
			wantErr: `REDIS_KEY_PREFIX must be 1 to 64 letters, digits, or any of _ . - :, got "cinema{1}"`,
		},
		{
			name:    "hold claim not longer than the lock timeout",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HOLD_CLAIM_TTL": "3s"},
			wantErr: "HOLD_CLAIM_TTL must be longer than DB_LOCK_TIMEOUT (3s) and at most 1m0s, got 3s",
		},
		{
			name:    "hold claim above a minute",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HOLD_CLAIM_TTL": "61s"},
			wantErr: "HOLD_CLAIM_TTL",
		},
		{
			name:    "negative seat map cache ttl",
			vars:    map[string]string{"DATABASE_URL": testDSN, "SEATMAP_CACHE_TTL": "-1s"},
			wantErr: "SEATMAP_CACHE_TTL must be 0 (off) or between 1ms and 1m0s, got -1s",
		},
		{
			name:    "sub-millisecond seat map cache ttl",
			vars:    map[string]string{"DATABASE_URL": testDSN, "SEATMAP_CACHE_TTL": "10us"},
			wantErr: "SEATMAP_CACHE_TTL",
		},
		{
			name:    "schedule cache ttl above a minute",
			vars:    map[string]string{"DATABASE_URL": testDSN, "SCHEDULE_CACHE_TTL": "2m"},
			wantErr: "SCHEDULE_CACHE_TTL",
		},
		{
			name:    "negative booking rate limit",
			vars:    map[string]string{"DATABASE_URL": testDSN, "BOOKING_RATE_LIMIT_PER_MIN": "-1"},
			wantErr: "BOOKING_RATE_LIMIT_PER_MIN must be between 0 (off) and 10000, got -1",
		},
		{
			name:    "huge auth rate limit",
			vars:    map[string]string{"DATABASE_URL": testDSN, "AUTH_IP_RATE_LIMIT_PER_MIN": "10001"},
			wantErr: "AUTH_IP_RATE_LIMIT_PER_MIN",
		},
		{
			name:    "negative login rate limit",
			vars:    map[string]string{"DATABASE_URL": testDSN, "LOGIN_EMAIL_RATE_LIMIT_PER_MIN": "-5"},
			wantErr: "LOGIN_EMAIL_RATE_LIMIT_PER_MIN",
		},
		{
			name:    "trusted proxy without a prefix length",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HTTP_TRUSTED_PROXIES": "10.0.0.1"},
			wantErr: "HTTP_TRUSTED_PROXIES",
		},
		{
			name:    "api address without a port",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HTTP_ADDR": "localhost"},
			wantErr: `HTTP_ADDR must be host:port or :port, got "localhost"`,
		},
		{
			name:    "metrics address without a port",
			vars:    map[string]string{"DATABASE_URL": testDSN, "WORKER_METRICS_ADDR": "9091"},
			wantErr: "WORKER_METRICS_ADDR",
		},
		{
			name:    "metrics on the api address",
			vars:    map[string]string{"DATABASE_URL": testDSN, "HTTP_ADDR": ":8080", "METRICS_ADDR": ":8080"},
			wantErr: "METRICS_ADDR must differ from HTTP_ADDR",
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
