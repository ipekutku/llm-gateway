package usage

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// StatusClientClosed is the Status of a request whose client disconnected
// before a response was written. It follows the convention of nginx's 499;
// no response with this status is ever sent.
const StatusClientClosed = 499

// Record describes one request for usage accounting. It holds metadata
// only, never prompt or completion content.
type Record struct {
	// RequestID is the gateway-assigned request ID.
	RequestID string
	// Time is when the request arrived.
	Time time.Time
	// Duration is how long the gateway took to answer.
	Duration time.Duration
	// ClientID identifies the authenticated client.
	ClientID string
	// RequestedModel is the model the client asked for.
	RequestedModel string
	// Provider is the provider that answered, or the last one that failed;
	// empty if no provider was called.
	Provider string
	// Model is the model the provider reported; empty on failure.
	Model string
	// Status is the HTTP status sent to the client, or StatusClientClosed.
	Status int
	// ErrorCode is the public error code; empty on success.
	ErrorCode string
	// Usage is the token usage the provider reported, or nil if unknown.
	Usage *llm.Usage
	// Cost is the estimated cost of Usage, or nil if unknown.
	Cost *Cost
}

// maxDuration bounds Record.Duration far above any request's time budget,
// so its milliseconds always fit in a 32-bit integer.
const maxDuration = 24 * time.Hour

// Validate reports an error if r is not a consistent record: a field is
// missing, a value is out of range, or the token counts or cost contradict
// each other. It never includes r's values in the error, except numbers.
func (r Record) Validate() error {
	switch {
	case r.RequestID == "":
		return errors.New("record has no request ID")
	case r.Time.IsZero():
		return errors.New("record has no time")
	case r.Duration < 0 || r.Duration > maxDuration:
		return fmt.Errorf("record duration %v is out of range", r.Duration)
	case r.ClientID == "":
		return errors.New("record has no client ID")
	case r.RequestedModel == "":
		return errors.New("record has no requested model")
	case r.Status < 100 || r.Status > 599:
		return fmt.Errorf("record status %d is not an HTTP status", r.Status)
	case (r.Status >= 200 && r.Status <= 299) != (r.ErrorCode == ""):
		return errors.New("record must have an error code exactly when its status is not 2xx")
	}
	if u := r.Usage; u != nil {
		for _, n := range []int{u.InputTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens, u.OutputTokens} {
			if n < 0 || n > math.MaxInt32 {
				return fmt.Errorf("record token count %d is out of range", n)
			}
		}
		if u.CacheReadInputTokens+u.CacheWriteInputTokens > u.InputTokens {
			return errors.New("record has more cached than input tokens")
		}
	}
	if r.Cost != nil {
		if r.Usage == nil {
			return errors.New("record has a cost but no usage")
		}
		if *r.Cost < 0 {
			return fmt.Errorf("record cost %v is negative", *r.Cost)
		}
	}
	return nil
}
