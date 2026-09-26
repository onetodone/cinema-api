// Package config loads and validates the application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the complete runtime configuration shared by all binaries.
type Config struct {
	HTTP       HTTPConfig
	Metrics    MetricsConfig
	DB         DBConfig
	Redis      RedisConfig
	Log        LogConfig
	Cinema     CinemaConfig
	Auth       AuthConfig
	Booking    BookingConfig
	Expirer    ExpirerConfig
	Payment    PaymentConfig
	Reconciler ReconcilerConfig
	Cache      CacheConfig
	RateLimit  RateLimitConfig
}

// Limits for authentication settings.
const (
	// minJWTSecretBytes is the smallest HS256 key RFC 7518 §3.2 allows: as long as the hash output.
	minJWTSecretBytes = 32
	maxJWTTTL         = 24 * time.Hour
	minBcryptCost     = 10 // OWASP minimum
	maxBcryptCost     = 14 // about 0.7 s per hash on the development machine; more would make logins a DoS vector
)

// Limits for booking settings.
const (
	minHoldTTL      = 10 * time.Second
	maxHoldTTL      = 24 * time.Hour
	maxSeatsCeiling = 50
)

// Limits for the expiry worker.
const (
	minExpirerInterval = time.Second
	// maxExpirerInterval bounds how long seats stay held after their hold has run out.
	maxExpirerInterval = 10 * time.Minute
	// maxExpirerBatchSize bounds the rows one transaction locks: up to BOOKING_MAX_SEATS seats per booking.
	maxExpirerBatchSize = 5000
)

// Limits for payments.
const (
	minPaymentTimeout = time.Second
	maxPaymentGrace   = time.Hour
)

// Limits for the payment reconciler.
const (
	minReconcilerInterval = time.Second
	maxReconcilerInterval = 10 * time.Minute
)

// Limits for the Redis layer.
const (
	maxHoldClaimTTL = time.Minute
	// maxCacheTTL bounds how stale a cached seat map or schedule may be.
	maxCacheTTL      = time.Minute
	maxRateLimit     = 10000
	maxKeyPrefixSize = 64
)

// CacheConfig configures the Redis read caches of the catalog. A TTL of 0 turns that cache off.
type CacheConfig struct {
	// SeatMapTTL is how long a seat map is served from the cache. Every committed seat change also deletes the
	// cached seat map of its showtime, so the TTL only bounds the staleness left by a lost race or a failed delete.
	SeatMapTTL time.Duration `env:"SEATMAP_CACHE_TTL" envDefault:"5s"`
	// ScheduleTTL is how long the schedule of a day is served from the cache. It bounds how stale the free seat
	// counts in the schedule may be, because bookings do not delete cached schedules.
	ScheduleTTL time.Duration `env:"SCHEDULE_CACHE_TTL" envDefault:"10s"`
}

// RateLimitConfig configures the per-minute limits on attempts. A limit of 0 turns it off. The limits count in
// Redis and let every request through while Redis is unavailable.
type RateLimitConfig struct {
	// BookingPerMin caps booking attempts per user.
	BookingPerMin int `env:"BOOKING_RATE_LIMIT_PER_MIN" envDefault:"20"`
	// AuthIPPerMin caps login and registration attempts per client IP address (per /64 network for IPv6).
	AuthIPPerMin int `env:"AUTH_IP_RATE_LIMIT_PER_MIN" envDefault:"30"`
	// LoginEmailPerMin caps login attempts per account email, whichever address they come from.
	LoginEmailPerMin int `env:"LOGIN_EMAIL_RATE_LIMIT_PER_MIN" envDefault:"10"`
}

// PaymentConfig configures payments and the payment providers. Each provider has its own block of settings; a
// provider that is not enabled takes no new payments, but still settles the payments it has in flight.
type PaymentConfig struct {
	// Timeout bounds each call to a payment provider. A charge without an answer by then is settled later by
	// the worker.
	Timeout time.Duration `env:"PAYMENT_TIMEOUT" envDefault:"10s"`
	// Grace is how long a payment may stay in flight before the worker asks its provider what became of it.
	// It must be longer than Timeout, so the API has given up on the charge by then.
	Grace time.Duration `env:"PAYMENT_GRACE" envDefault:"2m"`
	Local LocalPaymentConfig
}

// LocalPaymentConfig configures the local test provider (internal/payment/local).
type LocalPaymentConfig struct {
	// Enabled offers the local provider to clients. It accepts test tokens and moves no money, so anyone could
	// buy tickets for free: it is off unless switched on, and must stay off in production.
	Enabled bool `env:"PAYMENT_LOCAL_ENABLED" envDefault:"false"`
}

// ReconcilerConfig configures the worker job that settles payments the API did not settle.
type ReconcilerConfig struct {
	// Interval is the average pause between two passes over stuck payments, varied by up to 20% like the
	// expirer's.
	Interval time.Duration `env:"RECONCILER_INTERVAL" envDefault:"30s"`
}

// ExpirerConfig configures the worker that expires unpaid bookings.
type ExpirerConfig struct {
	// Interval is the average pause between two sweeps. Each pause varies at random by up to 20%, so worker
	// replicas that started together do not query the database in lockstep.
	Interval time.Duration `env:"EXPIRER_INTERVAL"   envDefault:"5s"`
	// BatchSize is the most bookings one transaction expires. A sweep runs batches until one comes back short.
	BatchSize int `env:"EXPIRER_BATCH_SIZE" envDefault:"500"`
}

// BookingConfig configures seat holds.
type BookingConfig struct {
	// HoldTTL is how long a booking holds its seats while waiting for payment.
	HoldTTL time.Duration `env:"BOOKING_HOLD_TTL"  envDefault:"15m"`
	// MaxSeats is the most seats one booking may hold.
	MaxSeats int `env:"BOOKING_MAX_SEATS" envDefault:"10"`
	// HoldClaimTTL is how long the Redis hold gate reserves seats for a booking request whose transaction has not
	// committed yet. It must outlast the transaction's lock waits (DB_LOCK_TIMEOUT); a claim left behind by a
	// crashed request blocks its seats for at most this long.
	HoldClaimTTL time.Duration `env:"HOLD_CLAIM_TTL" envDefault:"15s"`
}

// AuthConfig configures accounts and access tokens.
type AuthConfig struct {
	// JWTSecret signs access tokens (HS256). Only the API needs it, so it is checked where tokens are built;
	// here it is only rejected when it is set but too short.
	JWTSecret string        `env:"JWT_SECRET"`
	JWTTTL    time.Duration `env:"JWT_TTL"     envDefault:"1h"`
	// BcryptCost is the work factor of password hashes. Each step doubles the time of a login.
	BcryptCost int `env:"BCRYPT_COST" envDefault:"12"`
	// AdminEmail and AdminPassword describe the admin account that cmd/seed creates or updates.
	// Without a password the seed skips the admin account.
	AdminEmail    string `env:"ADMIN_EMAIL"    envDefault:"admin@cinema.local"`
	AdminPassword string `env:"ADMIN_PASSWORD"`
}

// CinemaConfig describes the cinema itself.
type CinemaConfig struct {
	// Location is the cinema's IANA time zone. Schedule days start at local midnight, and times in responses
	// carry this zone's offset.
	Location *time.Location `env:"CINEMA_TIMEZONE" envDefault:"UTC"`
	// Currency is the ISO 4217 code of all prices.
	Currency string `env:"CINEMA_CURRENCY" envDefault:"USD"`
}

// HTTPConfig configures the HTTP server.
type HTTPConfig struct {
	Addr              string        `env:"HTTP_ADDR"                envDefault:":8080"`
	ReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"5s"`
	ReadTimeout       time.Duration `env:"HTTP_READ_TIMEOUT"        envDefault:"10s"`
	WriteTimeout      time.Duration `env:"HTTP_WRITE_TIMEOUT"       envDefault:"15s"`
	IdleTimeout       time.Duration `env:"HTTP_IDLE_TIMEOUT"        envDefault:"60s"`
	ShutdownTimeout   time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT"    envDefault:"15s"`
	// TrustedProxies lists the networks of reverse proxies and load balancers, in CIDR notation. A request from
	// one of them is attributed to the client address its X-Forwarded-For header names; any other request to its
	// connection's address. Empty trusts nobody, which is right when clients connect directly.
	TrustedProxies []netip.Prefix `env:"HTTP_TRUSTED_PROXIES" envSeparator:","`
}

// MetricsConfig configures the listeners that serve the Prometheus metrics. They are separate from HTTP_ADDR, so
// that the metrics can stay on an internal network while the API is public.
type MetricsConfig struct {
	// Addr is the API's listener for GET /metrics.
	Addr string `env:"METRICS_ADDR" envDefault:":9090"`
	// WorkerAddr is the worker's listener for GET /metrics, /healthz, and /readyz. Workers that run on one host
	// need an address each; a port of 0 picks a free port, which the worker logs.
	WorkerAddr string `env:"WORKER_METRICS_ADDR" envDefault:":9091"`
}

// DBConfig configures the PostgreSQL connection pool.
type DBConfig struct {
	URL             string        `env:"DATABASE_URL,required,notEmpty"`
	MaxConns        int32         `env:"DB_MAX_CONNS"          envDefault:"20"`
	MinConns        int32         `env:"DB_MIN_CONNS"          envDefault:"2"`
	MaxConnLifetime time.Duration `env:"DB_MAX_CONN_LIFETIME"  envDefault:"30m"`
	MaxConnIdleTime time.Duration `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"5m"`
	ConnectTimeout  time.Duration `env:"DB_CONNECT_TIMEOUT"    envDefault:"5s"`
	// LockTimeout bounds how long a booking transaction waits for a row lock. It turns a pile-up on a hot seat
	// into a quick "busy, retry" answer instead of a request that hangs until the HTTP timeout.
	LockTimeout time.Duration `env:"DB_LOCK_TIMEOUT" envDefault:"3s"`
}

// RedisConfig configures the Redis client. Redis is a fail-open accelerator, so the defaults favor failing fast
// over retrying: during an outage every request would otherwise pay the full retry and timeout budget.
type RedisConfig struct {
	Addr         string        `env:"REDIS_ADDR"          envDefault:"localhost:6379"`
	Password     string        `env:"REDIS_PASSWORD"`
	DB           int           `env:"REDIS_DB"            envDefault:"0"`
	DialTimeout  time.Duration `env:"REDIS_DIAL_TIMEOUT"  envDefault:"1s"`
	ReadTimeout  time.Duration `env:"REDIS_READ_TIMEOUT"  envDefault:"500ms"`
	WriteTimeout time.Duration `env:"REDIS_WRITE_TIMEOUT" envDefault:"500ms"`
	// DialAttempts caps dial attempts per new connection; REDIS_DIAL_TIMEOUT applies to each attempt.
	// go-redis defaults to 5 attempts with 100 ms pauses, which adds ~0.4 s per command while Redis is down.
	DialAttempts int `env:"REDIS_DIAL_ATTEMPTS" envDefault:"1"`
	// MaxRetries: -1 disables retries. One retry covers a stale pooled connection after a Redis restart.
	MaxRetries int `env:"REDIS_MAX_RETRIES" envDefault:"1"`
	// KeyPrefix starts every key the application writes, so that several deployments can share one Redis.
	KeyPrefix string `env:"REDIS_KEY_PREFIX" envDefault:"cinema"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level  slog.Level `env:"LOG_LEVEL"  envDefault:"info"`
	Format string     `env:"LOG_FORMAT" envDefault:"json"`
}

// Load reads the configuration from the process environment and validates it.
func Load() (Config, error) {
	return load(env.Options{})
}

func load(opts env.Options) (Config, error) {
	cfg, err := env.ParseAsWithOptions[Config](opts)
	if err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", nameVariables(err))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// nameVariables rewrites value-parsing errors so they name the environment variable instead of the Go struct
// field, because the variable is what an operator has to fix. Other errors already name the variable.
func nameVariables(err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}

	keys := envKeysByField(reflect.TypeFor[Config]())
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if errors.As(e, &pe) && len(keys[pe.Name]) > 0 {
			out = append(out, fmt.Errorf("%s: %w", strings.Join(keys[pe.Name], " or "), pe.Err))
			continue
		}
		out = append(out, e)
	}
	return errors.Join(out...)
}

// envKeysByField maps each struct field name to the environment variables declared for it. A name can map to
// several variables when nested structs reuse a field name.
func envKeysByField(t reflect.Type) map[string][]string {
	keys := make(map[string][]string)
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("env")
			if tag == "" && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if key, _, _ := strings.Cut(tag, ","); key != "" {
				keys[f.Name] = append(keys[f.Name], key)
			}
		}
	}
	walk(t)
	return keys
}

// Validate checks cross-field and range constraints that struct tags cannot express.
func (c Config) Validate() error {
	var errs []error

	positive := map[string]time.Duration{
		"HTTP_READ_HEADER_TIMEOUT": c.HTTP.ReadHeaderTimeout,
		"HTTP_READ_TIMEOUT":        c.HTTP.ReadTimeout,
		"HTTP_WRITE_TIMEOUT":       c.HTTP.WriteTimeout,
		"HTTP_IDLE_TIMEOUT":        c.HTTP.IdleTimeout,
		"HTTP_SHUTDOWN_TIMEOUT":    c.HTTP.ShutdownTimeout,
		"DB_CONNECT_TIMEOUT":       c.DB.ConnectTimeout,
		"REDIS_DIAL_TIMEOUT":       c.Redis.DialTimeout,
		"REDIS_READ_TIMEOUT":       c.Redis.ReadTimeout,
		"REDIS_WRITE_TIMEOUT":      c.Redis.WriteTimeout,
	}
	for name, d := range positive {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", name, d))
		}
	}

	if c.DB.MaxConns < 1 {
		errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be at least 1, got %d", c.DB.MaxConns))
	}
	if c.DB.MinConns < 0 || c.DB.MinConns > c.DB.MaxConns {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS (%d), got %d",
			c.DB.MaxConns, c.DB.MinConns))
	}
	if c.Redis.DB < 0 {
		errs = append(errs, fmt.Errorf("REDIS_DB must not be negative, got %d", c.Redis.DB))
	}
	if c.Redis.DialAttempts < 1 {
		errs = append(errs, fmt.Errorf("REDIS_DIAL_ATTEMPTS must be at least 1, got %d", c.Redis.DialAttempts))
	}
	if c.Redis.MaxRetries < -1 {
		errs = append(errs, fmt.Errorf("REDIS_MAX_RETRIES must be -1 (disabled) or more, got %d", c.Redis.MaxRetries))
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be %q or %q, got %q", "json", "text", c.Log.Format))
	}
	if c.Auth.JWTSecret != "" && len(c.Auth.JWTSecret) < minJWTSecretBytes {
		errs = append(errs, fmt.Errorf("JWT_SECRET must be at least %d bytes, got %d",
			minJWTSecretBytes, len(c.Auth.JWTSecret)))
	}
	if c.Auth.JWTTTL <= 0 || c.Auth.JWTTTL > maxJWTTTL {
		errs = append(errs, fmt.Errorf("JWT_TTL must be positive and at most %s, got %s", maxJWTTTL, c.Auth.JWTTTL))
	}
	if c.Auth.BcryptCost < minBcryptCost || c.Auth.BcryptCost > maxBcryptCost {
		errs = append(errs, fmt.Errorf("BCRYPT_COST must be between %d and %d, got %d",
			minBcryptCost, maxBcryptCost, c.Auth.BcryptCost))
	}
	if c.DB.LockTimeout < time.Millisecond || c.DB.LockTimeout >= c.HTTP.WriteTimeout {
		errs = append(errs, fmt.Errorf("DB_LOCK_TIMEOUT must be at least 1ms and shorter than HTTP_WRITE_TIMEOUT (%s), got %s",
			c.HTTP.WriteTimeout, c.DB.LockTimeout))
	}
	if c.Booking.HoldTTL < minHoldTTL || c.Booking.HoldTTL > maxHoldTTL {
		errs = append(errs, fmt.Errorf("BOOKING_HOLD_TTL must be between %s and %s, got %s",
			minHoldTTL, maxHoldTTL, c.Booking.HoldTTL))
	}
	if c.Booking.MaxSeats < 1 || c.Booking.MaxSeats > maxSeatsCeiling {
		errs = append(errs, fmt.Errorf("BOOKING_MAX_SEATS must be between 1 and %d, got %d",
			maxSeatsCeiling, c.Booking.MaxSeats))
	}
	if c.Expirer.Interval < minExpirerInterval || c.Expirer.Interval > maxExpirerInterval {
		errs = append(errs, fmt.Errorf("EXPIRER_INTERVAL must be between %s and %s, got %s",
			minExpirerInterval, maxExpirerInterval, c.Expirer.Interval))
	}
	if c.Expirer.BatchSize < 1 || c.Expirer.BatchSize > maxExpirerBatchSize {
		errs = append(errs, fmt.Errorf("EXPIRER_BATCH_SIZE must be between 1 and %d, got %d",
			maxExpirerBatchSize, c.Expirer.BatchSize))
	}
	if c.Payment.Timeout < minPaymentTimeout || c.Payment.Timeout >= c.HTTP.WriteTimeout {
		errs = append(errs, fmt.Errorf("PAYMENT_TIMEOUT must be at least %s and shorter than HTTP_WRITE_TIMEOUT (%s), got %s",
			minPaymentTimeout, c.HTTP.WriteTimeout, c.Payment.Timeout))
	}
	if c.Payment.Grace <= c.Payment.Timeout || c.Payment.Grace > maxPaymentGrace {
		errs = append(errs, fmt.Errorf("PAYMENT_GRACE must be longer than PAYMENT_TIMEOUT (%s) and at most %s, got %s",
			c.Payment.Timeout, maxPaymentGrace, c.Payment.Grace))
	}
	if c.Reconciler.Interval < minReconcilerInterval || c.Reconciler.Interval > maxReconcilerInterval {
		errs = append(errs, fmt.Errorf("RECONCILER_INTERVAL must be between %s and %s, got %s",
			minReconcilerInterval, maxReconcilerInterval, c.Reconciler.Interval))
	}
	if !isKeyPrefix(c.Redis.KeyPrefix) {
		errs = append(errs, fmt.Errorf("REDIS_KEY_PREFIX must be 1 to %d letters, digits, or any of _ . - :, got %q",
			maxKeyPrefixSize, c.Redis.KeyPrefix))
	}
	if c.Booking.HoldClaimTTL <= c.DB.LockTimeout || c.Booking.HoldClaimTTL > maxHoldClaimTTL {
		errs = append(errs, fmt.Errorf("HOLD_CLAIM_TTL must be longer than DB_LOCK_TIMEOUT (%s) and at most %s, got %s",
			c.DB.LockTimeout, maxHoldClaimTTL, c.Booking.HoldClaimTTL))
	}
	cacheTTLs := map[string]time.Duration{
		"SEATMAP_CACHE_TTL":  c.Cache.SeatMapTTL,
		"SCHEDULE_CACHE_TTL": c.Cache.ScheduleTTL,
	}
	for name, ttl := range cacheTTLs {
		// Redis stores expiry times in milliseconds, so a positive TTL below 1ms would not expire at all.
		if ttl != 0 && (ttl < time.Millisecond || ttl > maxCacheTTL) {
			errs = append(errs, fmt.Errorf("%s must be 0 (off) or between 1ms and %s, got %s", name, maxCacheTTL, ttl))
		}
	}
	rateLimits := map[string]int{
		"BOOKING_RATE_LIMIT_PER_MIN":     c.RateLimit.BookingPerMin,
		"AUTH_IP_RATE_LIMIT_PER_MIN":     c.RateLimit.AuthIPPerMin,
		"LOGIN_EMAIL_RATE_LIMIT_PER_MIN": c.RateLimit.LoginEmailPerMin,
	}
	for name, n := range rateLimits {
		if n < 0 || n > maxRateLimit {
			errs = append(errs, fmt.Errorf("%s must be between 0 (off) and %d, got %d", name, maxRateLimit, n))
		}
	}
	listeners := map[string]string{
		"HTTP_ADDR":           c.HTTP.Addr,
		"METRICS_ADDR":        c.Metrics.Addr,
		"WORKER_METRICS_ADDR": c.Metrics.WorkerAddr,
	}
	for name, addr := range listeners {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			errs = append(errs, fmt.Errorf("%s must be host:port or :port, got %q", name, addr))
		}
	}
	if _, port, _ := net.SplitHostPort(c.HTTP.Addr); c.Metrics.Addr == c.HTTP.Addr && port != "0" {
		errs = append(errs, fmt.Errorf("METRICS_ADDR must differ from HTTP_ADDR (%s)", c.HTTP.Addr))
	}
	if !isCurrencyCode(c.Cinema.Currency) {
		errs = append(errs, fmt.Errorf("CINEMA_CURRENCY must be a 3-letter uppercase ISO 4217 code, got %q",
			c.Cinema.Currency))
	}

	return errors.Join(errs...)
}

// isKeyPrefix accepts Redis key prefixes without braces, which would change the hash slots of keys that rely on
// a hash tag, and without spaces or other characters that make keys awkward to inspect.
func isKeyPrefix(s string) bool {
	if s == "" || len(s) > maxKeyPrefixSize {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-', c == ':':
		default:
			return false
		}
	}
	return true
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}
