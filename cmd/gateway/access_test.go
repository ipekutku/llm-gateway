package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
)

// withClients configures two enabled clients, team-a (clientKey) and
// team-b, and a disabled one, all with the given limits.
func withClients(limits ratelimit.Limits) func(*config) {
	return func(cfg *config) {
		cfg.Clients = []auth.Client{
			{ID: "team-a", KeyHash: auth.HashKey(clientKey)},
			{ID: "team-b", KeyHash: auth.HashKey("gw-team-b-key")},
			{ID: "team-old", KeyHash: auth.HashKey("gw-team-old-key"), Disabled: true},
		}
		cfg.RateLimits = map[string]ratelimit.Limits{"team-a": limits, "team-b": limits, "team-old": limits}
	}
}

func TestRequestPathRejectsUnauthenticatedRequests(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, withClients(defaultLimits))

	for _, tt := range []struct {
		name          string
		authorization string
		code          string
	}{
		{"anonymous", "", "missing_api_key"},
		{"malformed header", "Token " + clientKey, "missing_api_key"},
		{"unknown key", "Bearer gw-not-a-key", "invalid_api_key"},
		{"provider key", "Bearer " + openaiKey, "invalid_api_key"},
		{"disabled key", "Bearer gw-team-old-key", "invalid_api_key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp, data, err := postChatAs(t, context.Background(), gw.URL, tt.authorization, chatBody("gpt-4o"))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body %s)", resp.StatusCode, data)
			}
			if got := errorCode(t, data); got != tt.code {
				t.Errorf("code = %q, want %q", got, tt.code)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("WWW-Authenticate header missing")
			}
		})
	}
	if n := len(oa.received()) + len(an.received()); n != 0 {
		t.Errorf("upstreams received %d requests, want 0", n)
	}
}

func TestRequestPathKeepsCredentialsSeparate(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, withClients(defaultLimits))

	for _, model := range []string{"gpt-4o", "claude-opus-5-5"} {
		resp, data, err := postChat(t, context.Background(), gw.URL, chatBody(model))
		if err != nil {
			t.Fatalf("POST error = %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, data)
		}
		for _, secret := range []string{openaiKey, anthropicKey} {
			if strings.Contains(string(data), secret) || strings.Contains(strings.Join(headerValues(resp.Header), " "), secret) {
				t.Errorf("response exposes a provider key")
			}
		}
	}

	// Each upstream gets its own provider key and never the client's key.
	for _, u := range []struct {
		up     *upstream
		header string
		want   string
	}{
		{oa, "Authorization", "Bearer " + openaiKey},
		{an, "X-Api-Key", anthropicKey},
	} {
		got := u.up.received()
		if len(got) != 1 {
			t.Fatalf("upstream received %d requests, want 1", len(got))
		}
		if v := got[0].Header.Get(u.header); v != u.want {
			t.Errorf("%s = %q, want the provider key", u.header, v)
		}
		if strings.Contains(strings.Join(headerValues(got[0].Header), " "), clientKey) {
			t.Error("the gateway client's key was forwarded upstream")
		}
	}
}

func headerValues(h http.Header) []string {
	var all []string
	for _, vs := range h {
		all = append(all, vs...)
	}
	return all
}

func TestRequestPathRateLimitsEachClient(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, withClients(ratelimit.Limits{RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 5}))

	post := func(key string) (*http.Response, []byte) {
		t.Helper()
		resp, data, err := postChatAs(t, context.Background(), gw.URL, "Bearer "+key, chatBody("gpt-4o"))
		if err != nil {
			t.Fatalf("POST error = %v", err)
		}
		return resp, data
	}

	for i := range 2 {
		if resp, data := post(clientKey); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (body %s)", i+1, resp.StatusCode, data)
		}
	}
	resp, data := post(clientKey)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "rate_limit_exceeded" {
		t.Errorf("code = %q, want rate_limit_exceeded", got)
	}
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}

	// team-b's limit is untouched by team-a's requests.
	for i := range 2 {
		if resp, data := post("gw-team-b-key"); resp.StatusCode != http.StatusOK {
			t.Errorf("team-b request %d: status = %d, want 200 (body %s)", i+1, resp.StatusCode, data)
		}
	}
	if n := len(oa.received()); n != 4 {
		t.Errorf("upstream received %d requests, want 4: rate-limited requests must not reach it", n)
	}
}
