package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/usage"
	"go.opentelemetry.io/otel/trace"
)

type fakeStartupStore struct {
	schemaErr error
	closed    bool
}

func (s *fakeStartupStore) CheckSchema(context.Context) error            { return s.schemaErr }
func (s *fakeStartupStore) Close()                                       { s.closed = true }
func (s *fakeStartupStore) Insert(context.Context, []usage.Record) error { return nil }

func runWithFakeDatabase(ctx context.Context, vars map[string]string, logger *slog.Logger) error {
	vars[databaseURLVar], vars[pricingFileVar] = testDatabaseURL, pricingPath
	return runWith(ctx, env(vars), func(path string) ([]byte, error) {
		if path == pricingPath {
			return []byte(`{"prices":[]}`), nil
		}
		return os.ReadFile(path)
	}, func(context.Context, string, trace.TracerProvider) (accountingStore, error) {
		return &fakeStartupStore{}, nil
	}, logger)
}

func TestAccountingConfigurationIsRequired(t *testing.T) {
	for _, name := range []string{databaseURLVar, pricingFileVar} {
		_, err := load(map[string]string{"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, name: " "})
		if err == nil || !strings.Contains(err.Error(), name+" is required") {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

const priceEntry = `{"provider":"openai","model":"gpt-4o","input":"2.5","cache_read":"1.25","cache_write":"0","output":"10"}`

func TestLoadPricing(t *testing.T) {
	pricing, err := loadPricing(env(map[string]string{pricingFileVar: pricingPath}), files(map[string]string{pricingPath: `{"prices":[` + priceEntry + `]}`}))
	if err != nil {
		t.Fatal(err)
	}
	cost, err := pricing.Cost(usage.Model{Provider: "openai", Model: "gpt-4o"}, llm.Usage{InputTokens: 100, CacheReadInputTokens: 20, OutputTokens: 10})
	if err != nil || cost != usage.Cost(325_000_000) {
		t.Errorf("cost = %v, %v; want $0.000325", cost, err)
	}
}

func TestLoadPricingRejectsMalformedFiles(t *testing.T) {
	for name, data := range map[string]string{
		"missing array": `{}`, "null": `null`, "trailing": `{"prices":[]} {}`, "unknown field": `{"prices":[],"secret":"sensitive-value"}`,
		"duplicate":                `{"prices":[` + priceEntry + `,` + priceEntry + `]}`,
		"missing rate":             `{"prices":[{"provider":"openai","model":"gpt-4o"}]}`,
		"invalid price":            strings.Replace(`{"prices":[`+priceEntry+`]}`, `"2.5"`, `"sensitive-value"`, 1),
		"too many decimals":        strings.Replace(`{"prices":[`+priceEntry+`]}`, `"2.5"`, `"0.1234567"`, 1),
		"number instead of string": strings.Replace(`{"prices":[`+priceEntry+`]}`, `"2.5"`, `2.5`, 1),
		"blank provider":           strings.Replace(`{"prices":[`+priceEntry+`]}`, `"openai"`, `" "`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadPricing(env(map[string]string{pricingFileVar: pricingPath}), files(map[string]string{pricingPath: data}))
			if err == nil || !strings.Contains(err.Error(), pricingFileVar) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "sensitive-value") {
				t.Errorf("configuration value leaked: %v", err)
			}
		})
	}
	if _, err := loadPricing(env(map[string]string{pricingFileVar: pricingPath}), files(nil)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file error = %v, want preserved filesystem cause", err)
	}
}

func TestWarnPricingGaps(t *testing.T) {
	const prices = `{"prices":[
		{"provider":"openai","model":"gpt-4o","input":"2.5","cache_read":"1.25","cache_write":"0","output":"10"},
		{"provider":"anthropic","model":"claude-opus-5.5","input":"5","cache_read":"0.5","cache_write":"6.25","output":"25"}
	]}`
	cfg, err := loadConfig(env(map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
		"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
		clientsFileVar: clientsPath, databaseURLVar: testDatabaseURL, pricingFileVar: pricingPath,
	}), files(map[string]string{clientsPath: testClientsFile, pricingPath: prices}))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnPricingGaps(cfg, slog.New(slog.NewTextHandler(&logs, nil)))

	// The Anthropic price has a typo: a dot instead of a hyphen.
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d warnings, want 2:\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], "not configured") || !strings.Contains(lines[0], "model=claude-opus-5.5") {
		t.Errorf("first warning = %q, want the unused price", lines[0])
	}
	if !strings.Contains(lines[1], "will be unknown") || !strings.Contains(lines[1], "model=claude-opus-5-5") {
		t.Errorf("second warning = %q, want the unpriced configured model", lines[1])
	}
	if strings.Contains(logs.String(), "gpt-4o") {
		t.Errorf("warned about the correctly priced model:\n%s", logs.String())
	}
}

func TestWarnPricingGapsQuietWhenComplete(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
		clientsFileVar: clientsPath, databaseURLVar: testDatabaseURL, pricingFileVar: pricingPath,
	}), files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[` + priceEntry + `]}`}))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnPricingGaps(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Errorf("warnings for complete pricing:\n%s", logs.String())
	}
}

func TestStartupRequiresReachableCurrentDatabase(t *testing.T) {
	vars := map[string]string{"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, clientsFileVar: clientsPath, databaseURLVar: testDatabaseURL, pricingFileVar: pricingPath}
	readFile := files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[]}`})
	store := &fakeStartupStore{schemaErr: errors.New("database is not migrated")}
	err := runWith(t.Context(), env(vars), readFile, func(context.Context, string, trace.TracerProvider) (accountingStore, error) { return store, nil }, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "database schema") || !store.closed {
		t.Errorf("run = %v; store closed = %v", err, store.closed)
	}
	err = runWith(t.Context(), env(vars), readFile, func(context.Context, string, trace.TracerProvider) (accountingStore, error) {
		return nil, errors.New("unreachable")
	}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "database: unreachable") {
		t.Errorf("run = %v", err)
	}
}

func TestCommandArguments(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	if err := execute(t.Context(), []string{"migrate"}, env(nil), logger); err == nil || !strings.Contains(err.Error(), databaseURLVar) {
		t.Errorf("migrate = %v", err)
	}
	if err := execute(t.Context(), []string{"invalid"}, env(nil), logger); err == nil || err.Error() != "usage: gateway [migrate]" {
		t.Errorf("unknown command = %v", err)
	}
}
