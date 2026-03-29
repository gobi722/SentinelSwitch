package velocity

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	window1MinMs = int64(60 * 1000)
	window5MinMs = int64(5 * 60 * 1000)
	window1HrMs  = int64(60 * 60 * 1000)
)

// Result holds all velocity metrics computed for a single card+transaction.
type Result struct {
	TxnCount1Min        int32
	TxnCount5Min        int32
	TxnCount1Hr         int32
	AmountSum1Min       int64
	AmountSum1Hr        int64
	UniqueMerchants5Min int32
	UniqueTerminals5Min int32
}

// Store provides velocity checks backed by Redis sorted sets and HyperLogLogs.
type Store struct {
	rdb    *redis.Client
	logger *zap.Logger
}

// NewStore creates a new velocity Store.
func NewStore(rdb *redis.Client, logger *zap.Logger) *Store {
	return &Store{rdb: rdb, logger: logger}
}

// Record adds the current transaction to all velocity structures, prunes stale
// entries, and returns the updated velocity metrics for the card.
func (s *Store) Record(
	ctx context.Context,
	cardHash, txnID, merchantID, terminalID string,
	amountMinor int64,
) (*Result, error) {
	nowMs := time.Now().UnixMilli()

	// ── Write phase: pipeline all ZADDs, HFSETs, PFADDs ──────────────────────
	pipe := s.rdb.Pipeline()

	// Count rules
	type countRule struct {
		id     string
		window int64
		ttl    time.Duration
	}
	countRules := []countRule{
		{"TXN_COUNT_1MIN", window1MinMs, 2 * time.Minute},
		{"TXN_COUNT_5MIN", window5MinMs, 10 * time.Minute},
		{"TXN_COUNT_1HR", window1HrMs, 2 * time.Hour},
	}
	for _, cr := range countRules {
		k := fmt.Sprintf("velocity:count:%s:%s", cr.id, cardHash)
		cutoff := float64(nowMs - cr.window)
		pipe.ZRemRangeByScore(ctx, k, "-inf", strconv.FormatFloat(cutoff, 'f', 0, 64))
		pipe.ZAdd(ctx, k, redis.Z{Score: float64(nowMs), Member: txnID})
		pipe.Expire(ctx, k, cr.ttl)
	}

	// Amount rules
	type amtRule struct {
		id     string
		window int64
		ttl    time.Duration
	}
	amtRules := []amtRule{
		{"AMOUNT_SUM_1MIN", window1MinMs, 2 * time.Minute},
		{"AMOUNT_SUM_1HR", window1HrMs, 2 * time.Hour},
	}
	for _, ar := range amtRules {
		scoreKey := fmt.Sprintf("velocity:amount:%s:%s", ar.id, cardHash)
		valKey := fmt.Sprintf("velocity:amtval:%s:%s", ar.id, cardHash)
		cutoff := float64(nowMs - ar.window)
		pipe.ZRemRangeByScore(ctx, scoreKey, "-inf", strconv.FormatFloat(cutoff, 'f', 0, 64))
		pipe.ZAdd(ctx, scoreKey, redis.Z{Score: float64(nowMs), Member: txnID})
		pipe.Expire(ctx, scoreKey, ar.ttl)
		pipe.HSet(ctx, valKey, txnID, amountMinor)
		pipe.Expire(ctx, valKey, ar.ttl)
	}

	// HyperLogLog for unique merchants and terminals within the last 5 min bucket
	bucket := nowMs / window5MinMs
	merchantHLLKey := fmt.Sprintf("hll:merchants:%s:%d", cardHash, bucket)
	terminalHLLKey := fmt.Sprintf("hll:terminals:%s:%d", cardHash, bucket)
	pipe.PFAdd(ctx, merchantHLLKey, merchantID)
	pipe.Expire(ctx, merchantHLLKey, 10*time.Minute)
	pipe.PFAdd(ctx, terminalHLLKey, terminalID)
	pipe.Expire(ctx, terminalHLLKey, 10*time.Minute)

	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("velocity: pipeline exec: %w", err)
	}

	// ── Read phase: collect counts ─────────────────────────────────────────────
	result := &Result{}

	for i, cr := range countRules {
		k := fmt.Sprintf("velocity:count:%s:%s", cr.id, cardHash)
		cnt, err := s.rdb.ZCount(ctx, k, strconv.FormatInt(nowMs-cr.window, 10), "+inf").Result()
		if err != nil {
			s.logger.Warn("velocity: zcount failed", zap.String("rule", cr.id), zap.Error(err))
		}
		switch i {
		case 0:
			result.TxnCount1Min = int32(cnt)
		case 1:
			result.TxnCount5Min = int32(cnt)
		case 2:
			result.TxnCount1Hr = int32(cnt)
		}
	}

	for i, ar := range amtRules {
		scoreKey := fmt.Sprintf("velocity:amount:%s:%s", ar.id, cardHash)
		valKey := fmt.Sprintf("velocity:amtval:%s:%s", ar.id, cardHash)
		members, err := s.rdb.ZRangeByScore(ctx, scoreKey, &redis.ZRangeBy{
			Min: strconv.FormatInt(nowMs-ar.window, 10),
			Max: "+inf",
		}).Result()
		if err != nil {
			s.logger.Warn("velocity: zrangebyscore failed", zap.String("rule", ar.id), zap.Error(err))
			continue
		}
		var sum int64
		if len(members) > 0 {
			vals, err := s.rdb.HMGet(ctx, valKey, members...).Result()
			if err == nil {
				for _, v := range vals {
					if v == nil {
						continue
					}
					n, err := strconv.ParseInt(fmt.Sprintf("%v", v), 10, 64)
					if err == nil {
						sum += n
					}
				}
			}
		}
		switch i {
		case 0:
			result.AmountSum1Min = sum
		case 1:
			result.AmountSum1Hr = sum
		}
	}

	if cnt, err := s.rdb.PFCount(ctx, merchantHLLKey).Result(); err == nil {
		result.UniqueMerchants5Min = int32(cnt)
	}
	if cnt, err := s.rdb.PFCount(ctx, terminalHLLKey).Result(); err == nil {
		result.UniqueTerminals5Min = int32(cnt)
	}

	return result, nil
}
