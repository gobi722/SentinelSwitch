// Package idempotency provides a Redis-backed idempotency guard.
//
// Key format  : {prefix}:{idempotency_key}   (e.g. idempotency:uuid-v4)
// Value       : the original txn_id assigned when the request was first accepted
// TTL         : configurable (default 86400 s = 24 h)
//
// On first call   → SET NX EX succeeds → returns ("", false, nil)
// On repeat call  → SET NX EX fails    → GET returns original txn_id
//                   → returns (txn_id, true, nil)
//
// If Redis is unavailable:
//   - on_redis_unavailable: allow  → passes through (returns "", false, nil)
//   - on_redis_unavailable: reject → returns ("", false, err)
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is the Redis-backed idempotency store.
type Store struct {
	client    *redis.Client
	keyPrefix string
	ttl       time.Duration
	allowOnErr bool // true → pass through on Redis failure
}

// New creates a Store.
//   - addr     — Redis address host:port
//   - password — Redis AUTH password (empty = no auth)
//   - db       — Redis DB index (api-gateway.yaml: idempotency.redis_db)
//   - prefix   — key prefix (api-gateway.yaml: idempotency.key_prefix)
//   - ttl      — key TTL (api-gateway.yaml: idempotency.ttl_seconds)
//   - onUnavailable — "allow" or "reject"
func New(addr, password string, db int, prefix string, ttl int, onUnavailable string) *Store {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
	})
	return &Store{
		client:     client,
		keyPrefix:  prefix,
		ttl:        time.Duration(ttl) * time.Second,
		allowOnErr: onUnavailable != "reject",
	}
}

// Ping checks liveness of the Redis connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// CheckAndSet atomically checks for a duplicate and records the new txn_id.
//
//	Returns:
//	  originalTxnID — non-empty only when isDuplicate == true
//	  isDuplicate   — true if idempotency_key was already registered
//	  err           — non-nil only when Redis is unavailable AND allowOnErr==false
func (s *Store) CheckAndSet(ctx context.Context, idempotencyKey, txnID string) (originalTxnID string, isDuplicate bool, err error) {
	key := fmt.Sprintf("%s:%s", s.keyPrefix, idempotencyKey)

	// Attempt atomic SET NX EX
	set, err := s.client.SetNX(ctx, key, txnID, s.ttl).Result()
	if err != nil {
		if s.allowOnErr {
			return "", false, nil // degrade gracefully
		}
		return "", false, fmt.Errorf("idempotency redis: %w", err)
	}

	if set {
		// First occurrence — key was created.
		return "", false, nil
	}

	// Key already exists — fetch the original txn_id.
	original, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		// Race: key expired between SetNX and Get — treat as new.
		return "", false, nil
	}
	if err != nil {
		if s.allowOnErr {
			return "", false, nil
		}
		return "", false, fmt.Errorf("idempotency redis get: %w", err)
	}

	return original, true, nil
}

// Close releases the Redis connection.
func (s *Store) Close() error {
	return s.client.Close()
}
