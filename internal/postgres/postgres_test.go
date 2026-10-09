package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

// testDatabaseURLVar names a PostgreSQL URL for the integration tests, such
// as the one printed by make db. Each test creates and drops its own
// database there, so the user needs the CREATEDB privilege.
const testDatabaseURLVar = "GATEWAY_TEST_DATABASE_URL"

// guard bounds every database call in the tests.
const guard = 30 * time.Second

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), guard)
	t.Cleanup(cancel)
	return ctx
}

// newDatabase creates an empty database for one test, drops it when the
// test ends, and returns its URL. Without testDatabaseURLVar the test is
// skipped, except in CI, where a missing database must not pass silently.
func newDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv(testDatabaseURLVar)
	if base == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set; CI must run the PostgreSQL integration tests", testDatabaseURLVar)
		}
		t.Skipf("%s is not set; run make db to enable the PostgreSQL integration tests", testDatabaseURLVar)
	}
	ctx := testContext(t)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", testDatabaseURLVar, err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	name := "gateway_test_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s must be a URL: %v", testDatabaseURLVar, err)
	}
	u.Path = "/" + name
	return u.String()
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(testContext(t), newDatabase(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func migratedStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	if _, err := s.Migrate(testContext(t)); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return s
}

// useMigrations replaces the embedded migrations for one test.
func useMigrations(t *testing.T, files map[string]string) {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, sql := range files {
		fsys["migrations/"+name] = &fstest.MapFile{Data: []byte(sql)}
	}
	old := migrationFiles
	migrationFiles = fsys
	t.Cleanup(func() { migrationFiles = old })
}

func TestLoadEmbeddedMigrations(t *testing.T) {
	migrations, err := loadMigrations(embeddedMigrations)
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(migrations) == 0 || migrations[0].name != "0001_create_usage_records.sql" {
		t.Errorf("migrations = %+v, want 0001_create_usage_records.sql first", migrations)
	}
}

func TestLoadMigrationsRejectsBadNames(t *testing.T) {
	tests := map[string][]string{
		"gap":              {"0001_a.sql", "0003_c.sql"},
		"not from 0001":    {"0002_b.sql"},
		"duplicate number": {"0001_a.sql", "0001_b.sql"},
		"short number":     {"001_a.sql"},
		"no description":   {"0001.sql"},
		"wrong extension":  {"0001_a.txt"},
		"upper case":       {"0001_Add.sql"},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for _, f := range files {
				fsys["migrations/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
			}
			if _, err := loadMigrations(fsys); err == nil {
				t.Errorf("loadMigrations(%v) error = nil, want error", files)
			}
		})
	}
}

func TestOpenDoesNotRevealURL(t *testing.T) {
	const password = "s3cret-password"
	tests := map[string]string{
		"empty":      " ",
		"unparsable": "postgres://gateway:" + password + "@127.0.0.1:notaport/gateway",
		// Port 1 refuses connections, so Open fails at the ping.
		"unreachable": "postgres://gateway:" + password + "@127.0.0.1:1/gateway?connect_timeout=5",
	}
	for name, u := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := Open(testContext(t), u)
			if err == nil {
				s.Close()
				t.Fatal("Open() error = nil, want error")
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("error reveals the password: %v", err)
			}
		})
	}
}

func TestMigrate(t *testing.T) {
	s := newStore(t)
	ctx := testContext(t)

	if err := s.CheckSchema(ctx); err == nil {
		t.Error("CheckSchema() on an empty database = nil, want error")
	}

	n, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if n != 1 {
		t.Errorf("Migrate() applied %d migrations, want 1", n)
	}
	if err := s.CheckSchema(ctx); err != nil {
		t.Errorf("CheckSchema() after Migrate = %v, want nil", err)
	}

	n, err = s.Migrate(ctx)
	if err != nil || n != 0 {
		t.Errorf("second Migrate() = %d, %v; want 0, nil", n, err)
	}
}

func TestConcurrentMigrateAppliesOnce(t *testing.T) {
	s := newStore(t)
	ctx := testContext(t)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for range 5 {
		wg.Go(func() {
			n, err := s.Migrate(ctx)
			if err != nil {
				t.Errorf("Migrate() error = %v", err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		})
	}
	wg.Wait()
	if total != 1 {
		t.Errorf("concurrent Migrate() calls applied %d migrations in total, want 1", total)
	}
}

func TestMigrateIsAtomic(t *testing.T) {
	useMigrations(t, map[string]string{
		"0001_good.sql":   "CREATE TABLE first (id integer);",
		"0002_broken.sql": "CREATE TABLE second (id integer); SELEC 1;",
	})
	s := newStore(t)
	ctx := testContext(t)

	if _, err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "0002_broken.sql") {
		t.Fatalf("Migrate() error = %v, want an error naming 0002_broken.sql", err)
	}
	var tables int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_tables WHERE tablename IN ('first', 'second', 'schema_migrations')").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("a failed Migrate() left %d tables behind, want none", tables)
	}
}

func TestMigrateAppliesOnlyPendingMigrations(t *testing.T) {
	useMigrations(t, map[string]string{"0001_first.sql": "CREATE TABLE first (id integer);"})
	s := newStore(t)
	ctx := testContext(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	// A newer gateway adds a migration; the old schema is now behind.
	useMigrations(t, map[string]string{
		"0001_first.sql":  "CREATE TABLE first (id integer);",
		"0002_second.sql": "CREATE TABLE second (id integer);",
	})
	if err := s.CheckSchema(ctx); err == nil || !strings.Contains(err.Error(), "version 1, want 2") {
		t.Errorf("CheckSchema() = %v, want an error about version 1 of 2", err)
	}
	n, err := s.Migrate(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Migrate() = %d, %v; want 1, nil", n, err)
	}
	if err := s.CheckSchema(ctx); err != nil {
		t.Errorf("CheckSchema() = %v, want nil", err)
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	s := migratedStore(t)
	ctx := testContext(t)
	if _, err := s.pool.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES (999, '0999_future.sql')"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("Migrate() = %v, want an error about a newer schema", err)
	}
	if err := s.CheckSchema(ctx); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("CheckSchema() = %v, want an error about a newer schema", err)
	}
}

func TestMigrateRejectsChangedHistory(t *testing.T) {
	t.Run("renamed migration", func(t *testing.T) {
		s := migratedStore(t)
		ctx := testContext(t)
		if _, err := s.pool.Exec(ctx, "UPDATE schema_migrations SET name = '0001_other.sql' WHERE version = 1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("Migrate() = %v, want a history mismatch", err)
		}
		if err := s.CheckSchema(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("CheckSchema() = %v, want a history mismatch", err)
		}
	})

	t.Run("missing first migration", func(t *testing.T) {
		useMigrations(t, map[string]string{
			"0001_first.sql":  "CREATE TABLE first (id integer);",
			"0002_second.sql": "CREATE TABLE second (id integer);",
		})
		s := newStore(t)
		ctx := testContext(t)
		if _, err := s.pool.Exec(ctx, `CREATE TABLE schema_migrations (
			version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES (2, '0002_second.sql')"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("Migrate() = %v, want a history mismatch", err)
		}
		if err := s.CheckSchema(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("CheckSchema() = %v, want a history mismatch", err)
		}
	})
}

// received is a fixed arrival time with microsecond precision, which
// PostgreSQL stores exactly.
var received = time.Date(2026, 10, 9, 12, 30, 0, 123456000, time.UTC)

func successRecord(id string) usage.Record {
	cost := usage.Cost(123_456_789_012) // $0.123456789012
	return usage.Record{
		RequestID:      id,
		Time:           received,
		Duration:       1500 * time.Millisecond,
		ClientID:       "team-a",
		RequestedModel: "gpt-4o",
		Provider:       "openai",
		Model:          "gpt-4o-2024-08-06",
		Status:         200,
		Usage:          &llm.Usage{InputTokens: 2006, CacheReadInputTokens: 1920, OutputTokens: 300},
		Cost:           &cost,
	}
}

func failureRecord(id string) usage.Record {
	return usage.Record{
		RequestID:      id,
		Time:           received,
		Duration:       20 * time.Millisecond,
		ClientID:       "team-b",
		RequestedModel: "claude-opus-5-5",
		Status:         404,
		ErrorCode:      "model_not_found",
	}
}

type storedRecord struct {
	RequestID, ClientID, RequestedModel  string
	ReceivedAt                           time.Time
	DurationMS, Status                   int
	Provider, Model, ErrorCode           *string
	Input, CacheRead, CacheWrite, Output *int
	CostUSD                              *string
}

func readRecord(t *testing.T, s *Store, id string) storedRecord {
	t.Helper()
	var r storedRecord
	err := s.pool.QueryRow(testContext(t), `SELECT request_id, client_id, requested_model, received_at, duration_ms, status,
		provider, model, error_code,
		input_tokens, cache_read_input_tokens, cache_write_input_tokens, output_tokens, cost_usd::text
		FROM usage_records WHERE request_id = $1`, id).Scan(
		&r.RequestID, &r.ClientID, &r.RequestedModel, &r.ReceivedAt, &r.DurationMS, &r.Status,
		&r.Provider, &r.Model, &r.ErrorCode,
		&r.Input, &r.CacheRead, &r.CacheWrite, &r.Output, &r.CostUSD)
	if err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	return r
}

func countRecords(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(testContext(t), "SELECT count(*) FROM usage_records").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestInsert(t *testing.T) {
	s := migratedStore(t)
	if err := s.Insert(testContext(t), []usage.Record{successRecord("req-ok"), failureRecord("req-failed")}); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	ok := readRecord(t, s, "req-ok")
	if ok.ClientID != "team-a" || ok.RequestedModel != "gpt-4o" || ok.Status != 200 || ok.DurationMS != 1500 ||
		!ok.ReceivedAt.Equal(received) || deref(ok.Provider) != "openai" || deref(ok.Model) != "gpt-4o-2024-08-06" || ok.ErrorCode != nil {
		t.Errorf("success record = %+v", ok)
	}
	if deref(ok.Input) != 2006 || deref(ok.CacheRead) != 1920 || deref(ok.CacheWrite) != 0 || deref(ok.Output) != 300 {
		t.Errorf("success record tokens = %v %v %v %v, want 2006 1920 0 300",
			deref(ok.Input), deref(ok.CacheRead), deref(ok.CacheWrite), deref(ok.Output))
	}
	if got := deref(ok.CostUSD); got != "0.123456789012" {
		t.Errorf("cost_usd = %v, want exactly 0.123456789012", got)
	}

	failed := readRecord(t, s, "req-failed")
	if failed.Status != 404 || deref(failed.ErrorCode) != "model_not_found" {
		t.Errorf("failure record = %+v", failed)
	}
	if failed.Provider != nil || failed.Model != nil || failed.Input != nil || failed.CacheRead != nil ||
		failed.CacheWrite != nil || failed.Output != nil || failed.CostUSD != nil {
		t.Errorf("failure record has values for unknown fields: %+v", failed)
	}
}

func TestInsertUsageWithoutCost(t *testing.T) {
	s := migratedStore(t)
	r := successRecord("req-unpriced")
	r.Cost = nil
	if err := s.Insert(testContext(t), []usage.Record{r}); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if got := readRecord(t, s, "req-unpriced"); got.CostUSD != nil || deref(got.Input) != 2006 {
		t.Errorf("record = %+v, want usage with a NULL cost", got)
	}
}

func TestInsertSkipsStoredRequestIDs(t *testing.T) {
	s := migratedStore(t)
	ctx := testContext(t)
	if err := s.Insert(ctx, []usage.Record{successRecord("req-1")}); err != nil {
		t.Fatal(err)
	}

	// A retried batch: one record already stored, one new.
	retry := successRecord("req-1")
	retry.ClientID = "changed"
	if err := s.Insert(ctx, []usage.Record{retry, successRecord("req-2")}); err != nil {
		t.Fatalf("Insert() of a retried batch error = %v", err)
	}
	if n := countRecords(t, s); n != 2 {
		t.Errorf("stored %d records, want 2", n)
	}
	if got := readRecord(t, s, "req-1"); got.ClientID != "team-a" {
		t.Errorf("retried record overwrote the stored one: client %q", got.ClientID)
	}
}

func TestInsertRejectsInvalidRecordWithoutWriting(t *testing.T) {
	s := migratedStore(t)
	invalid := failureRecord("req-invalid")
	invalid.ClientID = ""

	err := s.Insert(testContext(t), []usage.Record{successRecord("req-ok"), invalid})
	if err == nil || !strings.Contains(err.Error(), "record 1") {
		t.Errorf("Insert() error = %v, want an error naming record 1", err)
	}
	if n := countRecords(t, s); n != 0 {
		t.Errorf("stored %d records, want 0", n)
	}
}

func TestInsertIsAtomic(t *testing.T) {
	s := migratedStore(t)
	ctx := testContext(t)
	// The database rejects one row of the batch, as a constraint could.
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION reject_poison() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.client_id = 'poison' THEN RAISE EXCEPTION 'rejected'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_poison BEFORE INSERT ON usage_records
			FOR EACH ROW EXECUTE FUNCTION reject_poison();`); err != nil {
		t.Fatal(err)
	}
	poison := successRecord("req-poison")
	poison.ClientID = "poison"

	if err := s.Insert(ctx, []usage.Record{successRecord("req-ok"), poison, successRecord("req-later")}); err == nil {
		t.Fatal("Insert() error = nil, want error")
	}
	if n := countRecords(t, s); n != 0 {
		t.Errorf("a failed batch stored %d records, want 0", n)
	}
}

func TestInsertEmptyBatch(t *testing.T) {
	s := migratedStore(t)
	if err := s.Insert(testContext(t), nil); err != nil {
		t.Errorf("Insert(nil) error = %v", err)
	}
}

func TestInsertBeforeMigrateFails(t *testing.T) {
	s := newStore(t)
	if err := s.Insert(testContext(t), []usage.Record{successRecord("req-1")}); err == nil {
		t.Error("Insert() into an unmigrated database error = nil, want error")
	}
}

func TestInsertHonorsContext(t *testing.T) {
	s := migratedStore(t)
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	if err := s.Insert(ctx, []usage.Record{successRecord("req-1")}); !errors.Is(err, context.Canceled) {
		t.Errorf("Insert() with a canceled context error = %v, want context.Canceled", err)
	}
}

func TestConcurrentInserts(t *testing.T) {
	s := migratedStore(t)
	ctx := testContext(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			batch := make([]usage.Record, 10)
			for j := range batch {
				batch[j] = successRecord(rand.Text())
			}
			if err := s.Insert(ctx, batch); err != nil {
				t.Errorf("Insert() %d error = %v", i, err)
			}
		})
	}
	wg.Wait()
	if n := countRecords(t, s); n != 80 {
		t.Errorf("stored %d records, want 80", n)
	}
}
