package usage

import (
	"math"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

func validRecord() Record {
	cost := Cost(1)
	return Record{
		RequestID:      "req-1",
		Time:           time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		Duration:       time.Second,
		ClientID:       "team-a",
		RequestedModel: "gpt-4o",
		Provider:       "openai",
		Model:          "gpt-4o-2024-08-06",
		Status:         200,
		Usage:          &llm.Usage{InputTokens: 10, CacheReadInputTokens: 4, CacheWriteInputTokens: 6, OutputTokens: 5},
		Cost:           &cost,
	}
}

func TestRecordValidateAccepts(t *testing.T) {
	tests := map[string]func(*Record){
		"success with cost": func(*Record) {},
		"usage without cost": func(r *Record) {
			r.Cost = nil
		},
		"failure without usage": func(r *Record) {
			r.Status, r.ErrorCode, r.Provider, r.Model, r.Usage, r.Cost = 404, "model_not_found", "", "", nil, nil
		},
		"client closed": func(r *Record) {
			r.Status, r.ErrorCode, r.Usage, r.Cost = StatusClientClosed, "client_closed", nil, nil
		},
		"zero duration": func(r *Record) { r.Duration = 0 },
		"largest counts": func(r *Record) {
			r.Usage = &llm.Usage{InputTokens: math.MaxInt32, OutputTokens: math.MaxInt32}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r := validRecord()
			change(&r)
			if err := r.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestRecordValidateRejects(t *testing.T) {
	negative := Cost(-1)
	tests := map[string]func(*Record){
		"no request ID":           func(r *Record) { r.RequestID = "" },
		"no time":                 func(r *Record) { r.Time = time.Time{} },
		"negative duration":       func(r *Record) { r.Duration = -time.Millisecond },
		"duration too long":       func(r *Record) { r.Duration = 25 * time.Hour },
		"no client":               func(r *Record) { r.ClientID = "" },
		"no requested model":      func(r *Record) { r.RequestedModel = "" },
		"status too low":          func(r *Record) { r.Status = 0 },
		"status too high":         func(r *Record) { r.Status = 600 },
		"success with error code": func(r *Record) { r.ErrorCode = "upstream_error" },
		"failure without code":    func(r *Record) { r.Status = 502 },
		"negative tokens":         func(r *Record) { r.Usage.OutputTokens = -1 },
		"count beyond 32 bits":    func(r *Record) { r.Usage.InputTokens = math.MaxInt32 + 1 },
		"more cached than input":  func(r *Record) { r.Usage.CacheReadInputTokens = 5 },
		"cost without usage":      func(r *Record) { r.Usage = nil },
		"negative cost":           func(r *Record) { r.Cost = &negative },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r := validRecord()
			change(&r)
			if err := r.Validate(); err == nil {
				t.Error("Validate() = nil, want error")
			}
		})
	}
}
