package postgres

import (
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipekutku/llm-gateway/internal/usage"
)

// tracedStore is newStore with spans recorded in memory. It also returns
// the database URL.
func tracedStore(t *testing.T) (*Store, *tracetest.SpanRecorder, string) {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	url := newDatabase(t)
	s, err := Open(testContext(t), url, tp)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(s.Close)
	return s, spans, url
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := make(map[string]string)
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// spansNamed returns the ended spans called name.
func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// requestSpan returns a span context as a request's server span would have.
func requestSpan(n byte, sampled bool) trace.SpanContext {
	cfg := trace.SpanContextConfig{TraceID: trace.TraceID{n}, SpanID: trace.SpanID{n}}
	if sampled {
		cfg.TraceFlags = trace.FlagsSampled
	}
	return trace.NewSpanContext(cfg)
}

func TestInsertSpan(t *testing.T) {
	s, spans, url := tracedStore(t)
	ctx := testContext(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, []usage.Record{successRecord("req-secret-1")}); err != nil {
		t.Fatal(err)
	}
	// One record is already stored and skipped. Unsampled request spans
	// are not linked: they were never exported.
	stored, fresh, unsampled := successRecord("req-secret-1"), failureRecord("req-secret-2"), successRecord("req-secret-3")
	stored.Span, fresh.Span, unsampled.Span = requestSpan(1, true), requestSpan(2, true), requestSpan(3, false)
	if err := s.Insert(ctx, []usage.Record{stored, fresh, unsampled}); err != nil {
		t.Fatal(err)
	}

	inserts := spansNamed(spans, "INSERT usage_records")
	if len(inserts) != 2 {
		t.Fatalf("got %d insert spans, want one per batch", len(inserts))
	}
	span := inserts[1]
	if span.SpanKind() != trace.SpanKindClient || span.Status().Code == codes.Error || span.Parent().IsValid() {
		t.Errorf("insert span: kind %v, status %v, parent %v; want a successful root client span", span.SpanKind(), span.Status(), span.Parent())
	}
	want := map[string]string{
		"db.system.name": "postgresql", "db.operation.name": "INSERT", "db.collection.name": "usage_records",
		"gateway.usage.records": "3", "gateway.usage.inserted": "2",
	}
	got := spanAttrs(span)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attributes = %v, want %v", got, want)
	}
	var links []trace.SpanContext
	for _, l := range span.Links() {
		links = append(links, l.SpanContext)
	}
	if len(links) != 2 || !links[0].Equal(stored.Span) || !links[1].Equal(fresh.Span) {
		t.Errorf("links = %v, want the two sampled request spans", links)
	}

	// No record values or connection settings.
	dump := fmt.Sprint(got, span.Status(), span.Events())
	for _, secret := range []string{"req-secret", "team-a", "team-b", url} {
		if strings.Contains(dump, secret) {
			t.Errorf("insert span contains %q: %s", secret, dump)
		}
	}
}

func TestInsertSpanError(t *testing.T) {
	s, spans, _ := tracedStore(t) // not migrated
	if err := s.Insert(testContext(t), []usage.Record{successRecord("req-1")}); err == nil {
		t.Fatal("Insert() error = nil, want error")
	}
	inserts := spansNamed(spans, "INSERT usage_records")
	if len(inserts) != 1 {
		t.Fatalf("got %d insert spans, want 1", len(inserts))
	}
	span := inserts[0]
	attrs := spanAttrs(span)
	// 42P01: undefined_table.
	if span.Status().Code != codes.Error || attrs["error.type"] != "42P01" {
		t.Errorf("status %v, error.type %q; want an error with SQLSTATE 42P01", span.Status(), attrs["error.type"])
	}
	if _, ok := attrs["gateway.usage.inserted"]; ok {
		t.Error("failed insert reports inserted rows")
	}
}

func TestInsertEmptyBatchHasNoSpan(t *testing.T) {
	s, spans, _ := tracedStore(t)
	if err := s.Insert(testContext(t), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(spans.Ended()); n != 0 {
		t.Errorf("got %d spans, want none", n)
	}
}

func TestMigrateSpans(t *testing.T) {
	s, spans, _ := tracedStore(t)
	ctx := testContext(t)
	for range 2 {
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs := spansNamed(spans, "migrate")
	if len(runs) != 2 {
		t.Fatalf("got %d migrate spans, want 2", len(runs))
	}
	for i, want := range []string{"1", "0"} {
		if got := spanAttrs(runs[i])["gateway.migrations.applied"]; got != want {
			t.Errorf("run %d applied = %q, want %s", i, got, want)
		}
	}
	applied := spansNamed(spans, "migration 0001_create_usage_records.sql")
	if len(applied) != 1 || applied[0].Parent().SpanID() != runs[0].SpanContext().SpanID() {
		t.Fatalf("migration spans = %d, want one child of the first run", len(applied))
	}
	if v := spanAttrs(applied[0])["gateway.migration.version"]; v != "1" {
		t.Errorf("migration version = %q, want 1", v)
	}
}

func TestMigrateSpansOnFailure(t *testing.T) {
	useMigrations(t, map[string]string{
		"0001_good.sql":   "CREATE TABLE first (id integer);",
		"0002_broken.sql": "SELEC 1;",
	})
	s, spans, _ := tracedStore(t)
	if _, err := s.Migrate(testContext(t)); err == nil {
		t.Fatal("Migrate() error = nil, want error")
	}
	// 42601: syntax_error.
	for _, name := range []string{"migrate", "migration 0002_broken.sql"} {
		got := spansNamed(spans, name)
		if len(got) != 1 || got[0].Status().Code != codes.Error || spanAttrs(got[0])["error.type"] != "42601" {
			t.Errorf("%s: want one span with error.type 42601", name)
		}
	}
	if good := spansNamed(spans, "migration 0001_good.sql"); len(good) != 1 || good[0].Status().Code == codes.Error {
		t.Error("the migration that ran before the failure has no successful span")
	}
}
