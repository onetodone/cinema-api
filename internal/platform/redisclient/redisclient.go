// Package redisclient creates the Redis client.
package redisclient

import (
	"github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/config"
)

// New returns a Redis client for cfg. It does not connect eagerly: Redis is an optional accelerator,
// so callers decide how to react when it is unreachable (the API keeps serving in fail-open mode).
func New(cfg config.RedisConfig, clientName string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:          cfg.Addr,
		Password:      cfg.Password,
		DB:            cfg.DB,
		DialTimeout:   cfg.DialTimeout,
		DialerRetries: cfg.DialAttempts, // go-redis counts total attempts here, not retries
		ReadTimeout:   cfg.ReadTimeout,
		WriteTimeout:  cfg.WriteTimeout,
		MaxRetries:    cfg.MaxRetries,
		ClientName:    clientName,
	})
}
