package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	fraudpb "github.com/sentinelswitch/proto/fraud/v1"
	"go.uber.org/zap"
)

const upsertSQL = `
INSERT INTO transactions (
    txn_id, card_hash, masked_pan, amount_minor, currency,
    merchant_id, terminal_id, mcc, transaction_type, channel, scheme, rrn,
    txn_timestamp, status, risk_score, triggered_rules, decision, processed_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (txn_id, txn_timestamp) DO UPDATE SET
    status          = EXCLUDED.status,
    risk_score      = EXCLUDED.risk_score,
    triggered_rules = EXCLUDED.triggered_rules,
    decision        = EXCLUDED.decision,
    processed_at    = EXCLUDED.processed_at,
    updated_at      = NOW()`

// Store wraps a pgxpool.Pool and provides batch upsert operations.
type Store struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New creates a Store by connecting to PostgreSQL using the supplied pool config.
func New(ctx context.Context, poolCfg *pgxpool.Config, log *zap.Logger) (*Store, error) {
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect pool: %w", err)
	}
	return &Store{pool: pool, log: log}, nil
}

// Close releases all connections in the pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping verifies the pool can reach the database.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// UpsertBatch upserts a slice of FraudResultEvents into the transactions table
// using a single pgx Batch to minimise round-trips.
func (s *Store) UpsertBatch(ctx context.Context, events []*fraudpb.FraudResultEvent) error {
	if len(events) == 0 {
		return nil
	}

	batch := &pgx.Batch{}

	for _, ev := range events {
		txnTimestamp, err := time.Parse(time.RFC3339, ev.TxnTimestamp)
		if err != nil {
			s.log.Warn("failed to parse txn_timestamp — using zero time",
				zap.String("txn_id", ev.TxnId),
				zap.String("raw", ev.TxnTimestamp),
				zap.Error(err),
			)
			txnTimestamp = time.Time{}
		}

		processedAt, err := time.Parse(time.RFC3339, ev.ProcessedAt)
		if err != nil {
			s.log.Warn("failed to parse processed_at — using now",
				zap.String("txn_id", ev.TxnId),
				zap.String("raw", ev.ProcessedAt),
				zap.Error(err),
			)
			processedAt = time.Now().UTC()
		}

		triggeredRulesJSON, err := json.Marshal(ev.TriggeredRules)
		if err != nil {
			triggeredRulesJSON = []byte("[]")
		}

		decisionStr := mapDecision(ev.Decision)

		batch.Queue(
			upsertSQL,
			ev.TxnId,
			ev.CardHash,
			ev.MaskedPan,
			ev.AmountMinor,
			ev.Currency,
			ev.MerchantId,
			ev.TerminalId,
			ev.Mcc,
			mapTransactionType(ev.TransactionType),
			mapChannel(ev.Channel),
			mapScheme(ev.Scheme),
			ev.Rrn,
			txnTimestamp,
			decisionStr, // status mirrors decision
			ev.RiskScore,
			string(triggeredRulesJSON),
			decisionStr,
			processedAt,
		)
	}

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close() //nolint:errcheck

	for i := range events {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("store: upsert row %d (txn_id=%s): %w", i, events[i].TxnId, err)
		}
	}

	return nil
}

func mapDecision(d fraudpb.Decision) string {
	switch d {
	case fraudpb.Decision_APPROVE:
		return "APPROVED"
	case fraudpb.Decision_DECLINE:
		return "DECLINED"
	case fraudpb.Decision_REVIEW:
		return "REVIEW"
	default:
		return "UNKNOWN"
	}
}

func mapTransactionType(t fraudpb.TransactionType) string {
	switch t {
	case fraudpb.TransactionType_SALES:
		return "sales"
	case fraudpb.TransactionType_VOID:
		return "void"
	case fraudpb.TransactionType_REFUND:
		return "refund"
	case fraudpb.TransactionType_REVERSAL:
		return "reversal"
	default:
		return "unknown"
	}
}

func mapChannel(c fraudpb.Channel) string {
	switch c {
	case fraudpb.Channel_POS:
		return "pos"
	case fraudpb.Channel_PG:
		return "pg"
	default:
		return "unknown"
	}
}

func mapScheme(sc fraudpb.Scheme) string {
	switch sc {
	case fraudpb.Scheme_MASTERCARD:
		return "mastercard"
	case fraudpb.Scheme_VISA:
		return "visa"
	case fraudpb.Scheme_AMEX:
		return "amex"
	case fraudpb.Scheme_UNION_PAY:
		return "union_pay"
	default:
		return "unknown"
	}
}
