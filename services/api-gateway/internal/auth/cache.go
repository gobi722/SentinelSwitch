package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// negativeMarker is cached in place of a client_id when a key is known not
// to exist, to avoid hammering Postgres with repeated invalid-key lookups.
const negativeMarker = "\x00NOTFOUND"

// Cache is a Redis-backed read-through cache in front of Store.
//
// Unlike idempotency.Store, this cache fails CLOSED on the source of truth:
// if Postgres cannot answer, the error propagates and the caller MUST deny
// the request — it is never treated as "no client found" or "allow through".
// Redis itself is treated as a pure optimization: if the cache is unreachable
// on read, or a write-back fails, lookups fall through to (or simply don't
// cache) the authoritative Postgres answer rather than failing the request.
type Cache struct {
	redis *redis.Client
	store *Store
	ttl   time.Duration
}

// NewCache constructs a Cache.
//   - addr, password, db — Redis connection details (dedicated DB index)
//   - ttlSeconds         — how long positive/negative lookups are cached
func NewCache(store *Store, addr, password string, db int, ttlSeconds int) *Cache {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
	})
	return &Cache{
		redis: client,
		store: store,
		ttl:   time.Duration(ttlSeconds) * time.Second,
	}
}

// Lookup resolves an API key hash to a verified, active client_id.
//
// Returns ErrNotFound if the key doesn't exist or belongs to a suspended
// client. Returns any other error (Postgres unavailable) as-is — callers
// MUST treat a non-nil, non-ErrNotFound error as "deny the request".
func (c *Cache) Lookup(ctx context.Context, apiKeyHash string) (clientID string, err error) {
	key := "apikey:" + apiKeyHash

	if cached, err := c.redis.Get(ctx, key).Result(); err == nil {
		if cached == negativeMarker {
			return "", ErrNotFound
		}
		// cached value format: "<client_id>|<status>"
		if parts := strings.SplitN(cached, "|", 2); len(parts) == 2 && parts[1] == "active" {
			return parts[0], nil
		}
		return "", ErrNotFound
	}
	// Any Redis error (miss or connection failure) falls through to the
	// authoritative store — the cache is an optimization, not a dependency.

	clientID, status, err := c.store.Lookup(ctx, apiKeyHash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.trySet(ctx, key, negativeMarker)
			return "", ErrNotFound
		}
		return "", fmt.Errorf("auth: store unavailable: %w", err)
	}

	if status != "active" {
		c.trySet(ctx, key, clientID+"|"+status)
		return "", ErrNotFound
	}

	c.trySet(ctx, key, clientID+"|"+status)
	return clientID, nil
}

// trySet writes a best-effort cache entry. A failed write never fails an
// otherwise-successful, authoritative lookup — it just means the next
// request pays the Postgres round-trip again.
func (c *Cache) trySet(ctx context.Context, key, value string) {
	_ = c.redis.Set(ctx, key, value, c.ttl).Err()
}

// Ping verifies the Redis connection.
func (c *Cache) Ping(ctx context.Context) error {
	return c.redis.Ping(ctx).Err()
}

// Close releases the Redis connection.
func (c *Cache) Close() error {
	return c.redis.Close()
}
