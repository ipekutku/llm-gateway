package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxRequestBytes = 1 << 20
	// Fixed text and synthetic counts, not tokenization of a request or reply.
	completionText = "synthetic completion"
	openAIResponse = `{"id":"chatcmpl-fake","object":"chat.completion","created":0,"model":%s,` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"synthetic completion"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":0}}}`
	anthropicResponse = `{"id":"msg_fake","type":"message","role":"assistant","model":%s,` +
		`"content":[{"type":"text","text":"synthetic completion"}],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":12,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
)

type fakeProvider struct {
	handler     http.Handler
	delay       time.Duration
	connections atomic.Uint64
}

func newFakeProvider(delay time.Duration) *fakeProvider {
	f := &fakeProvider{delay: delay}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", f.respond(openAIResponse))
	mux.HandleFunc("POST /v1/messages", f.respond(anthropicResponse))
	f.handler = mux
	return f
}

func (f *fakeProvider) connState(_ net.Conn, state http.ConnState) {
	if state == http.StateNew {
		f.connections.Add(1)
	}
}

func (f *fakeProvider) respond(response string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only model and stream affect the fixture. Other fields are ignored;
		// this is not an emulator of provider request validation or generation.
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		err := decoder.Decode(&req)
		if err == nil {
			var extra json.RawMessage
			if trailingErr := decoder.Decode(&extra); !errors.Is(trailingErr, io.EOF) {
				err = trailingErr
				if err == nil {
					err = errors.New("trailing data")
				}
			}
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			status := http.StatusBadRequest
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "invalid request", status)
			return
		}
		if strings.TrimSpace(req.Model) == "" || req.Stream {
			http.Error(w, "a model and non-streaming request are required", http.StatusBadRequest)
			return
		}
		if f.delay > 0 {
			timer := time.NewTimer(f.delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		if r.Context().Err() != nil {
			return
		}
		// JSON-encode the model before inserting it into the fixed fixture.
		// Marshaling a string cannot fail, and no request content is retained.
		model, _ := json.Marshal(req.Model)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, response, model)
	}
}
