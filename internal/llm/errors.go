package llm

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnknownModel reports that no provider is configured for a model.
var ErrUnknownModel = errors.New("unknown model")

// ErrCircuitOpen reports that a provider was not called because its recent
// calls failed and its circuit breaker is open.
var ErrCircuitOpen = errors.New("circuit breaker open")

// ProviderError reports a failed upstream call: a non-2xx response, a
// transport failure, or an unreadable or unusable response body.
//
// It must not carry credentials or raw upstream bodies.
type ProviderError struct {
	// Provider identifies the upstream, e.g. "openai".
	Provider string
	// StatusCode is the upstream HTTP status, or 0 if no response was received.
	StatusCode int
	// RetryAfter is the delay the upstream asked for before a new attempt,
	// or 0 if it did not ask for one.
	RetryAfter time.Duration
	// Err is the underlying cause, if any.
	Err error
}

func (e *ProviderError) Error() string {
	switch {
	case e.StatusCode != 0 && e.Err != nil:
		return fmt.Sprintf("%s: upstream status %d: %v", e.Provider, e.StatusCode, e.Err)
	case e.StatusCode != 0:
		return fmt.Sprintf("%s: upstream status %d", e.Provider, e.StatusCode)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Provider, e.Err)
	default:
		return fmt.Sprintf("%s: upstream failure", e.Provider)
	}
}

func (e *ProviderError) Unwrap() error {
	return e.Err
}
