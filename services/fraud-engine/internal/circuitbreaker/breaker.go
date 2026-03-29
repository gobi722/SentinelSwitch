package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sentinelswitch/fraud-engine/internal/config"
	"go.uber.org/zap"
)

// State represents the circuit breaker state stored in Redis.
type State string

const (
	StateClosed   State = "CLOSED"
	StateOpen     State = "OPEN"
	StateHalfOpen State = "HALF_OPEN"
)

// ErrCircuitOpen is returned by Allow when the circuit is OPEN.
var ErrCircuitOpen = errors.New("circuit breaker: circuit is open")

// Breaker is a Redis-backed circuit breaker for the Risk Service gRPC calls.
type Breaker struct {
	rdb    *redis.Client
	cfg    config.CircuitBreakerCfg
	logger *zap.Logger
}

// New creates a new Breaker.
func New(rdb *redis.Client, cfg *config.Config, logger *zap.Logger) *Breaker {
	return &Breaker{
		rdb:    rdb,
		cfg:    cfg.RiskService.CircuitBreaker,
		logger: logger,
	}
}

// Allow returns nil if the call is permitted, ErrCircuitOpen if blocked.
// On Redis error, fails open to avoid blocking all traffic.
func (b *Breaker) Allow(ctx context.Context) error {
	state, err := b.getState(ctx)
	if err != nil {
		b.logger.Warn("circuit breaker: state read failed, failing open", zap.Error(err))
		return nil
	}
	if state == StateOpen {
		return ErrCircuitOpen
	}
	return nil
}

// RecordSuccess is called after a successful Risk Service call.
// In HALF_OPEN state, once successThreshold consecutive successes are
// recorded, the circuit transitions back to CLOSED.
func (b *Breaker) RecordSuccess(ctx context.Context) {
	state, err := b.getState(ctx)
	if err != nil {
		return
	}
	if state != StateHalfOpen {
		return
	}

	successKey := b.cfg.FailuresKey + ":success"
	count, err := b.rdb.Incr(ctx, successKey).Result()
	if err != nil {
		b.logger.Warn("circuit breaker: incr success counter failed", zap.Error(err))
		return
	}
	b.rdb.Expire(ctx, successKey, time.Duration(b.cfg.StateTTLSeconds)*time.Second) //nolint:errcheck

	if int(count) >= b.cfg.SuccessThreshold {
		b.setState(ctx, StateClosed)
		b.rdb.Del(ctx, b.cfg.FailuresKey, successKey) //nolint:errcheck
		b.logger.Info("circuit breaker: transitioned HALF_OPEN → CLOSED")
	}
}

// RecordFailure is called after a failed Risk Service call.
// When the failure count reaches the threshold, the circuit opens.
func (b *Breaker) RecordFailure(ctx context.Context) {
	count, err := b.rdb.Incr(ctx, b.cfg.FailuresKey).Result()
	if err != nil {
		b.logger.Warn("circuit breaker: incr failures failed", zap.Error(err))
		return
	}
	b.rdb.Expire(ctx, b.cfg.FailuresKey,
		time.Duration(b.cfg.FailuresTTLSeconds)*time.Second) //nolint:errcheck

	if int(count) >= b.cfg.FailureThreshold {
		openDuration := time.Duration(b.cfg.OpenDurationMs) * time.Millisecond
		b.setStateWithTTL(ctx, StateOpen, openDuration)
		b.logger.Warn("circuit breaker: transitioned CLOSED → OPEN",
			zap.Int64("failure_count", count),
			zap.Int("threshold", b.cfg.FailureThreshold),
			zap.Duration("open_duration", openDuration),
		)
	}
}

// FallbackRiskScore returns the score to use when the circuit is open.
func (b *Breaker) FallbackRiskScore() int32 {
	return b.cfg.FallbackRiskScore
}

// getState reads the current circuit state from Redis.
// If the state key has expired (OPEN → HALF_OPEN transition via TTL), returns HALF_OPEN.
func (b *Breaker) getState(ctx context.Context) (State, error) {
	val, err := b.rdb.Get(ctx, b.cfg.StateKey).Result()
	if errors.Is(err, redis.Nil) {
		// Key expired — treat as HALF_OPEN (probe phase).
		return StateHalfOpen, nil
	}
	if err != nil {
		return StateClosed, fmt.Errorf("circuit breaker: get state: %w", err)
	}
	return State(val), nil
}

func (b *Breaker) setState(ctx context.Context, state State) {
	b.setStateWithTTL(ctx, state, time.Duration(b.cfg.StateTTLSeconds)*time.Second)
}

func (b *Breaker) setStateWithTTL(ctx context.Context, state State, ttl time.Duration) {
	if err := b.rdb.Set(ctx, b.cfg.StateKey, string(state), ttl).Err(); err != nil {
		b.logger.Warn("circuit breaker: set state failed",
			zap.String("state", string(state)), zap.Error(err))
	}
	if state == StateClosed {
		b.rdb.Set(ctx, b.cfg.FailuresKey, strconv.Itoa(0),
			time.Duration(b.cfg.FailuresTTLSeconds)*time.Second) //nolint:errcheck
	}
}
