package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no client matches the given API key hash.
var ErrNotFound = errors.New("auth: api key not found")

// Store looks up clients by API key hash against the api_clients table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pgxpool.Pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Lookup returns the client_id and status for a given API key hash.
// Returns ErrNotFound if no row matches.
func (s *Store) Lookup(ctx context.Context, apiKeyHash string) (clientID, status string, err error) {
	row := s.pool.QueryRow(ctx,
		`SELECT client_id, status FROM api_clients WHERE api_key_hash = $1`,
		apiKeyHash,
	)
	if err := row.Scan(&clientID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("auth: store lookup: %w", err)
	}
	return clientID, status, nil
}

// Ping verifies the pool can reach Postgres.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases all connections in the pool.
func (s *Store) Close() {
	s.pool.Close()
}
