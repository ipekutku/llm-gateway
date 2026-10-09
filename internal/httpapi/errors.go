package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// Error types returned in the error envelope.
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typeRateLimit      = "rate_limit_error"
	typeServer         = "server_error"
)

// Error codes returned in the error envelope.
const (
	codeInvalidRequest           = "invalid_request"
	codeRequestTooLarge          = "request_too_large"
	codeMissingAPIKey            = "missing_api_key"
	codeInvalidAPIKey            = "invalid_api_key"
	codeRateLimitExceeded        = "rate_limit_exceeded"
	codeConcurrencyLimitExceeded = "concurrency_limit_exceeded"
	codeModelNotFound            = "model_not_found"
	codeProviderRateLimited      = "provider_rate_limited"
	codeUpstreamError            = "upstream_error"
	codeUpstreamTimeout          = "upstream_timeout"
	codeProviderUnavailable      = "provider_unavailable"
	codeInternalError            = "internal_error"
)

// apiError is a gateway-owned error response. Its message is fixed per
// category, or gateway-authored for validation failures, and never contains
// upstream data.
type apiError struct {
	status  int
	typ     string
	code    string
	message string
}

var (
	errRequestTooLarge = apiError{
		status:  http.StatusRequestEntityTooLarge,
		typ:     typeInvalidRequest,
		code:    codeRequestTooLarge,
		message: "The request body exceeds the 1 MiB limit.",
	}
	errMissingAPIKey = apiError{
		status:  http.StatusUnauthorized,
		typ:     typeAuthentication,
		code:    codeMissingAPIKey,
		message: "Send a gateway API key in the Authorization header using the Bearer scheme.",
	}
	errInvalidAPIKey = apiError{
		status:  http.StatusUnauthorized,
		typ:     typeAuthentication,
		code:    codeInvalidAPIKey,
		message: "The API key is invalid or disabled.",
	}
	errRateLimitExceeded = apiError{
		status:  http.StatusTooManyRequests,
		typ:     typeRateLimit,
		code:    codeRateLimitExceeded,
		message: "Too many requests for this API key. Retry after the time in the Retry-After header.",
	}
	errConcurrencyLimitExceeded = apiError{
		status:  http.StatusTooManyRequests,
		typ:     typeRateLimit,
		code:    codeConcurrencyLimitExceeded,
		message: "Too many concurrent requests for this API key. Retry when a request has finished.",
	}
	errModelNotFound = apiError{
		status:  http.StatusNotFound,
		typ:     typeInvalidRequest,
		code:    codeModelNotFound,
		message: "The requested model is not configured.",
	}
	errUpstreamRejected = apiError{
		status:  http.StatusBadRequest,
		typ:     typeInvalidRequest,
		code:    codeInvalidRequest,
		message: "The upstream provider rejected the request.",
	}
	errProviderRateLimited = apiError{
		status:  http.StatusTooManyRequests,
		typ:     typeRateLimit,
		code:    codeProviderRateLimited,
		message: "The upstream provider is rate limiting requests.",
	}
	errUpstream = apiError{
		status:  http.StatusBadGateway,
		typ:     typeServer,
		code:    codeUpstreamError,
		message: "The upstream provider could not complete the request.",
	}
	errUpstreamTimeout = apiError{
		status:  http.StatusGatewayTimeout,
		typ:     typeServer,
		code:    codeUpstreamTimeout,
		message: "The upstream provider did not respond in time.",
	}
	errProviderUnavailable = apiError{
		status:  http.StatusServiceUnavailable,
		typ:     typeServer,
		code:    codeProviderUnavailable,
		message: "The upstream provider is temporarily unavailable after repeated failures.",
	}
	errInternal = apiError{
		status:  http.StatusInternalServerError,
		typ:     typeServer,
		code:    codeInternalError,
		message: "The gateway encountered an unexpected error.",
	}
)

func invalidRequest(message string) apiError {
	return apiError{
		status:  http.StatusBadRequest,
		typ:     typeInvalidRequest,
		code:    codeInvalidRequest,
		message: message,
	}
}

// classifyChatError maps an error returned by the provider (normally the
// router) to a response. The caller must already have checked that the
// incoming request is still active.
//
// Context errors are checked before provider errors: a deadline means the
// upstream timed out, and a cancellation that did not come from the
// incoming request is an upstream failure.
func classifyChatError(err error) apiError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errUpstreamTimeout
	case errors.Is(err, context.Canceled):
		return errUpstream
	case errors.Is(err, llm.ErrUnknownModel):
		return errModelNotFound
	case errors.Is(err, llm.ErrCircuitOpen):
		return errProviderUnavailable
	}

	if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
		switch pe.StatusCode {
		case http.StatusBadRequest:
			return errUpstreamRejected
		case http.StatusTooManyRequests:
			return errProviderRateLimited
		default:
			return errUpstream
		}
	}
	return errInternal
}
