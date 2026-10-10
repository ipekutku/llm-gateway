package metrics

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

// dashboardPath is the provisioned Grafana dashboard of the local stack.
const dashboardPath = "../../deploy/observability/grafana/dashboards/llm-gateway.json"

var metricName = regexp.MustCompile(`\bgateway_[a-z_]+`)

// TestDashboardQueriesExportedMetrics keeps the dashboard in step with the
// metrics: every metric a panel queries must be one the gateway serves.
func TestDashboardQueriesExportedMetrics(t *testing.T) {
	data, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}

	// Give every metric a series, so all of them are in the scrape.
	m := newTestMetrics(t)
	m.ObserveRequest("gpt-4o", 200, "", time.Second)
	_, _ = m.Instrument("openai", returning(nil)).Chat(context.Background(), llm.ChatRequest{})
	_, _ = m.Instrument("openai", returning(&llm.ProviderError{Provider: "openai", StatusCode: 503})).Chat(context.Background(), llm.ChatRequest{})
	m.ObserveRetry("openai")
	m.ObserveFallback("openai", "anthropic")
	m.SetCircuitState("openai", breaker.Closed)
	cost := usage.Cost(1)
	m.ObserveUsage("openai", "gpt-4o", llm.Usage{InputTokens: 2, OutputTokens: 1}, &cost)
	text := scrape(t, m)

	queries := 0
	for _, p := range dashboard.Panels {
		for _, target := range p.Targets {
			queries++
			names := metricName.FindAllString(target.Expr, -1)
			if len(names) == 0 {
				t.Errorf("panel %q queries no gateway metric: %s", p.Title, target.Expr)
			}
			for _, name := range names {
				if len(seriesLines(text, name)) == 0 {
					t.Errorf("panel %q queries %s, which the gateway does not export", p.Title, name)
				}
			}
		}
	}
	if queries == 0 {
		t.Error("dashboard has no queries")
	}
}
