package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationFiles holds the migrations directory. Replaced in tests.
var migrationFiles fs.FS = embeddedMigrations

// migrationLockID is the key of the advisory lock that serializes
// migrations, so gateways migrating at the same time apply each migration
// once. It is an arbitrary constant.
const migrationLockID = 0x6c6c6d2d67617465 // "llm-gate"

type migration struct {
	version int
	name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// loadMigrations returns the embedded migrations in version order. File
// names are NNNN_description.sql, numbered from 0001 without gaps.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %q is not named NNNN_description.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if version != len(out)+1 {
			return nil, fmt.Errorf("migration %q: want version %04d", e.Name(), len(out)+1)
		}
		sql, err := fs.ReadFile(fsys, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(sql)})
	}
	return out, nil
}

// Migrate applies the migrations the database has not applied yet and
// returns how many it applied. Migrations are forward-only.
//
// All pending migrations run in one transaction under an advisory lock, so
// a failure leaves the schema unchanged and concurrent calls apply each
// migration once. A migration therefore must not use statements that
// cannot run in a transaction, such as CREATE INDEX CONCURRENTLY.
//
// It fails without changes if the database has a migration this gateway
// does not know, which means a newer gateway migrated it.
//
// The run is a "migrate" span with one child span per migration applied.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	ctx, span := s.tracer.Start(ctx, "migrate", trace.WithAttributes(attribute.String("db.system.name", "postgresql")))
	defer span.End()
	n, err := s.migrate(ctx)
	if err != nil {
		setError(span, err)
		return 0, err
	}
	span.SetAttributes(attribute.Int("gateway.migrations.applied", n))
	return n, nil
}

func (s *Store) migrate(ctx context.Context) (int, error) {
	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLockID)); err != nil {
		return 0, fmt.Errorf("migrate: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return 0, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}
	if err := checkKnown(applied, migrations); err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}

	n := 0
	for _, m := range migrations {
		if m.version <= len(applied) {
			continue
		}
		if err := s.apply(ctx, tx, m); err != nil {
			return 0, err
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("migrate: commit: %w", err)
	}
	return n, nil
}

// apply runs migration m in tx and records it, in a span of its own. The
// span succeeds even if the transaction is later rolled back; the migrate
// span then fails.
func (s *Store) apply(ctx context.Context, tx pgx.Tx, m migration) error {
	ctx, span := s.tracer.Start(ctx, "migration "+m.name, trace.WithAttributes(
		attribute.String("db.system.name", "postgresql"),
		attribute.Int("gateway.migration.version", m.version),
	))
	defer span.End()
	var err error
	if _, execErr := tx.Exec(ctx, m.sql); execErr != nil {
		err = fmt.Errorf("migrate: %s: %w", m.name, execErr)
	} else if _, execErr := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name); execErr != nil {
		err = fmt.Errorf("migrate: record %s: %w", m.name, execErr)
	}
	if err != nil {
		setError(span, err)
	}
	return err
}

// CheckSchema reports an error unless the database has applied exactly the
// migrations this gateway knows, so a gateway never writes to a schema it
// was not built for.
func (s *Store) CheckSchema(ctx context.Context) error {
	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if !exists {
		return fmt.Errorf("database is not migrated; run the migrations first")
	}
	applied, err := appliedVersions(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if err := checkKnown(applied, migrations); err != nil {
		return err
	}
	if len(applied) < len(migrations) {
		return fmt.Errorf("database schema is at version %d, want %d; run the migrations first", len(applied), len(migrations))
	}
	return nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type appliedMigration struct {
	Version int
	Name    string
}

// appliedVersions returns the recorded migrations in version order.
func appliedVersions(ctx context.Context, q querier) ([]appliedMigration, error) {
	rows, err := q.Query(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[appliedMigration])
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return versions, nil
}

// checkKnown requires applied migrations to be an exact prefix of the
// embedded files. A gap or changed file name must not be silently repaired.
func checkKnown(applied []appliedMigration, migrations []migration) error {
	for i, m := range applied {
		if m.Version > len(migrations) {
			return fmt.Errorf("database has migration %d, which this gateway does not know; it was migrated by a newer version", m.Version)
		}
		if m.Version != i+1 || i >= len(migrations) || m.Name != migrations[i].name {
			return fmt.Errorf("database migration %d does not match the embedded migration history", m.Version)
		}
	}
	return nil
}
