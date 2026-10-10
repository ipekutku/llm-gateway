// Package postgres stores the gateway's persistent data in PostgreSQL:
// usage records, and the schema migrations that create their tables.
//
// With a TracerProvider, each batch insert and each migration run is a
// trace span. Spans carry counts, migration names, and errors, never SQL
// parameters or the connection string.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ipekutku/llm-gateway/internal/usage"
)

// scopeName is the instrumentation scope of the package's spans.
const scopeName = "github.com/ipekutku/llm-gateway/internal/postgres"

// Store is a PostgreSQL connection pool. It is safe for concurrent use.
type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

// Open connects to the database at url, a PostgreSQL URL or key/value
// connection string, and checks that it is reachable. Pool settings such as
// pool_max_conns can be given in url. Errors never include url, which may
// contain a password. Inserts and migrations create spans with tp; a nil tp
// creates none.
func Open(ctx context.Context, url string, tp trace.TracerProvider) (*Store, error) {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
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
	return &Store{pool: pool, tracer: tp.Tracer(scopeName)}, nil
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
//
// A non-empty batch is one client span, a child of the span in ctx if any.
// The recorder writes with its own context, so its batches start new
// traces; the span links to each record's sampled request span instead.
// It counts the records and the rows inserted, which is fewer when stored
// request IDs were skipped.
func (s *Store) Insert(ctx context.Context, records []usage.Record) error {
	if len(records) == 0 {
		return nil
	}
	var links []trace.Link
	for _, r := range records {
		if r.Span.IsSampled() {
			links = append(links, trace.Link{SpanContext: r.Span})
		}
	}
	ctx, span := s.tracer.Start(ctx, "INSERT usage_records",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithLinks(links...),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", "INSERT"),
			attribute.String("db.collection.name", "usage_records"),
			attribute.Int("gateway.usage.records", len(records)),
		))
	defer span.End()
	inserted, err := s.insert(ctx, records)
	if err != nil {
		setError(span, err)
		return err
	}
	span.SetAttributes(attribute.Int64("gateway.usage.inserted", inserted))
	return nil
}

// insert validates and sends records, returning the number of rows
// inserted.
func (s *Store) insert(ctx context.Context, records []usage.Record) (int64, error) {
	for i, r := range records {
		if err := r.Validate(); err != nil {
			return 0, fmt.Errorf("postgres: record %d: %w", i, err)
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
	results := s.pool.SendBatch(ctx, batch)
	var inserted int64
	for range records {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()
			return 0, fmt.Errorf("postgres: insert usage records: %w", err)
		}
		inserted += tag.RowsAffected()
	}
	if err := results.Close(); err != nil {
		return 0, fmt.Errorf("postgres: insert usage records: %w", err)
	}
	return inserted, nil
}

// setError marks span as failed with err's message, which never contains
// the connection string or SQL parameters, and an error.type: the SQLSTATE
// code for an error PostgreSQL reported, timeout, canceled, or _OTHER.
func setError(span trace.Span, err error) {
	typ := "_OTHER"
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		typ = pgErr.Code
	} else if errors.Is(err, context.DeadlineExceeded) {
		typ = "timeout"
	} else if errors.Is(err, context.Canceled) {
		typ = "canceled"
	}
	span.SetAttributes(attribute.String("error.type", typ))
	span.SetStatus(codes.Error, err.Error())
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
