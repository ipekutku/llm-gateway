package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// shortBodyReadTimeout makes the body read deadline expire quickly for the
// duration of the test.
func shortBodyReadTimeout(t *testing.T) time.Duration {
	t.Helper()
	old := bodyReadTimeout
	bodyReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { bodyReadTimeout = old })
	return bodyReadTimeout
}

// sendPartial opens a connection to srv and sends request headers
// announcing a body of contentLength bytes, followed by only the first part
// of it. The connection then stalls. A guard deadline bounds every read.
func sendPartial(t *testing.T, srv *httptest.Server, path, authorization string, contentLength int, part string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}
	head := "POST " + path + " HTTP/1.1\r\nHost: gateway\r\nContent-Type: application/json\r\n"
	if authorization != "" {
		head += "Authorization: " + authorization + "\r\n"
	}
	head += "Content-Length: " + strconv.Itoa(contentLength) + "\r\n\r\n"
	if _, err := io.WriteString(conn, head+part); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return conn
}

func TestSlowRequestBodyTimesOut(t *testing.T) {
	shortBodyReadTimeout(t)
	p := &recordingProvider{}
	h, logs := newHandler(t, p)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	conn := sendPartial(t, srv, ChatCompletionsPath, "Bearer "+keyA, len(validBody), validBody[:10])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("ReadResponse() error = %v (no response before the guard deadline)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408", resp.StatusCode)
	}
	var body errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Code != codeRequestTimeout || body.Error.Type != typeInvalidRequest {
		t.Errorf("error type/code = %q/%q, want %q/%q", body.Error.Type, body.Error.Code, typeInvalidRequest, codeRequestTimeout)
	}
	if p.calls != 0 {
		t.Errorf("provider called %d times, want 0", p.calls)
	}
	if !strings.Contains(logs.String(), "client_id=team-a") {
		t.Errorf("log does not name the client:\n%s", logs)
	}
}

func TestBodyReadDeadlineDoesNotLimitUpstreamWait(t *testing.T) {
	timeout := shortBodyReadTimeout(t)
	h, _ := newHandler(t, providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		// A slow upstream: answer only well after the body deadline would
		// have expired, unless the request is canceled first.
		select {
		case <-time.After(3 * timeout):
			return okProvider(ctx, req)
		case <-ctx.Done():
			return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: ctx.Err()}
		}
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+ChatCompletionsPath, strings.NewReader(validBody))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+keyA)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	// A canceled request writes nothing, which the server sends as an
	// empty 200, so the body must be checked too.
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"object":"chat.completion"`) {
		t.Errorf("status = %d, body %q; want a 200 completion", resp.StatusCode, data)
	}
}

func TestRejectedRequestWithStalledBodyIsAnswered(t *testing.T) {
	// Before a rejection is written, the server discards a small unread
	// body. Without a deadline a stalled body would hold the connection,
	// and the response, forever.
	for name, tc := range map[string]struct {
		path, authorization string
		status              int
	}{
		"unauthenticated": {ChatCompletionsPath, "", http.StatusUnauthorized},
		"invalid key":     {ChatCompletionsPath, "Bearer not-a-key", http.StatusUnauthorized},
		"unknown path":    {"/v1/other", "Bearer " + keyA, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			shortBodyReadTimeout(t)
			h, _ := newHandler(t, okProvider)
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			conn := sendPartial(t, srv, tc.path, tc.authorization, 1000, "{")
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("ReadResponse() error = %v (no response before the guard deadline)", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			// The rest of the body never came, so the connection cannot be
			// reused and must be closed.
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Errorf("connection after the response: err = %v, want io.EOF", err)
			}
		})
	}
}
