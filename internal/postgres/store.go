// Package postgres stores the gateway's persistent data in PostgreSQL:
// usage records, and the schema migrations that create their tables.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipekutku/llm-gateway/internal/usage"
)

// Store is a PostgreSQL connection pool. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at url, a PostgreSQL URL or key/value
// connection string, and checks that it is reachable. Pool settings such as
// pool_max_conns can be given in url. Errors never include url, which may
// contain a password.
func Open(ctx context.Context, url string) (*Store, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("postgres: invalid database URL")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx redacts passwords from parse errors only on a best-effort
		// basis, so its message is not passed on.
		return nil, errors.New("postgres: invalid database URL")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes the pool, waiting for connections in use to be returned.
func (s *Store) Close() {
	s.pool.Close()
}

const insertRecord = `INSERT INTO usage_records (
	request_id, received_at, duration_ms, client_id, requested_model,
	provider, model, status, error_code,
	input_tokens, cache_read_input_tokens, cache_write_input_tokens, output_tokens,
	cost_usd
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (request_id) DO NOTHING`

// Insert stores records in one round trip and one transaction: all of them
// or none. A record whose request ID is already stored is skipped, so a
// batch can be retried after an uncertain failure without duplicates. If
// any record is invalid, nothing is sent.
func (s *Store) Insert(ctx context.Context, records []usage.Record) error {
	if len(records) == 0 {
		return nil
	}
	for i, r := range records {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("postgres: record %d: %w", i, err)
		}
	}

	batch := &pgx.Batch{}
	for _, r := range records {
		var input, cacheRead, cacheWrite, output *int
		if u := r.Usage; u != nil {
			input, cacheRead, cacheWrite, output = &u.InputTokens, &u.CacheReadInputTokens, &u.CacheWriteInputTokens, &u.OutputTokens
		}
		batch.Queue(insertRecord,
			r.RequestID, r.Time, r.Duration.Milliseconds(), r.ClientID, r.RequestedModel,
			nullIfEmpty(r.Provider), nullIfEmpty(r.Model), r.Status, nullIfEmpty(r.ErrorCode),
			input, cacheRead, cacheWrite, output,
			costUSD(r.Cost),
		)
	}
	// pgx sends a batch with a single sync, so PostgreSQL runs it as one
	// implicit transaction.
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("postgres: insert usage records: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// costUSD converts a cost in picodollars to an exact numeric in dollars.
func costUSD(c *usage.Cost) pgtype.Numeric {
	if c == nil {
		return pgtype.Numeric{}
	}
	return pgtype.Numeric{Int: big.NewInt(int64(*c)), Exp: -12, Valid: true}
}
