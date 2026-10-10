package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Mode says which limiter a Fallback uses.
type Mode int

// Fallback modes. A new Fallback is in SharedMode.
const (
	// SharedMode uses the Shared limiter in Redis.
	SharedMode Mode = iota
	// LocalMode uses the in-process Limiter, because Redis failed.
	LocalMode
)

func (m Mode) String() string {
	if m == SharedMode {
		return "shared"
	}
	return "local"
}

// FallbackSettings configures a Fallback.
type FallbackSettings struct {
	// RetryInterval is how long after a Redis failure the Fallback uses
	// local limits before it tries Redis again. It must be positive.
	RetryInterval time.Duration
	// StartLocal starts the Fallback in LocalMode, for a Redis known to be
	// unreachable, such as at startup. The first request then probes Redis.
	// Starting in LocalMode is not a mode change and is not logged.
	StartLocal bool
	// OnModeChange, if not nil, is called with the new mode on every mode
	// change. It is called with the Fallback's lock held, so calls arrive in
	// order; it must return quickly and must not call the Fallback.
	OnModeChange func(Mode)
}

// Fallback limits requests with a Shared limiter while Redis works, and
// with an in-process Limiter while it does not, so a Redis outage neither
// rejects every request nor removes all limits. In LocalMode each gateway
// instance applies the full limits on its own, as without Redis.
//
// After a Redis failure, Fallback stays in LocalMode for RetryInterval
// without calling Redis, so an outage does not add a Redis timeout to every
// request. Then one request at a time probes Redis; the first one that
// Redis answers returns it to SharedMode. Each mode change is logged once.
//
// Fallback is safe for concurrent use.
type Fallback struct {
	shared   *Shared
	local    *Limiter
	settings FallbackSettings
	log      *slog.Logger
	now      func() time.Time // replaced in tests

	mu      sync.Mutex
	mode    Mode
	probeAt time.Time // in LocalMode, when Redis may be tried again
}

// NewFallback returns a Fallback in SharedMode, or in LocalMode if
// settings.StartLocal is set. shared and local should have the same client
// limits. A nil log uses slog.Default.
func NewFallback(shared *Shared, local *Limiter, settings FallbackSettings, log *slog.Logger) (*Fallback, error) {
	switch {
	case shared == nil:
		return nil, errors.New("ratelimit: nil shared limiter")
	case local == nil:
		return nil, errors.New("ratelimit: nil local limiter")
	case settings.RetryInterval <= 0:
		return nil, errors.New("ratelimit: retry interval must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	f := &Fallback{shared: shared, local: local, settings: settings, log: log, now: time.Now}
	if settings.StartLocal {
		f.mode = LocalMode
	}
	return f, nil
}

// Mode returns the current mode.
func (f *Fallback) Mode() Mode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

// Acquire admits one request for clientID, or rejects it with an *Error,
// using the shared or the local limits as described on Fallback. The
// returned release function frees the slot in the limiter that admitted
// the request, whatever the mode is by then.
//
// A Redis failure is never returned: the request is decided by the local
// limits instead. If ctx ends during the Redis call, its error is
// returned; that says nothing about Redis, so the mode does not change.
func (f *Fallback) Acquire(ctx context.Context, clientID string) (release func(), err error) {
	if !f.tryShared() {
		return f.local.Acquire(ctx, clientID)
	}
	release, err = f.shared.Acquire(ctx, clientID)
	if _, rejected := errors.AsType[*Error](err); err == nil || rejected || errors.Is(err, ErrUnknownClient) {
		f.succeeded(ctx)
		return release, err
	}
	if ctx.Err() != nil {
		f.inconclusive()
		return nil, err
	}
	f.failed(ctx, err)
	return f.local.Acquire(ctx, clientID)
}

// tryShared reports whether to call Redis. In LocalMode only one call is
// let through per RetryInterval, as a probe.
func (f *Fallback) tryShared() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == SharedMode {
		return true
	}
	now := f.now()
	if now.Before(f.probeAt) {
		return false
	}
	f.probeAt = now.Add(f.settings.RetryInterval)
	return true
}

// succeeded records that Redis answered.
func (f *Fallback) succeeded(ctx context.Context) {
	f.mu.Lock()
	recovered := f.mode == LocalMode
	if recovered {
		f.setMode(SharedMode)
	}
	f.mu.Unlock()
	if recovered {
		f.log.LogAttrs(ctx, slog.LevelInfo, "rate limit store recovered, using shared limits")
	}
}

// failed records a Redis failure.
func (f *Fallback) failed(ctx context.Context, err error) {
	f.mu.Lock()
	f.probeAt = f.now().Add(f.settings.RetryInterval)
	degraded := f.mode == SharedMode
	if degraded {
		f.setMode(LocalMode)
	}
	f.mu.Unlock()
	if degraded {
		f.log.LogAttrs(ctx, slog.LevelWarn, "rate limit store unavailable, using local limits",
			slog.String("error", err.Error()),
			slog.Duration("retry_in", f.settings.RetryInterval),
		)
	}
}

// inconclusive records a Redis call that ended because its request did. A
// probe that tells nothing lets the next request probe.
func (f *Fallback) inconclusive() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == LocalMode {
		f.probeAt = f.now()
	}
}

// setMode changes the mode. f.mu must be held.
func (f *Fallback) setMode(m Mode) {
	f.mode = m
	if f.settings.OnModeChange != nil {
		f.settings.OnModeChange(m)
	}
}
