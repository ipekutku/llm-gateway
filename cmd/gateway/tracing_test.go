package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestTracingDiagnosticsExcludeCollectorResponse(t *testing.T) {
	const secret = "COLLECTOR-RESPONSE-MARKER-248cbd"
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, secret)
	}))
	t.Cleanup(collector.Close)
	t.Setenv(otlpEndpointVar, collector.URL)
	t.Setenv(otlpTracesEndpointVar, "")
	previous := otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetErrorHandler(previous) })
	var logs syncBuffer
	tp, err := newTracerProvider(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("test").Start(t.Context(), "test export")
	span.End()
	// Shutdown flushes the batch. The SDK reports export failures through
	// the error handler; shutdown itself need not return an error.
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()
	err = tp.Shutdown(ctx)
	if strings.Contains(fmt.Sprint(err, logs.String()), secret) {
		t.Error("tracing diagnostic contains the collector response")
	}
	if !strings.Contains(logs.String(), "tracing error") {
		t.Fatal("failed export did not produce a warning")
	}
}

// spanCollector keeps every ended span and signals each ended server span,
// so a test can wait until a request's trace is complete.
type spanCollector struct {
	mu      sync.Mutex
	spans   []sdktrace.ReadOnlySpan
	servers chan struct{}
}

func newSpanCollector() *spanCollector {
	return &spanCollector{servers: make(chan struct{}, 100)}
}

func (c *spanCollector) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (c *spanCollector) Shutdown(context.Context) error                  { return nil }
func (c *spanCollector) ForceFlush(context.Context) error                { return nil }

func (c *spanCollector) OnEnd(s sdktrace.ReadOnlySpan) {
	c.mu.Lock()
	c.spans = append(c.spans, s)
	c.mu.Unlock()
	if s.SpanKind() == trace.SpanKindServer {
		c.servers <- struct{}{}
	}
}

// serverSpan waits for the next server span to end and returns it. The
// server span ends last, so the request's whole trace is then collected.
func (c *spanCollector) serverSpan(t *testing.T) sdktrace.ReadOnlySpan {
	t.Helper()
	select {
	case <-c.servers:
	case <-time.After(guard):
		t.Fatal("timed out waiting for the server span")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range slices.Backward(c.spans) {
		if s.SpanKind() == trace.SpanKindServer {
			return s
		}
	}
	panic("unreachable")
}

// trace returns the spans of traceID.
func (c *spanCollector) trace(traceID trace.TraceID) []sdktrace.ReadOnlySpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	var spans []sdktrace.ReadOnlySpan
	for _, s := range c.spans {
		if s.SpanContext().TraceID() == traceID {
			spans = append(spans, s)
		}
	}
	return spans
}

// tree renders the span tree below root, one span per line, indented by
// depth, with its events in brackets and " !" for an error status.
// Siblings are in start order.
func tree(root sdktrace.ReadOnlySpan, spans []sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	var render func(s sdktrace.ReadOnlySpan, depth int)
	render = func(s sdktrace.ReadOnlySpan, depth int) {
		b.WriteString(strings.Repeat("  ", depth) + s.Name())
		if len(s.Events()) > 0 {
			var names []string
			for _, e := range s.Events() {
				names = append(names, e.Name)
			}
			b.WriteString(" [" + strings.Join(names, " ") + "]")
		}
		if s.Status().Code == codes.Error {
			b.WriteString(" !")
		}
		b.WriteString("\n")
		var children []sdktrace.ReadOnlySpan
		for _, c := range spans {
			if c.Parent().SpanID() == s.SpanContext().SpanID() {
				children = append(children, c)
			}
		}
		slices.SortFunc(children, func(a, b sdktrace.ReadOnlySpan) int { return a.StartTime().Compare(b.StartTime()) })
		for _, c := range children {
			render(c, depth+1)
		}
	}
	render(root, 0)
	return b.String()
}

// attrs returns the attributes of s as strings, by key.
func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := make(map[string]string)
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// tracedGateway is gatewayWith with tracing to an in-memory collector and
// JSON logs.
func tracedGateway(t *testing.T, oa, an *upstream, edit func(*config)) (*httptest.Server, *spanCollector, *syncBuffer) {
	t.Helper()
	cfg := gatewayConfig(oa, an)
	edit(&cfg)
	spans := newSpanCollector()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	logs := new(syncBuffer)
	logger, err := newLogger(logs, env(map[string]string{logFormatVar: "json"}))
	if err != nil {
		t.Fatalf("newLogger() error = %v", err)
	}
	h, err := newHandler(cfg, nil, discardRecorder{}, nil, tp, logger)
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw, spans, logs
}

func TestRequestPathSpanTree(t *testing.T) {
	tests := []struct {
		name     string
		oa       http.HandlerFunc
		fallback bool
		model    string
		key      string
		want     string
	}{
		{
			name: "success",
			oa:   reply(http.StatusOK, openaiReply),
			want: `POST /v1/chat/completions
  route
    provider openai
      chat gpt-4o
`,
		},
		{
			name: "retries",
			oa:   sequence(openaiReply, "", 503, 503, 200),
			want: `POST /v1/chat/completions
  route
    provider openai [retry retry]
      chat gpt-4o !
      chat gpt-4o !
      chat gpt-4o
`,
		},
		{
			name:     "fallback",
			oa:       reply(http.StatusInternalServerError, openaiReply),
			fallback: true,
			want: `POST /v1/chat/completions
  route [fallback]
    provider openai !
      chat gpt-4o !
    provider anthropic
      chat claude-opus-5-5
`,
		},
		{
			name:  "unknown model",
			oa:    reply(http.StatusOK, openaiReply),
			model: "gpt-5",
			want: `POST /v1/chat/completions
  route !
`,
		},
		{
			name: "rejected before routing",
			oa:   reply(http.StatusOK, openaiReply),
			key:  "Bearer not-a-gateway-key",
			want: `POST /v1/chat/completions
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oa := newUpstream(t, "/v1/chat/completions", tt.oa)
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			gw, spans, _ := tracedGateway(t, oa, an, func(c *config) {
				if tt.fallback {
					c.OpenAI.FallbackTo = "anthropic"
				}
			})
			model, key := orDefault(tt.model, "gpt-4o"), orDefault(tt.key, "Bearer "+clientKey)
			if _, _, err := postChatAs(t, context.Background(), gw.URL, key, chatBody(model)); err != nil {
				t.Fatalf("POST error = %v", err)
			}
			server := spans.serverSpan(t)
			if got := tree(server, spans.trace(server.SpanContext().TraceID())); got != tt.want {
				t.Errorf("span tree:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// orDefault returns s, or def if s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func TestRequestPathSpanAttributes(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", sequence(openaiReply, "", 503, 200))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw, spans, _ := tracedGateway(t, oa, an, func(*config) {})

	resp, _, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	server := spans.serverSpan(t)
	all := spans.trace(server.SpanContext().TraceID())

	want := map[string]map[string]string{
		"POST /v1/chat/completions": {
			"http.request.method": "POST", "http.route": "/v1/chat/completions", "http.response.status_code": "200",
			"gateway.request_id": resp.Header.Get(httpapi.RequestIDHeader), "gateway.client_id": "team-a",
			"gen_ai.request.model": "gpt-4o", "gateway.retry_count": "1", "gateway.fallback": "false",
		},
		"route":           {"gen_ai.request.model": "gpt-4o", "gen_ai.provider.name": "openai"},
		"provider openai": {"gen_ai.request.model": "gpt-4o", "gen_ai.provider.name": "openai"},
	}
	var attempts []map[string]string
	for _, s := range all {
		if s.Name() == "provider openai" {
			events := s.Events()
			if len(events) != 1 || events[0].Name != "retry" {
				t.Fatalf("provider span events = %v, want one retry", events)
			}
			got := make(map[string]string)
			for _, kv := range events[0].Attributes {
				got[string(kv.Key)] = kv.Value.Emit()
			}
			if got["gateway.retry.failed_attempt"] != "1" || got["http.response.status_code"] != "503" || got["gateway.retry.delay_ms"] == "" {
				t.Errorf("retry event attributes = %v", got)
			}
		}
		if s.Name() == "chat gpt-4o" {
			if s.SpanKind() != trace.SpanKindClient {
				t.Errorf("attempt span kind = %v, want client", s.SpanKind())
			}
			attempts = append(attempts, attrs(s))
			continue
		}
		for k, v := range want[s.Name()] {
			if got := attrs(s)[k]; got != v {
				t.Errorf("%s: %s = %q, want %q", s.Name(), k, got, v)
			}
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("got %d attempt spans, want 2", len(attempts))
	}
	// Spans end in order, so the failed attempt comes first.
	for k, v := range map[string]string{"http.response.status_code": "503", "error.type": "503", "gen_ai.provider.name": "openai"} {
		if attempts[0][k] != v {
			t.Errorf("failed attempt: %s = %q, want %q", k, attempts[0][k], v)
		}
	}
	for k, v := range map[string]string{
		"gen_ai.response.model": "gpt-4o-2024-08-06", "gen_ai.usage.input_tokens": "12", "gen_ai.usage.output_tokens": "3",
		"gen_ai.response.finish_reasons": `["stop"]`,
	} {
		if attempts[1][k] != v {
			t.Errorf("successful attempt: %s = %q, want %q", k, attempts[1][k], v)
		}
	}
}

func TestRequestPathTraceContext(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		prompt       = "PROMPT-MARKER-7f3a"
	)
	upstreamBody := strings.Replace(openaiReply, "from openai", "COMPLETION-MARKER-91c2", 1)
	oa := newUpstream(t, "/v1/chat/completions", sequence(upstreamBody, "", 503, 200))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw, spans, logs := tracedGateway(t, oa, an, func(*config) {})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + prompt + `"}]}`
	req, err := http.NewRequest(http.MethodPost, gw.URL+httpapi.ChatCompletionsPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("traceparent", "00-"+traceID+"-"+parentSpanID+"-01")
	req.Header.Set("tracestate", "vendor=value")
	req.Header.Set("baggage", "user=someone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	server := spans.serverSpan(t)

	// The client's trace continues in the gateway.
	if got := server.SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace ID = %s, want the incoming %s", got, traceID)
	}
	if got := server.Parent(); got.SpanID().String() != parentSpanID || !got.IsRemote() {
		t.Errorf("server span parent = %v, want the remote span %s", got.SpanID(), parentSpanID)
	}

	// But trace context never goes to a provider.
	received := oa.received()
	if len(received) != 2 {
		t.Fatalf("upstream received %d requests, want 2", len(received))
	}
	for _, r := range received {
		for _, h := range []string{"Traceparent", "Tracestate", "Baggage"} {
			if v := r.Header.Get(h); v != "" {
				t.Errorf("upstream request carries %s: %q", h, v)
			}
		}
	}

	// Every log line of the request carries its trace ID.
	lines := 0
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		lines++
		if rec["trace_id"] != traceID {
			t.Errorf("log line %q has trace_id %v, want %s", rec["msg"], rec["trace_id"], traceID)
		}
	}
	if lines < 2 {
		t.Errorf("got %d log lines, want the retry and the outcome", lines)
	}

	// Spans contain no content or credentials.
	var dump strings.Builder
	for _, s := range spans.trace(server.SpanContext().TraceID()) {
		fmt.Fprintln(&dump, s.Name(), s.Status().Description, attrs(s))
		for _, e := range s.Events() {
			fmt.Fprintln(&dump, e.Name, e.Attributes)
		}
	}
	for _, secret := range []string{prompt, "COMPLETION-MARKER-91c2", clientKey, openaiKey, anthropicKey, "someone"} {
		if strings.Contains(dump.String(), secret) {
			t.Errorf("spans contain %q:\n%s", secret, dump.String())
		}
	}
}

func TestRunExportsSpansOverOTLP(t *testing.T) {
	type export struct {
		contentType string
		body        string
	}
	exports := make(chan export, 10)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			exports <- export{r.Header.Get("Content-Type"), string(body)}
		}
	}))
	t.Cleanup(collector.Close)
	// The exporter reads its endpoint from the process environment.
	t.Setenv(otlpEndpointVar, collector.URL)

	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runWithFakeDatabase(ctx, map[string]string{
			"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
			"GATEWAY_ADDR": "127.0.0.1:0", "GATEWAY_METRICS_ADDR": "127.0.0.1:0", clientsFileVar: writeClientsFile(t),
			otlpEndpointVar: collector.URL,
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	deadline := time.After(guard)
	var match []string
	for match == nil {
		select {
		case err := <-result:
			t.Fatalf("run() returned early: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for the gateway to listen")
		case <-time.After(10 * time.Millisecond):
		}
		match = listenAddrs.FindStringSubmatch(logs.String())
	}
	if !strings.Contains(logs.String(), "tracing=true") {
		t.Errorf("startup log does not report tracing:\n%s", logs.String())
	}

	const key = "KEY-MARKER-5d1e"
	if resp, _, err := postChatAs(t, ctx, "http://"+match[1], "Bearer "+key, chatBody("claude-opus-5-5")); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("request = %v, %v; want 401", resp, err)
	}
	// Shutdown flushes the batched span before run returns.
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run() error = %v", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for shutdown")
	}
	select {
	case e := <-exports:
		if e.contentType != "application/x-protobuf" {
			t.Errorf("export Content-Type = %q", e.contentType)
		}
		for _, want := range []string{"POST /v1/chat/completions", "llm-gateway"} {
			if !strings.Contains(e.body, want) {
				t.Errorf("export does not contain %q", want)
			}
		}
		if strings.Contains(e.body, key) {
			t.Error("export contains the gateway key")
		}
	default:
		t.Fatal("no spans were exported by shutdown")
	}
}
