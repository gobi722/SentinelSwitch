// Package txnstatus looks up a transaction's fraud decision from the
// transactions table (written by Persistence Service once Fraud Engine has
// decided) for GetTransactionStatus.
package txnstatus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means no decided row exists for this (txn_id, client_id) pair —
// either the transaction is still being processed (Persistence Service only
// inserts a row once Fraud Engine has decided), the txn_id is unknown, or it
// belongs to a different client. These are deliberately indistinguishable to
// the caller: returning anything more specific for "belongs to a different
// client" would leak the existence of another tenant's transaction.
var ErrNotFound = errors.New("txnstatus: no decided row for this txn_id/client_id")

// Row is a decided transaction's fraud outcome.
type Row struct {
	Decision    string // APPROVED | DECLINED | REVIEW | UNKNOWN
	RiskScore   int32
	SubmittedAt time.Time // transactions.txn_timestamp — set at SubmitTransaction time
	DecidedAt   time.Time // transactions.processed_at — set by Fraud Engine
}

// Store looks up decided transactions, scoped to a single client_id.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pgxpool.Pool (the same one auth.Store uses —
// same database, different table).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Lookup returns the decided outcome for txnID, scoped to clientID so a
// caller can never see another client's transaction. Returns ErrNotFound if
// no matching decided row exists yet.
func (s *Store) Lookup(ctx context.Context, txnID, clientID string) (*Row, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(decision, 'UNKNOWN'), COALESCE(risk_score, 0), txn_timestamp, processed_at
		 FROM transactions
		 WHERE txn_id = $1 AND client_id = $2
		 ORDER BY txn_timestamp DESC
		 LIMIT 1`,
		txnID, clientID,
	)

	var r Row
	var decidedAt *time.Time
	if err := row.Scan(&r.Decision, &r.RiskScore, &r.SubmittedAt, &decidedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("txnstatus: lookup: %w", err)
	}
	if decidedAt != nil {
		r.DecidedAt = *decidedAt
	}
	return &r, nil
}
