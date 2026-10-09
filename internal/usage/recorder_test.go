package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureStore struct {
	batches chan []Record
	err     error
}

func (s *captureStore) Insert(ctx context.Context, records []Record) error {
	batch := append([]Record(nil), records...)
	select {
	case s.batches <- batch:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func testRecorderOptions() RecorderOptions {
	return RecorderOptions{
		QueueCapacity:      4,
		BatchSize:          2,
		FlushInterval:      time.Hour,
		WriteTimeout:       time.Second,
		DropReportInterval: time.Hour,
	}
}

func testRecorderContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func receiveBatch(t *testing.T, ctx context.Context, batches <-chan []Record) []Record {
	t.Helper()
	select {
	case batch := <-batches:
		return batch
	case <-ctx.Done():
		t.Fatalf("timed out waiting for usage batch: %v", ctx.Err())
		return nil
	}
}

func newTestRecorder(t *testing.T, store RecordStore, logger *slog.Logger, opts RecorderOptions) *Recorder {
	t.Helper()
	r, err := NewRecorder(store, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("recorder cleanup: %v", err)
		}
	})
	return r
}

func testRecord(id string) Record {
	r := validRecord()
	r.RequestID = id
	return r
}

func TestNewRecorderRejectsInvalidInputs(t *testing.T) {
	store := &captureStore{batches: make(chan []Record, 1)}
	if _, err := NewRecorder(nil, nil, DefaultRecorderOptions()); err == nil {
		t.Error("NewRecorder with a nil store succeeded")
	}
	for name, change := range map[string]func(*RecorderOptions){
		"queue": func(o *RecorderOptions) { o.QueueCapacity = 0 },
		"batch": func(o *RecorderOptions) { o.BatchSize = 0 },
		"flush": func(o *RecorderOptions) { o.FlushInterval = 0 },
		"write": func(o *RecorderOptions) { o.WriteTimeout = 0 },
		"drops": func(o *RecorderOptions) { o.DropReportInterval = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			opts := DefaultRecorderOptions()
			change(&opts)
			if _, err := NewRecorder(store, nil, opts); err == nil {
				t.Error("NewRecorder with an invalid limit succeeded")
			}
		})
	}
}

func TestRecorderBatchesAndDrainsOnClose(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &captureStore{batches: make(chan []Record, 2)}
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(io.Discard, nil)), testRecorderOptions())
	for _, id := range []string{"first", "second"} {
		if !r.Record(testRecord(id)) {
			t.Fatalf("Record(%q) rejected", id)
		}
	}
	first := receiveBatch(t, ctx, store.batches)
	if len(first) != 2 || first[0].RequestID != "first" || first[1].RequestID != "second" {
		t.Errorf("first batch = %+v", first)
	}
	if !r.Record(testRecord("third")) {
		t.Fatal("Record(third) rejected")
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	second := receiveBatch(t, ctx, store.batches)
	if len(second) != 1 || second[0].RequestID != "third" {
		t.Errorf("shutdown batch = %+v", second)
	}
	if r.Record(testRecord("late")) {
		t.Error("Record after Close succeeded")
	}
}

func TestRecorderFlushesPartialBatch(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &captureStore{batches: make(chan []Record, 1)}
	opts := testRecorderOptions()
	opts.FlushInterval = 10 * time.Millisecond
	r := newTestRecorder(t, store, nil, opts)
	if !r.Record(testRecord("partial")) {
		t.Fatal("Record rejected")
	}
	batch := receiveBatch(t, ctx, store.batches)
	if len(batch) != 1 || batch[0].RequestID != "partial" {
		t.Errorf("partial batch = %+v", batch)
	}
}

type blockFirstStore struct {
	started chan struct{}
	release chan struct{}
	batches chan []Record
	first   sync.Once
}

func (s *blockFirstStore) Insert(ctx context.Context, records []Record) error {
	blocked := false
	s.first.Do(func() {
		blocked = true
		close(s.started)
	})
	if blocked {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case s.batches <- append([]Record(nil), records...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRecorderDropsWhenQueueIsFull(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &blockFirstStore{
		started: make(chan struct{}), release: make(chan struct{}), batches: make(chan []Record, 2),
	}
	var logs bytes.Buffer
	opts := testRecorderOptions()
	opts.QueueCapacity, opts.BatchSize = 1, 1
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(&logs, nil)), opts)
	if !r.Record(testRecord("first")) {
		t.Fatal("first Record rejected")
	}
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !r.Record(testRecord("second")) {
		t.Fatal("second Record rejected")
	}
	if r.Record(testRecord("dropped")) {
		t.Error("Record into a full queue succeeded")
	}
	close(store.release)
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	first := receiveBatch(t, ctx, store.batches)
	second := receiveBatch(t, ctx, store.batches)
	if first[0].RequestID != "first" || second[0].RequestID != "second" {
		t.Errorf("stored IDs = %q, %q", first[0].RequestID, second[0].RequestID)
	}
	if !strings.Contains(logs.String(), "usage records dropped: queue full") || !strings.Contains(logs.String(), "count=1") {
		t.Errorf("queue-full summary missing: %s", logs.String())
	}
}

// fillQueue returns a recorder whose writer is blocked in its first insert
// and whose one-record queue is full, so every further Record is dropped.
// Closing release lets the writer continue.
func fillQueue(t *testing.T, ctx context.Context, logger *slog.Logger, opts RecorderOptions) (r *Recorder, release chan struct{}) {
	t.Helper()
	store := &blockFirstStore{
		started: make(chan struct{}), release: make(chan struct{}), batches: make(chan []Record, 2),
	}
	opts.QueueCapacity, opts.BatchSize = 1, 1
	r = newTestRecorder(t, store, logger, opts)
	if !r.Record(testRecord("blocked")) {
		t.Fatal("first Record rejected")
	}
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !r.Record(testRecord("queued")) {
		t.Fatal("second Record rejected")
	}
	return r, store.release
}

func TestRecorderSummarizesDropsInOneWarning(t *testing.T) {
	ctx := testRecorderContext(t)
	var logs bytes.Buffer
	r, release := fillQueue(t, ctx, slog.New(slog.NewTextHandler(&logs, nil)), testRecorderOptions())
	for i := range 50 {
		if r.Record(testRecord("dropped-" + strconv.Itoa(i))) {
			t.Fatal("Record into a full queue succeeded")
		}
	}
	close(release)
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), "usage records dropped"); got != 1 {
		t.Errorf("got %d drop warnings, want 1: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "count=50") || strings.Contains(logs.String(), "dropped-") {
		t.Errorf("drop summary should count 50 records without naming them: %s", logs.String())
	}
}

// lineWriter sends every log line to a channel, so a test can wait for a
// line the writer goroutine logs.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func TestRecorderReportsDropsPeriodically(t *testing.T) {
	ctx := testRecorderContext(t)
	lines := make(lineWriter, 16)
	opts := testRecorderOptions()
	opts.DropReportInterval = 10 * time.Millisecond
	r, release := fillQueue(t, ctx, slog.New(slog.NewTextHandler(lines, nil)), opts)
	for range 3 {
		if r.Record(testRecord("dropped")) {
			t.Fatal("Record into a full queue succeeded")
		}
	}
	// Once its insert is released, the writer stores the two accepted
	// records and the ticker reports the drops while the recorder is still
	// open; Close runs only in cleanup.
	close(release)
	select {
	case line := <-lines:
		if !strings.Contains(line, "usage records dropped") || !strings.Contains(line, "count=3") {
			t.Errorf("log line = %q, want a drop summary with count=3", line)
		}
	case <-ctx.Done():
		t.Fatal("no periodic drop report before shutdown")
	}
}

func TestRecorderLogsWriteFailureAndContinues(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &captureStore{batches: make(chan []Record, 2), err: errors.New("database unavailable")}
	var logs bytes.Buffer
	opts := testRecorderOptions()
	opts.BatchSize = 1
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(&logs, nil)), opts)
	if !r.Record(testRecord("first")) {
		t.Fatal("first Record rejected")
	}
	receiveBatch(t, ctx, store.batches)
	if !r.Record(testRecord("second")) {
		t.Fatal("second Record rejected after write failure")
	}
	receiveBatch(t, ctx, store.batches)
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), "usage records not persisted"); got != 2 {
		t.Errorf("got %d failure warnings, want 2: %s", got, logs.String())
	}
}

func TestRecorderWriteTimeoutDoesNotBlockLaterBatches(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &blockFirstStore{
		started: make(chan struct{}), release: make(chan struct{}), batches: make(chan []Record, 1),
	}
	opts := testRecorderOptions()
	opts.BatchSize = 1
	opts.WriteTimeout = 10 * time.Millisecond
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	if !r.Record(testRecord("timed-out")) {
		t.Fatal("first Record rejected")
	}
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !r.Record(testRecord("next")) {
		t.Fatal("second Record rejected")
	}
	batch := receiveBatch(t, ctx, store.batches)
	if len(batch) != 1 || batch[0].RequestID != "next" {
		t.Errorf("batch after timeout = %+v", batch)
	}
}

func TestRecorderRejectsInvalidRecordBeforeWriting(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &captureStore{batches: make(chan []Record, 1)}
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(io.Discard, nil)), testRecorderOptions())
	invalid := testRecord("invalid")
	invalid.ClientID = ""
	if r.Record(invalid) {
		t.Error("invalid Record accepted")
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 0 {
		t.Error("invalid record reached the store")
	}
}

func TestRecorderSnapshotsUsageAndCost(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &captureStore{batches: make(chan []Record, 1)}
	r := newTestRecorder(t, store, nil, testRecorderOptions())
	record := testRecord("snapshot")
	if !r.Record(record) {
		t.Fatal("Record rejected")
	}
	record.Usage.InputTokens = 999
	*record.Cost = 999
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stored := receiveBatch(t, ctx, store.batches)[0]
	if stored.Usage.InputTokens != 10 || *stored.Cost != 1 {
		t.Errorf("queued record was mutated: usage %+v, cost %v", stored.Usage, *stored.Cost)
	}
}

func TestRecorderCloseCancelsBlockedWrite(t *testing.T) {
	ctx := testRecorderContext(t)
	store := &blockFirstStore{
		started: make(chan struct{}), release: make(chan struct{}), batches: make(chan []Record, 1),
	}
	opts := testRecorderOptions()
	opts.BatchSize = 1
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	if !r.Record(testRecord("blocked")) {
		t.Fatal("Record rejected")
	}
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closeCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Close(closeCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("Close() = %v, want context.Canceled", err)
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		t.Fatal("worker did not stop after Close canceled it")
	}
}

type countStore struct {
	mu    sync.Mutex
	count int
}

func (s *countStore) Insert(_ context.Context, records []Record) error {
	s.mu.Lock()
	s.count += len(records)
	s.mu.Unlock()
	return nil
}

func TestRecorderRecordRacesWithClose(t *testing.T) {
	store := &countStore{}
	opts := testRecorderOptions()
	opts.QueueCapacity = 64
	r := newTestRecorder(t, store, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	ctx := testRecorderContext(t)
	start := make(chan struct{})
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			<-start
			if r.Record(testRecord(strconv.Itoa(i))) {
				accepted.Add(1)
			}
		})
	}
	close(start)
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	store.mu.Lock()
	got := store.count
	store.mu.Unlock()
	if got != int(accepted.Load()) {
		t.Errorf("stored %d records, accepted %d", got, accepted.Load())
	}
}
