// Package breaker wraps an llm.Provider with a circuit breaker, so a
// provider that keeps failing is not called until it has had time to
// recover.
//
// A Breaker is closed while calls succeed. After Settings.Failures
// consecutive failures it opens and rejects calls with llm.ErrCircuitOpen
// without calling the provider. Once Settings.Cooldown has passed, it is
// half-open: exactly one call is let through as a probe. If the probe
// succeeds the breaker closes; if it fails the breaker opens again for
// another cooldown.
package breaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// Settings configures a Breaker.
type Settings struct {
	// Failures is the number of consecutive failures that opens the
	// breaker.
	Failures int
	// Cooldown is how long the breaker stays open before it lets a probe
	// through.
	Cooldown time.Duration
	// OnStateChange, if not nil, is called with the new state on every
	// state change. It is called with the breaker's lock held, so calls
	// arrive in order; it must return quickly and must not call the
	// Breaker.
	OnStateChange func(State)
}

// State is a breaker's state. A new Breaker is Closed.
type State int

// Breaker states.
const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	default:
		return "half-open"
	}
}

// outcome is how a call result affects the breaker.
type outcome int

const (
	// ignored results say nothing about the provider's health.
	ignored outcome = iota
	success
	failure
)

// Breaker is a circuit breaker around one provider. It is safe for
// concurrent use if the wrapped provider is.
type Breaker struct {
	name     string
	next     llm.Provider
	settings Settings
	log      *slog.Logger
	now      func() time.Time // replaced in tests

	mu       sync.Mutex
	state    State
	failures int       // consecutive failures while closed
	openedAt time.Time // when the breaker last opened
	probing  bool      // a half-open probe is in flight
	// generation changes with every state change. A result from a call
	// admitted under an earlier generation is stale and ignored, so a slow
	// call that started before the breaker opened cannot close it.
	generation uint64
}

var _ llm.Provider = (*Breaker)(nil)

// New returns a closed Breaker around next. name identifies the provider in
// errors and logs, for example "openai". A nil log uses slog.Default.
func New(name string, next llm.Provider, settings Settings, log *slog.Logger) (*Breaker, error) {
	switch {
	case strings.TrimSpace(name) == "":
		return nil, errors.New("breaker: blank provider name")
	case next == nil:
		return nil, errors.New("breaker: nil provider")
	case settings.Failures < 1:
		return nil, errors.New("breaker: failures must be at least 1")
	case settings.Cooldown <= 0:
		return nil, errors.New("breaker: cooldown must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Breaker{name: name, next: next, settings: settings, log: log, now: time.Now}, nil
}

// Chat calls the wrapped provider unless the breaker is open. A rejected
// call returns an error wrapping llm.ErrCircuitOpen and naming the
// provider, without calling it.
func (b *Breaker) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	gen, err := b.admit(ctx)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	resp, err := b.next.Chat(ctx, req)
	b.record(ctx, gen, classify(err))
	return resp, err
}

// admit decides whether a call may proceed and returns the generation it
// was admitted under.
func (b *Breaker) admit(ctx context.Context) (uint64, error) {
	b.mu.Lock()
	var halfOpened bool
	if b.state == Open && b.now().Sub(b.openedAt) >= b.settings.Cooldown {
		b.setState(HalfOpen)
		halfOpened = true
	}
	admitted := b.state == Closed || (b.state == HalfOpen && !b.probing)
	if admitted && b.state == HalfOpen {
		b.probing = true
	}
	gen := b.generation
	b.mu.Unlock()

	if halfOpened {
		b.log.LogAttrs(ctx, slog.LevelInfo, "circuit half-open, probing provider", slog.String("provider", b.name))
	}
	if !admitted {
		return 0, fmt.Errorf("%s: %w", b.name, llm.ErrCircuitOpen)
	}
	return gen, nil
}

// record applies the outcome of a call admitted under gen.
func (b *Breaker) record(ctx context.Context, gen uint64, o outcome) {
	b.mu.Lock()
	if gen != b.generation {
		b.mu.Unlock()
		return
	}
	from := b.state
	failures := 1 // consecutive failures, for the log when the breaker opens
	switch {
	case b.state == HalfOpen && o == ignored:
		// The probe told us nothing, for example because the client went
		// away. The next call probes instead.
		b.probing = false
	case b.state == HalfOpen && o == success:
		b.setState(Closed)
	case b.state == HalfOpen && o == failure:
		b.trip()
	case o == success:
		b.failures = 0
	case o == failure:
		b.failures++
		failures = b.failures
		if b.failures >= b.settings.Failures {
			b.trip()
		}
	}
	to := b.state
	b.mu.Unlock()

	switch {
	case from == to:
	case to == Open:
		b.log.LogAttrs(ctx, slog.LevelWarn, "circuit opened",
			slog.String("provider", b.name),
			slog.String("from", from.String()),
			slog.Int("consecutive_failures", failures),
			slog.Duration("cooldown", b.settings.Cooldown),
		)
	case to == Closed:
		b.log.LogAttrs(ctx, slog.LevelInfo, "circuit closed", slog.String("provider", b.name))
	}
}

// trip opens the breaker. b.mu must be held.
func (b *Breaker) trip() {
	b.setState(Open)
	b.openedAt = b.now()
}

// setState moves to s and starts a new generation. b.mu must be held.
func (b *Breaker) setState(s State) {
	b.state = s
	b.failures = 0
	b.probing = false
	b.generation++
	if b.settings.OnStateChange != nil {
		b.settings.OnStateChange(s)
	}
}

// classify maps a call result to its effect on the breaker. Failures are
// signs the provider is unavailable to the gateway: rate limiting (429),
// server errors (5xx, including Anthropic's 529), transport failures,
// timeouts, and unusable successful responses. Other 4xx responses show
// the provider is up and count as success. Client cancellations and errors
// that do not come from the provider are ignored.
func classify(err error) outcome {
	switch {
	case err == nil:
		return success
	case errors.Is(err, context.Canceled):
		return ignored
	case errors.Is(err, context.DeadlineExceeded):
		return failure
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	switch {
	case !ok:
		return ignored
	case pe.StatusCode == http.StatusTooManyRequests, pe.StatusCode >= 500:
		return failure
	case pe.StatusCode >= 400:
		return success
	default:
		// Status 0 (no response) or an unusable 2xx response.
		return failure
	}
}
