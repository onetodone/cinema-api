// Package config loads and validates the application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the complete runtime configuration shared by all binaries.
type Config struct {
	HTTP  HTTPConfig
	DB    DBConfig
	Redis RedisConfig
	Log   LogConfig
}

// HTTPConfig configures the HTTP server.
type HTTPConfig struct {
	Addr              string        `env:"HTTP_ADDR"                envDefault:":8080"`
	ReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"5s"`
	ReadTimeout       time.Duration `env:"HTTP_READ_TIMEOUT"        envDefault:"10s"`
	WriteTimeout      time.Duration `env:"HTTP_WRITE_TIMEOUT"       envDefault:"15s"`
	IdleTimeout       time.Duration `env:"HTTP_IDLE_TIMEOUT"        envDefault:"60s"`
	ShutdownTimeout   time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT"    envDefault:"15s"`
}

// DBConfig configures the PostgreSQL connection pool.
type DBConfig struct {
	URL             string        `env:"DATABASE_URL,required,notEmpty"`
	MaxConns        int32         `env:"DB_MAX_CONNS"          envDefault:"20"`
	MinConns        int32         `env:"DB_MIN_CONNS"          envDefault:"2"`
	MaxConnLifetime time.Duration `env:"DB_MAX_CONN_LIFETIME"  envDefault:"30m"`
	MaxConnIdleTime time.Duration `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"5m"`
	ConnectTimeout  time.Duration `env:"DB_CONNECT_TIMEOUT"    envDefault:"5s"`
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

	return errors.Join(errs...)
}
