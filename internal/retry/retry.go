// Package retry wraps an llm.Provider with bounded retries for transient
// upstream failures.
//
// Chat completions are not idempotent: an attempt that reached the model
// may have produced a billed generation. Only failures that normally mean
// the upstream did no work are retried; see Retryable.
package retry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// statusOverloaded is Anthropic's non-standard "overloaded" status.
const statusOverloaded = 529

// Policy bounds the retries for one request.
type Policy struct {
	// MaxAttempts is the total number of attempts, including the first.
	// 1 disables retries.
	MaxAttempts int
	// BaseDelay is the backoff ceiling before the second attempt. It
	// doubles for each later attempt, up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps the backoff ceiling and the longest wait the upstream
	// may ask for in Retry-After. If it asks for longer, retrying stops,
	// so the caller (a fallback, or the client) is not held waiting.
	MaxDelay time.Duration
}

// Provider retries a wrapped provider according to a Policy. It is safe
// for concurrent use if the wrapped provider is.
type Provider struct {
	next   llm.Provider
	policy Policy
	log    *slog.Logger

	// jitter returns a random duration in [0, d]. sleep waits for d or
	// until ctx is done. Tests replace both.
	jitter func(d time.Duration) time.Duration
	sleep  func(ctx context.Context, d time.Duration) error
}

var _ llm.Provider = (*Provider)(nil)

// New returns a Provider that sends requests to next under policy. Each
// retry is logged to log; a nil log uses slog.Default.
func New(next llm.Provider, policy Policy, log *slog.Logger) (*Provider, error) {
	switch {
	case next == nil:
		return nil, errors.New("retry: nil provider")
	case policy.MaxAttempts < 1:
		return nil, errors.New("retry: max attempts must be at least 1")
	case policy.BaseDelay <= 0:
		return nil, errors.New("retry: base delay must be positive")
	case policy.MaxDelay < policy.BaseDelay:
		return nil, errors.New("retry: max delay must not be less than base delay")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Provider{next: next, policy: policy, log: log, jitter: fullJitter, sleep: sleep}, nil
}

// Chat sends req to the wrapped provider, retrying retryable failures until
// an attempt succeeds, the attempts are used up, or ctx is done.
//
// Every attempt shares ctx, so its deadline is the time budget for all
// attempts and waits. A wait that would end past the deadline, or a
// Retry-After longer than Policy.MaxDelay, is not started; the last failure
// is returned instead. If ctx is canceled or
// expires during a wait, the returned error wraps both the context error
// and the last failure.
//
// After more than one attempt, the error reports the attempt count and
// still wraps the last failure, so its type and status remain inspectable.
// Each repeated attempt is counted in the llm.Stats carried by ctx, if any.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	for attempt := 1; ; attempt++ {
		resp, err := p.next.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		if attempt == p.policy.MaxAttempts || !Retryable(err) || ctx.Err() != nil {
			return llm.ChatResponse{}, attempts(attempt, err)
		}

		if pe, ok := errors.AsType[*llm.ProviderError](err); ok && pe.RetryAfter > p.policy.MaxDelay {
			return llm.ChatResponse{}, attempts(attempt, err)
		}
		delay := p.delay(attempt, err)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return llm.ChatResponse{}, attempts(attempt, err)
		}
		p.logRetry(ctx, req.Model, attempt, delay, err)
		if ctxErr := p.sleep(ctx, delay); ctxErr != nil {
			return llm.ChatResponse{}, fmt.Errorf("%w while waiting to retry: %w", ctxErr, attempts(attempt, err))
		}
		llm.StatsFrom(ctx).AddRetry()
	}
}

// Retryable reports whether err is a failure the upstream normally did no
// work for: status 429, 502, 503, 504, or 529, or a failure to establish
// the connection. Context errors, other statuses, transport failures after
// the connection was established, and unusable responses are not
// retryable.
//
// A 502 or 504 can come from a provider's proxy after the model already
// produced a generation, so retrying one can duplicate a billed
// generation. That risk is accepted for these transient failures.
func Retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		return false
	}
	switch pe.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, statusOverloaded:
		return true
	case 0:
		// Dial failures, including DNS lookup and connect timeouts, happen
		// before any request byte is sent.
		opErr, ok := errors.AsType[*net.OpError](pe)
		return ok && opErr.Op == "dial"
	default:
		return false
	}
}

// logRetry records a failed attempt that will be retried. Like the
// handler's failure log, it contains no prompt or completion content; err
// is a *llm.ProviderError, which carries no upstream body.
func (p *Provider) logRetry(ctx context.Context, model string, attempt int, delay time.Duration, err error) {
	attrs := []slog.Attr{
		slog.String("model", model),
		slog.Int("attempt", attempt),
		slog.Duration("retry_in", delay),
	}
	if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
		attrs = append(attrs,
			slog.String("provider", pe.Provider),
			slog.Int("upstream_status", pe.StatusCode),
		)
	}
	attrs = append(attrs, slog.Any("error", err))
	p.log.LogAttrs(ctx, slog.LevelWarn, "upstream attempt failed, retrying", attrs...)
}

// delay returns how long to wait after the given failed attempt:
// exponential backoff with full jitter, but at least the upstream's
// Retry-After, which Chat has already checked against MaxDelay.
func (p *Provider) delay(attempt int, err error) time.Duration {
	ceiling := p.policy.BaseDelay
	for i := 1; i < attempt && ceiling < p.policy.MaxDelay; i++ {
		ceiling *= 2
	}
	d := p.jitter(min(ceiling, p.policy.MaxDelay))
	if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
		d = max(d, pe.RetryAfter)
	}
	return d
}

// attempts annotates err with the attempt count once a request was retried.
func attempts(n int, err error) error {
	if n == 1 {
		return err
	}
	return fmt.Errorf("after %d attempts: %w", n, err)
}

func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d + 1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
