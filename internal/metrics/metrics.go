// Package metrics collects the gateway's Prometheus metrics in a private
// registry and serves them in the Prometheus text format.
//
// Label values are bounded: a model label is one of the configured model
// names or UnknownModel, so arbitrary model names sent by clients cannot
// create new series. Client IDs are never labels.
package metrics

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// UnknownModel is the model label of a request whose model is not
// configured or was never read, such as one rejected before its body.
const UnknownModel = "unknown"

// requestBuckets are the request duration histogram buckets, in seconds.
// They span quick rejections to the default 120s upstream budget.
var requestBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120}

// Metrics holds the gateway's collectors. It is safe for concurrent use.
type Metrics struct {
	registry *prometheus.Registry
	models   map[string]bool

	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
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
	)
	return m, nil
}

// ObserveRequest records one finished chat completion request. model is the
// requested model, or empty if it is unknown.
func (m *Metrics) ObserveRequest(model string, status int, code string, d time.Duration) {
	model = m.modelLabel(model)
	m.requests.WithLabelValues(model, strconv.Itoa(status), code).Inc()
	m.requestDuration.WithLabelValues(model).Observe(d.Seconds())
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
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelError),
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}
