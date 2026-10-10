// Package metrics collects the gateway's Prometheus metrics in a private
// registry and serves them in the Prometheus text format.
//
// Label values are bounded: a model label is one of the configured model
// names or UnknownModel, so arbitrary model names sent by clients cannot
// create new series. Provider labels are the gateway's provider names, and
// upstream statuses are HTTP status codes. Client IDs are never labels.
//
// The methods of a nil *Metrics do nothing, so wiring can leave metrics out.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// UnknownModel is the model label of a request whose model is not
// configured or was never read, such as one rejected before its body.
const UnknownModel = "unknown"

// Outcomes of one upstream attempt, the outcome label of
// gateway_provider_requests_total.
const (
	OutcomeSuccess  = "success"
	OutcomeError    = "error"
	OutcomeTimeout  = "timeout"
	OutcomeCanceled = "canceled"
)

// requestBuckets are the request and attempt duration histogram buckets, in
// seconds. They span quick rejections to the default 120s upstream budget.
var requestBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120}

// Metrics holds the gateway's collectors. It is safe for concurrent use.
type Metrics struct {
	registry *prometheus.Registry
	models   map[string]bool

	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec

	providerRequests *prometheus.CounterVec
	providerDuration *prometheus.HistogramVec
	providerErrors   *prometheus.CounterVec
	retries          *prometheus.CounterVec
	fallbacks        *prometheus.CounterVec
	circuitState     *prometheus.GaugeVec
}

// New returns Metrics whose model labels are limited to models, the
// configured model names. Go runtime and process metrics are included.
func New(models []string) (*Metrics, error) {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		models:   make(map[string]bool, len(models)),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Chat completion requests by model, HTTP status, and gateway error code (empty on success). Status 499 means the client went away.",
		}, []string{"model", "status", "code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Time from receiving a chat completion request to finishing its response, by model.",
			Buckets: requestBuckets,
		}, []string{"model"}),
		providerRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_provider_requests_total",
			Help: "Upstream attempts by provider and outcome (success, error, timeout, canceled). Every retry and fallback attempt counts.",
		}, []string{"provider", "outcome"}),
		providerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_provider_request_duration_seconds",
			Help:    "Duration of upstream attempts by provider, all outcomes.",
			Buckets: requestBuckets,
		}, []string{"provider"}),
		providerErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_provider_errors_total",
			Help: "Failed upstream attempts by provider and upstream HTTP status; 0 means no usable response was received.",
		}, []string{"provider", "upstream_status"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_provider_retries_total",
			Help: "Repeated upstream attempts by provider.",
		}, []string{"provider"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_provider_fallbacks_total",
			Help: "Requests sent to a fallback provider after the primary provider failed.",
		}, []string{"from_provider", "to_provider"}),
		circuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_provider_circuit_state",
			Help: "Circuit breaker state by provider: 0 closed, 1 half-open, 2 open.",
		}, []string{"provider"}),
	}
	for _, model := range models {
		if strings.TrimSpace(model) == "" || model == UnknownModel {
			return nil, errors.New("metrics: model names must be non-blank and not " + strconv.Quote(UnknownModel))
		}
		m.models[model] = true
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests,
		m.requestDuration,
		m.providerRequests,
		m.providerDuration,
		m.providerErrors,
		m.retries,
		m.fallbacks,
		m.circuitState,
	)
	return m, nil
}

// ObserveRequest records one finished chat completion request. model is the
// requested model, or empty if it is unknown.
func (m *Metrics) ObserveRequest(model string, status int, code string, d time.Duration) {
	if m == nil {
		return
	}
	model = m.modelLabel(model)
	m.requests.WithLabelValues(model, strconv.Itoa(status), code).Inc()
	m.requestDuration.WithLabelValues(model).Observe(d.Seconds())
}

// Instrument returns p wrapped so that each call is counted as one
// upstream attempt of provider. Wrap each adapter directly, below retries,
// so every attempt is seen. The series of provider start at zero.
func (m *Metrics) Instrument(provider string, p llm.Provider) llm.Provider {
	if m == nil {
		return p
	}
	for _, outcome := range []string{OutcomeSuccess, OutcomeError, OutcomeTimeout, OutcomeCanceled} {
		m.providerRequests.WithLabelValues(provider, outcome)
	}
	m.retries.WithLabelValues(provider)
	m.circuitState.WithLabelValues(provider).Set(circuitValue(breaker.Closed))
	return instrumented{provider: provider, next: p, m: m}
}

// ObserveRetry records a repeated attempt to provider.
func (m *Metrics) ObserveRetry(provider string) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(provider).Inc()
}

// ObserveFallback records a request sent from provider from to the
// fallback provider to.
func (m *Metrics) ObserveFallback(from, to string) {
	if m == nil {
		return
	}
	m.fallbacks.WithLabelValues(from, to).Inc()
}

// AddFallbackRoute starts the fallback series of a configured fallback at
// zero, so rates are defined before the first fallback.
func (m *Metrics) AddFallbackRoute(from, to string) {
	if m == nil {
		return
	}
	m.fallbacks.WithLabelValues(from, to)
}

// SetCircuitState records the circuit breaker state of provider.
func (m *Metrics) SetCircuitState(provider string, s breaker.State) {
	if m == nil {
		return
	}
	m.circuitState.WithLabelValues(provider).Set(circuitValue(s))
}

// circuitValue orders states by severity: closed, half-open, open.
func circuitValue(s breaker.State) float64 {
	switch s {
	case breaker.Closed:
		return 0
	case breaker.HalfOpen:
		return 1
	default:
		return 2
	}
}

type instrumented struct {
	provider string
	next     llm.Provider
	m        *Metrics
}

func (p instrumented) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	start := time.Now()
	resp, err := p.next.Chat(ctx, req)
	p.m.providerDuration.WithLabelValues(p.provider).Observe(time.Since(start).Seconds())
	outcome := attemptOutcome(err)
	p.m.providerRequests.WithLabelValues(p.provider, outcome).Inc()
	if outcome == OutcomeError {
		status := 0
		if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
			status = pe.StatusCode
		}
		p.m.providerErrors.WithLabelValues(p.provider, strconv.Itoa(status)).Inc()
	}
	return resp, err
}

// attemptOutcome classifies an attempt's result. Context errors are
// checked first, so a timed-out or canceled attempt is not an error of the
// provider.
func attemptOutcome(err error) string {
	switch {
	case err == nil:
		return OutcomeSuccess
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return OutcomeCanceled
	default:
		return OutcomeError
	}
}

func (m *Metrics) modelLabel(model string) string {
	if m.models[model] {
		return model
	}
	return UnknownModel
}

// Handler serves the metrics in the Prometheus text format. Errors while
// gathering are logged to log and reported in the response.
func (m *Metrics) Handler(log *slog.Logger) http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelError),
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}
