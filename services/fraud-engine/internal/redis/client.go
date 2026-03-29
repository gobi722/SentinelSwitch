package redisclient

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sentinelswitch/fraud-engine/internal/config"
)

// DB index aliases – callers pass one of these constants to NewClient.
const (
	DBVelocity       = 0
	DBMerchantRisk   = 1
	DBCircuitBreaker = 2
)

// NewClient creates a redis.Client connected to the address and DB index
// resolved from cfg. The db parameter must be one of the DB* constants;
// the actual index is read from the config to honour redis.yaml overrides.
func NewClient(cfg *config.Config, db int) (*redis.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port)

	dbIndex := db
	switch db {
	case DBVelocity:
		dbIndex = cfg.Redis.VelocityDB
	case DBMerchantRisk:
		dbIndex = cfg.Redis.MerchantRiskDB
	case DBCircuitBreaker:
		dbIndex = cfg.Redis.CircuitBreakerDB
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     cfg.Redis.Password,
		DB:           dbIndex,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		PoolSize:     20,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis: ping db=%d addr=%s: %w", dbIndex, addr, err)
	}

	return rdb, nil
}
