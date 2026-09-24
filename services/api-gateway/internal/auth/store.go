package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// postgresUniqueViolation is the SQLSTATE Postgres returns for a primary
// key / unique constraint conflict.
const postgresUniqueViolation = "23505"

// ErrNotFound is returned when no client matches the given API key hash.
var ErrNotFound = errors.New("auth: api key not found")

// ErrClientExists is returned by InsertClient when client_id is already taken.
var ErrClientExists = errors.New("auth: client_id already provisioned")

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

// InsertClient creates a new row in api_clients (status: active). Returns
// ErrClientExists if clientID is already taken — client_id is the table's
// primary key, so this is a single atomic INSERT, no separate existence
// check needed.
func (s *Store) InsertClient(ctx context.Context, clientID, apiKeyHash, apiKeyPrefix, name string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_clients (client_id, api_key_hash, api_key_prefix, name, status)
		 VALUES ($1, $2, $3, $4, 'active')`,
		clientID, apiKeyHash, apiKeyPrefix, name,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == postgresUniqueViolation {
			return ErrClientExists
		}
		return fmt.Errorf("auth: insert client: %w", err)
	}
	return nil
}

// Ping verifies the pool can reach Postgres.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases all connections in the pool.
func (s *Store) Close() {
	s.pool.Close()
}
