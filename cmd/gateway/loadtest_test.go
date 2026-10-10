package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

const loadtestDir = "../../deploy/loadtest"

// Keep development fixtures checked against the real configuration contract;
// these tests do not start load, access a database, or install Vegeta.
func TestLoadtestFixtures(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GATEWAY_CLIENTS_FILE": loadtestDir + "/clients.json",
		"GATEWAY_PRICING_FILE": loadtestDir + "/prices.json",
		"GATEWAY_DATABASE_URL": testDatabaseURL,
		"OPENAI_MODEL":         "local-openai", "OPENAI_API_KEY": openaiKey,
		"ANTHROPIC_MODEL": "local-anthropic", "ANTHROPIC_API_KEY": anthropicKey,
	}), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 1 {
		t.Fatalf("fixture clients = %d, want one", len(cfg.Clients))
	}
	a, err := auth.New(cfg.Clients)
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.Authenticate("synthetic-loadtest-key")
	if err != nil || id.ClientID != "loadtest" {
		t.Fatalf("fixture authentication = %v, %v", id, err)
	}
	if got := cfg.RateLimits[id.ClientID]; got != (ratelimit.Limits{
		RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: 10000,
	}) {
		t.Fatalf("fixture limits = %+v", got)
	}
	for _, provider := range []string{"openai", "anthropic"} {
		if cost, err := cfg.Pricing.Cost(usage.Model{Provider: provider, Model: "local-" + provider}, llm.Usage{InputTokens: 12, OutputTokens: 3}); err != nil || cost != 18_000_000 {
			t.Fatalf("%s synthetic cost = %v, %v", provider, cost, err)
		}
		body, err := os.ReadFile(loadtestDir + "/" + provider + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct{ Role, Content string }
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "local-"+provider || req.MaxTokens != 32 || len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "synthetic prompt" {
			t.Fatalf("invalid %s request fixture", provider)
		}
		for _, path := range []string{"baseline", "gateway"} {
			target, err := os.ReadFile(loadtestDir + "/" + path + "-" + provider + ".targets")
			if err != nil {
				t.Fatal(err)
			}
			url := "POST http://127.0.0.1:FAKE_PORT/v1/chat/completions"
			if provider == "anthropic" {
				url = "POST http://127.0.0.1:FAKE_PORT/v1/messages"
			}
			if path == "gateway" {
				url = "POST http://127.0.0.1:GATEWAY_PORT/v1/chat/completions"
			}
			lines := []string{url, "Content-Type: application/json"}
			if path == "gateway" {
				lines = append(lines, "Authorization: Bearer synthetic-loadtest-key")
			}
			lines = append(lines, "@deploy/loadtest/"+provider+".json")
			if strings.TrimSpace(string(target)) != strings.Join(lines, "\n") {
				t.Fatalf("unexpected target: %s/%s", path, provider)
			}
		}
	}
}

func loadtestCommand(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("development load-test checks require %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, name, args...)
}

func TestLoadtestScriptValidation(t *testing.T) {
	if out, err := loadtestCommand(t, "bash", "-n", loadtestDir+"/run.sh").CombinedOutput(); err != nil {
		t.Fatalf("script syntax: %v\n%s", err, out)
	}
	if out, err := loadtestCommand(t, "bash", "-n", loadtestDir+"/binary-size.sh").CombinedOutput(); err != nil {
		t.Fatalf("size script syntax: %v\n%s", err, out)
	}
	for _, tt := range []struct {
		name, setting, want string
	}{
		{"missing database", "", "Set GATEWAY_DATABASE_URL"},
		{"provider", "LOADTEST_PROVIDER=remote", "LOADTEST_PROVIDER must be"},
		{"Redis remote", "LOADTEST_REDIS_URL=redis://:synthetic-secret-marker@remote:6379/0", "LOADTEST_REDIS_URL must name"},
		{"Redis port", "LOADTEST_REDIS_URL=redis://127.0.0.1:99999/0", "Redis port must be"},
		{"scenario", "LOADTEST_SCENARIO=invalid", "LOADTEST_SCENARIO must be"},
		{"missing open rate", "LOADTEST_SCENARIO=C", "LOADTEST_OPEN_RATE must be"},
		{"unused open rate", "LOADTEST_OPEN_RATE=100", "LOADTEST_OPEN_RATE is only allowed"},
		{"seconds", "LOADTEST_SECONDS=not-a-number", "LOADTEST_SECONDS must be"},
		{"profile", "LOADTEST_SECONDS=5", "LOADTEST_PROFILE_SECONDS must be less"},
		{"workers", "LOADTEST_WORKERS=10001", "LOADTEST_WORKERS must be"},
		{"port", "LOADTEST_FAKE_PORT=https://remote", "Load-test port must be"},
		{"same ports", "LOADTEST_FAKE_PORT=18080", "Load-test ports must differ"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := loadtestCommand(t, "bash", loadtestDir+"/run.sh")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			if tt.setting != "" {
				cmd.Env = append(cmd.Env, "GATEWAY_DATABASE_URL=synthetic-secret-marker", tt.setting)
			}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tt.want) || strings.Contains(string(out), "synthetic-secret-marker") {
				t.Fatalf("validation: %v\n%s", err, out)
			}
		})
	}
}

func TestLoadtestRedisAndConnectionSummary(t *testing.T) {
	report := `{"latencies":{"50th":1,"95th":2,"99th":3},"requests":100,"rate":100,"throughput":100,"success":1,"status_codes":{"200":100}}`
	common := "process_cpu_seconds_total 1\ngo_gc_duration_seconds_count 1\ngo_gc_duration_seconds_sum 1\ngo_memstats_alloc_bytes_total 1\n"
	store := func(count int, sum float64) string {
		text := "gateway_ratelimit_store_up 1\n"
		for _, op := range []string{"acquire", "release"} {
			text += fmt.Sprintf("gateway_ratelimit_store_call_duration_seconds_count{operation=%q} %d\ngateway_ratelimit_store_call_duration_seconds_sum{operation=%q} %g\ngateway_ratelimit_store_errors_total{operation=%q} 0\n", op, count, op, sum, op)
		}
		return text
	}
	for _, tt := range []struct{ name, change, want string }{
		{"valid", "", "Redis release: 100 calls; mean 1.000 ms; errors 0"},
		{"missing calls", "missing", "Missing metric"},
		{"reset", "reset", "Metric counter decreased"},
		{"failed release", "error", "Missing/failed Redis calls"},
		{"degraded", "mode", "Redis degraded"},
		{"missing connection count", "connections", "Missing accepted-connection count"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			after, samples := common+store(110, 0.11), "process_resident_memory_bytes 1\ngo_goroutines 1\ngateway_ratelimit_store_up 1\n"
			connection := "fake provider stopped connections=501\n"
			switch tt.change {
			case "missing":
				after = common
			case "reset":
				after = common + store(1, 0.001)
			case "error":
				after = strings.Replace(after, "gateway_ratelimit_store_errors_total{operation=\"release\"} 0", "gateway_ratelimit_store_errors_total{operation=\"release\"} 1", 1)
			case "mode":
				samples += "gateway_ratelimit_store_up 0\n"
			case "connections":
				connection = ""
			}
			for name, text := range map[string]string{"baseline.json": report, "gateway.json": report, "before.prom": common + store(10, 0.01), "after.prom": after, "samples.prom": samples, "gateway.log": "", "fake-baseline.log": connection, "fake.log": connection} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := loadtestCommand(t, "awk", "-v", "dir="+dir, "-v", "shared=1", "-v", "connections=1", "-f", loadtestDir+"/summary.awk").CombinedOutput()
			if (err != nil) != (tt.change != "") || !strings.Contains(string(out), tt.want) {
				t.Fatalf("summary: %v\n%s", err, out)
			}
			if tt.change == "" && !strings.Contains(string(out), "direct 501; gateway upstream 501") {
				t.Fatalf("missing counts: %s", out)
			}
		})
	}
}

func TestBinarySizeSymbolAttribution(t *testing.T) {
	dir := t.TempDir()
	packages := filepath.Join(dir, "packages.txt")
	symbols := filepath.Join(dir, "symbols.txt")
	if err := os.WriteFile(packages, []byte("net stdlib\nnet/http stdlib\ngithub.com/example/module dep\ngithub.com/example/module/sub dep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(symbols, []byte("100 10 T net/http.Func\n100 10 T net/http.Alias\n110 20 T github.com/example/module/sub.Func\n130 30 T github.com/example/module-other.Func\n160 8388608 B net.bigZeroMemory\n170 99 R net.metadata\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := loadtestCommand(t, "awk", "-f", loadtestDir+"/symbols.awk", packages, symbols).CombinedOutput()
	if err != nil {
		t.Fatalf("symbols: %v %s", err, out)
	}
	for _, want := range []string{"10 stdlib\n", "20 dep\n", "30 unassigned\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if len(strings.Split(strings.TrimSpace(string(out)), "\n")) != 3 {
		t.Fatalf("unexpected attribution: %s", out)
	}
}

func TestLoadtestSummary(t *testing.T) {
	baseline := `{"latencies":{"50th":500000000,"95th":501000000,"99th":502000000},"requests":100,"rate":100,"throughput":99.5,"success":1,"status_codes":{"200":100}}`
	gateway := `{"latencies":{"50th":501000000,"95th":503000000,"99th":505000000},"requests":100,"rate":100,"throughput":99,"success":1,"status_codes":{"200":100}}`
	allFailed := strings.NewReplacer(`"success":1`, `"success":0`, `"200":100`, `"502":100`, `"throughput":99`, `"throughput":0`).Replace(gateway)
	before := "process_cpu_seconds_total 1\ngo_gc_duration_seconds_count 2\ngo_gc_duration_seconds_sum 0.001\ngo_memstats_alloc_bytes_total 1048576\n"
	after := "process_cpu_seconds_total 4\ngo_gc_duration_seconds_count 5\ngo_gc_duration_seconds_sum 0.004\ngo_memstats_alloc_bytes_total 3145728\n"
	for _, tt := range []struct {
		name, gateway, after, samples, rate, want string
		bad                                       bool
	}{
		{name: "valid", gateway: gateway, after: after, samples: "process_resident_memory_bytes 1.048576e+07\ngo_goroutines 501\nprocess_resident_memory_bytes 20971520\ngo_goroutines 900\n", want: "p50 1.000, p95 2.000, p99 3.000 ms"},
		{name: "errors", gateway: strings.Replace(gateway, `"success":1`, `"success":0.9`, 1), after: after, samples: "process_resident_memory_bytes 20971520\ngo_goroutines 900\n", want: "WARNING: HTTP errors"},
		{name: "non-200", gateway: strings.Replace(gateway, `"200":100`, `"200":90,"302":10`, 1), after: after, samples: "process_resident_memory_bytes 20971520\ngo_goroutines 900\n", want: "WARNING: HTTP errors"},
		{name: "all failed", gateway: allFailed, after: after, samples: "process_resident_memory_bytes 20971520\ngo_goroutines 900\n", want: "100.0000%"},
		{name: "missing metric", gateway: gateway, after: "", bad: true, want: "Missing metric"},
		{name: "missing report field", gateway: `{}`, after: after, bad: true, want: "Missing numeric report field"},
		{name: "missing samples", gateway: gateway, after: after, bad: true, want: "Missing memory/goroutine samples"},
		{name: "counter reset", gateway: gateway, after: strings.Replace(after, "process_cpu_seconds_total 4", "process_cpu_seconds_total 0", 1), bad: true, want: "Metric counter decreased"},
		{name: "open valid", gateway: gateway, after: after, samples: "process_resident_memory_bytes 20971520\ngo_goroutines 900\n", rate: "100", want: "Open target: 100 RPS"},
		{name: "open arrival missed", gateway: gateway, after: after, samples: "process_resident_memory_bytes 20971520\ngo_goroutines 900\n", rate: "200", bad: true, want: "Open arrival rate missed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, text := range map[string]string{
				"baseline.json": baseline, "gateway.json": tt.gateway, "before.prom": before, "after.prom": tt.after, "samples.prom": tt.samples,
				"gateway.log": "{\"msg\":\"usage records dropped: queue full\",\"count\":11}\n{\"msg\":\"usage records dropped: queue full\",\"count\":7}\n{\"msg\":\"usage records not persisted\",\"count\":100}\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := loadtestCommand(t, "awk", "-v", "dir="+dir, "-v", "rate="+tt.rate, "-f", loadtestDir+"/summary.awk")
			out, err := cmd.CombinedOutput()
			if (err != nil) != tt.bad || !strings.Contains(string(out), tt.want) {
				t.Fatalf("summary: %v\n%s", err, out)
			}
			if tt.name == "valid" {
				for _, want := range []string{"3.000 core-seconds", "20.00 MiB", "900", "3 cycles", "3.000 ms", "allocated: 2.00 MiB", "Queue-full drops: 18", "failed-batch records: 100"} {
					if !strings.Contains(string(out), want) {
						t.Errorf("summary missing %q: %s", want, out)
					}
				}
				cmd = loadtestCommand(t, "awk", "-v", "mode=rate", "-v", "file="+filepath.Join(dir, "gateway.json"), "-f", loadtestDir+"/summary.awk")
				if out, err := cmd.CombinedOutput(); err != nil || string(out) != "69\n" {
					t.Fatalf("70%% open rate: %v, %s", err, out)
				}
			}
		})
	}
}
