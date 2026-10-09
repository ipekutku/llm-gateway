package usage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// RecordStore persists a batch of usage records. Implementations must honor
// ctx. Each Recorder calls Insert on only one batch at a time.
type RecordStore interface {
	Insert(ctx context.Context, records []Record) error
}

// RecorderOptions bounds memory, batch size, and time spent waiting for the
// database. All values must be positive.
type RecorderOptions struct {
	QueueCapacity int
	BatchSize     int
	FlushInterval time.Duration
	WriteTimeout  time.Duration
	// DropReportInterval is how often records dropped because the queue was
	// full are reported, as one warning with their count.
	DropReportInterval time.Duration
}

// DefaultRecorderOptions returns the initial operating limits for a recorder.
func DefaultRecorderOptions() RecorderOptions {
	return RecorderOptions{
		QueueCapacity:      1024,
		BatchSize:          100,
		FlushInterval:      time.Second,
		WriteTimeout:       5 * time.Second,
		DropReportInterval: 10 * time.Second,
	}
}

// Recorder queues records without delaying requests and writes them in
// batches. A failed write loses that batch; it is logged and later batches
// are still attempted. Records dropped because the queue is full are counted
// and reported periodically rather than one warning each, so a database
// outage under load does not flood the logs. Its worker has its own context, so a client
// disconnecting does not cancel the write of an accepted record.
type Recorder struct {
	store   RecordStore
	log     *slog.Logger
	opts    RecorderOptions
	queue   chan Record
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	closing bool
	// dropped counts records rejected by a full queue since the last report.
	dropped atomic.Int64
}

// NewRecorder starts a background writer. The caller must call Close during
// shutdown and give it a bounded context. A nil logger uses slog.Default.
func NewRecorder(store RecordStore, logger *slog.Logger, opts RecorderOptions) (*Recorder, error) {
	if store == nil {
		return nil, errors.New("usage recorder needs a store")
	}
	if opts.QueueCapacity <= 0 || opts.BatchSize <= 0 || opts.FlushInterval <= 0 || opts.WriteTimeout <= 0 || opts.DropReportInterval <= 0 {
		return nil, errors.New("usage recorder limits must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Recorder{
		store:  store,
		log:    logger,
		opts:   opts,
		queue:  make(chan Record, opts.QueueCapacity),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go r.run(ctx)
	return r, nil
}

// Record queues a validated snapshot without blocking on database I/O. It
// returns false if the record is invalid, the queue is full, or shutdown has
// begun. Every rejection is logged, a full queue's in a periodic summary;
// callers need not fail the request.
func (r *Recorder) Record(record Record) bool {
	if err := record.Validate(); err != nil {
		r.log.Error("invalid usage record dropped", slog.Any("error", err))
		return false
	}
	if record.Usage != nil {
		u := *record.Usage
		record.Usage = &u
	}
	if record.Cost != nil {
		c := *record.Cost
		record.Cost = &c
	}

	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		r.log.Warn("usage record dropped: recorder closed", slog.String("request_id", record.RequestID))
		return false
	}
	select {
	case r.queue <- record:
		r.mu.Unlock()
		return true
	default:
		r.mu.Unlock()
		r.dropped.Add(1)
		return false
	}
}

// Close stops accepting records, drains the queue, and waits for the writer.
// If ctx expires, it cancels an in-progress database write and returns the
// context error. A store must honor its context for shutdown to be bounded.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.closing {
		r.closing = true
		close(r.queue)
	}
	r.mu.Unlock()

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		r.cancel()
		return ctx.Err()
	}
}

func (r *Recorder) run(ctx context.Context) {
	defer close(r.done)
	defer r.cancel()
	// Runs before done is closed, so drops are reported by the time Close
	// returns.
	defer r.reportDrops()
	dropReport := time.NewTicker(r.opts.DropReportInterval)
	defer dropReport.Stop()

	batch := make([]Record, 0, r.opts.BatchSize)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var flushAt <-chan time.Time
	flush := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			flushAt = nil
		}
		if len(batch) == 0 {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, r.opts.WriteTimeout)
		err := r.store.Insert(writeCtx, batch)
		cancel()
		if err != nil {
			r.log.Warn("usage records not persisted", slog.Int("count", len(batch)), slog.Any("error", err))
		}
		batch = batch[:0]
	}

	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case record, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			if len(batch) == 0 && r.opts.BatchSize > 1 {
				timer = time.NewTimer(r.opts.FlushInterval)
				flushAt = timer.C
			}
			batch = append(batch, record)
			if len(batch) == r.opts.BatchSize {
				flush()
			}
		case <-flushAt:
			flush()
		case <-dropReport.C:
			r.reportDrops()
		case <-ctx.Done():
			return
		}
	}
}

// reportDrops logs how many records a full queue rejected since the last
// report, if any.
func (r *Recorder) reportDrops() {
	if n := r.dropped.Swap(0); n > 0 {
		r.log.Warn("usage records dropped: queue full", slog.Int64("count", n))
	}
}
