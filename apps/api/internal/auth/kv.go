package auth

// The key/value surface this package needs, and the Redis implementation of it.
//
// An interface rather than *redis.Client passed around, for the same reason
// internal/api takes a Pinger rather than a pool: the session store and the
// rate limiter are the interesting code, and neither of them should be
// untestable without a running Redis. The integration suite exercises the real
// implementation against a real container; the unit tests exercise the logic
// against an in-memory one.
//
// The surface is deliberately four methods. Anything that needs a fifth is
// probably reaching for Redis as a database rather than as the ephemeral store
// it is here.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrKeyNotFound is what Get returns when the key is absent or expired.
//
// Redis does not distinguish the two and neither does this: a refresh token
// that has expired and one that never existed are the same answer, which is the
// answer a client should get for both.
var ErrKeyNotFound = errors.New("auth: key not found")

// KeyValue is the ephemeral store behind refresh tokens and rate limits.
type KeyValue interface {
	// Get returns the value at key, or ErrKeyNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set writes value at key with a time to live. A non-positive ttl is a
	// programming error rather than "forever": everything this package stores
	// is meant to expire on its own, so a key with no expiry is a leak.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes keys, ignoring any that are already gone.
	Delete(ctx context.Context, keys ...string) error

	// Increment adds one to the counter at key, setting the ttl if the key is
	// new, and reports the new count together with the time left in the
	// window. The window is fixed rather than sliding: the ttl is set once,
	// when the counter is created, so a client hammering the endpoint cannot
	// extend its own lockout indefinitely — or, more importantly, cannot have
	// the window silently restart under it.
	Increment(ctx context.Context, key string, ttl time.Duration) (count int64, remaining time.Duration, err error)
}

// RedisKeyValue is the production implementation.
type RedisKeyValue struct {
	client redis.UniversalClient
}

// NewRedisKeyValue adapts a go-redis client to [KeyValue].
func NewRedisKeyValue(client redis.UniversalClient) *RedisKeyValue {
	return &RedisKeyValue{client: client}
}

// Get implements [KeyValue].
func (r *RedisKeyValue) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrKeyNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	return value, nil
}

// Set implements [KeyValue].
func (r *RedisKeyValue) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("auth: refusing to write %s with a non-positive ttl", key)
	}

	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}

	return nil
}

// Delete implements [KeyValue].
func (r *RedisKeyValue) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}

	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("deleting %d key(s): %w", len(keys), err)
	}

	return nil
}

// Increment implements [KeyValue].
//
// INCR and EXPIRE go out in one pipeline, so the two commands cost one round
// trip. EXPIRE carries the NX flag, which is what makes the window fixed:
// without it every request would push the expiry out and a busy attacker's
// window would never close.
func (r *RedisKeyValue) Increment(ctx context.Context, key string, ttl time.Duration) (int64, time.Duration, error) {
	if ttl <= 0 {
		return 0, 0, fmt.Errorf("auth: refusing to count %s with a non-positive ttl", key)
	}

	pipe := r.client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, ttl)
	remaining := pipe.PTTL(ctx, key)

	if _, err := pipe.Exec(ctx); err != nil {
		return 0, 0, fmt.Errorf("incrementing %s: %w", key, err)
	}

	left := remaining.Val()
	if left < 0 {
		// -1 is "no expiry", -2 is "no key". Neither should happen inside a
		// transaction that just created or touched the key, but reporting a
		// negative duration to a caller that will put it in Retry-After would
		// be worse than assuming a full window.
		left = ttl
	}

	return incr.Val(), left, nil
}
