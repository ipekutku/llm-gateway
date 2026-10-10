package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/postgres"
	"github.com/ipekutku/llm-gateway/internal/usage"
	"github.com/jackc/pgx/v5"
)

func newAccountingDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI must set GATEWAY_TEST_DATABASE_URL for accounting integration tests")
		}
		t.Skip("set GATEWAY_TEST_DATABASE_URL to run the accounting integration test")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("GATEWAY_TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := "gateway_accounting_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		_ = admin.Close(ctx)
	})
	u.Path = "/" + name
	return u.String()
}

func TestUsageAccountingWithPostgres(t *testing.T) {
	database := newAccountingDatabase(t)
	ctx, deadlineCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer deadlineCancel()
	logger := slog.New(slog.DiscardHandler)
	vars := map[string]string{databaseURLVar: database, pricingFileVar: pricingPath, clientsFileVar: clientsPath, "OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey}
	readFile := files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[` + priceEntry + `]}`})
	openStore := func(ctx context.Context, url string) (accountingStore, error) { return postgres.Open(ctx, url) }
	// Serving never auto-migrates an empty database.
	if err := runWith(ctx, env(vars), readFile, openStore, logger); err == nil || !strings.Contains(err.Error(), "not migrated") {
		t.Fatalf("startup on empty database = %v", err)
	}
	// The migration command needs no provider, client, or pricing settings.
	for range 2 {
		if err := execute(ctx, []string{"migrate"}, env(map[string]string{databaseURLVar: database}), logger); err != nil {
			t.Fatal(err)
		}
	}
	store, err := postgres.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	recorder, err := usage.NewRecorder(store, logger, usage.DefaultRecorderOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		if err := recorder.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	oa := newUpstream(t, "/v1/chat/completions", reply(200, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(200, anthropicReply))
	cfg := gatewayConfig(oa, an)
	cfg.Pricing, err = loadPricing(env(vars), readFile)
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(cfg, nil, recorder, nil, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	ids := make([]string, 0, 2)
	for _, model := range []string{"gpt-4o", "unknown"} {
		resp, _, err := postChat(t, ctx, gw.URL, chatBody(model))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, resp.Header.Get(httpapi.RequestIDHeader))
	}
	gw.Close()
	drainCtx, cancel := context.WithTimeout(ctx, guard)
	defer cancel()
	if err := recorder.Close(drainCtx); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var client, provider, model, cost string
	var status, input, output int
	if err := conn.QueryRow(ctx, `SELECT client_id,provider,model,status,input_tokens,output_tokens,cost_usd::text FROM usage_records WHERE request_id=$1`, ids[0]).Scan(&client, &provider, &model, &status, &input, &output, &cost); err != nil {
		t.Fatal(err)
	}
	if client != "team-a" || provider != "openai" || model != "gpt-4o-2024-08-06" || status != 200 || input != 12 || output != 3 || cost != "0.000060000000" {
		t.Errorf("stored success: %s %s %s %d %d %d %s", client, provider, model, status, input, output, cost)
	}
	var code string
	var usageUnknown, costUnknown bool
	if err := conn.QueryRow(ctx, `SELECT status,error_code,input_tokens IS NULL,cost_usd IS NULL FROM usage_records WHERE request_id=$1`, ids[1]).Scan(&status, &code, &usageUnknown, &costUnknown); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusNotFound || code != "model_not_found" || !usageUnknown || !costUnknown {
		t.Errorf("stored failure: %d %s %v %v", status, code, usageUnknown, costUnknown)
	}
}
