package usage

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    Rate
		wantErr bool
	}{
		{in: "2.50", want: 2_500_000},
		{in: "2.5", want: 2_500_000},
		{in: "10", want: 10_000_000},
		{in: "0.075", want: 75_000},
		{in: "0.000001", want: 1},
		{in: "0", want: 0},
		{in: "0.0", want: 0},
		{in: "007", want: 7_000_000},
		{in: "1000000", want: MaxRate},
		{in: "1000000.000000", want: MaxRate},
		{in: "1000000.000001", wantErr: true},
		{in: "99999999999999999999", wantErr: true},
		{in: "0.0000001", wantErr: true},
		{in: "", wantErr: true},
		{in: ".5", wantErr: true},
		{in: "5.", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "+1", wantErr: true},
		{in: "1e3", wantErr: true},
		{in: "1.2.3", wantErr: true},
		{in: " 1", wantErr: true},
		{in: "1,5", wantErr: true},
		{in: "١", wantErr: true}, // a non-ASCII digit
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRate(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRate(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRate(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseRate(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestCostString(t *testing.T) {
	tests := []struct {
		c    Cost
		want string
	}{
		{0, "0.000000000000"},
		{1, "0.000000000001"},
		{123_450_000, "0.000123450000"},
		{2_500_000_000_000, "2.500000000000"},
		{-1, "-0.000000000001"},
	}
	for _, tt := range tests {
		if got := tt.c.String(); got != tt.want {
			t.Errorf("Cost(%d).String() = %q, want %q", int64(tt.c), got, tt.want)
		}
	}
}

// gpt4o and opus are example prices in dollars per million tokens.
var (
	gpt4o = Price{Input: 2_500_000, CacheRead: 1_250_000, CacheWrite: 0, Output: 10_000_000}
	opus  = Price{Input: 5_000_000, CacheRead: 500_000, CacheWrite: 6_250_000, Output: 25_000_000}
)

func TestPriceCost(t *testing.T) {
	tests := []struct {
		name  string
		price Price
		usage llm.Usage
		want  Cost
	}{
		{
			name:  "no cache",
			price: gpt4o,
			usage: llm.Usage{InputTokens: 12, OutputTokens: 8},
			// 12 × $2.50/M + 8 × $10/M = $0.00003 + $0.00008
			want: 110_000_000,
		},
		{
			name:  "cache reads are not charged as input",
			price: gpt4o,
			usage: llm.Usage{InputTokens: 2006, CacheReadInputTokens: 1920, OutputTokens: 300},
			// 86 × $2.50/M + 1920 × $1.25/M + 300 × $10/M
			want: 215_000_000 + 2_400_000_000 + 3_000_000_000,
		},
		{
			name:  "cache reads and writes",
			price: opus,
			usage: llm.Usage{InputTokens: 1110, CacheReadInputTokens: 1000, CacheWriteInputTokens: 100, OutputTokens: 4},
			// 10 × $5/M + 1000 × $0.50/M + 100 × $6.25/M + 4 × $25/M
			want: 50_000_000 + 500_000_000 + 625_000_000 + 100_000_000,
		},
		{
			name:  "all input cached",
			price: opus,
			usage: llm.Usage{InputTokens: 1000, CacheReadInputTokens: 1000},
			want:  500_000_000,
		},
		{
			name:  "smallest rate is exact",
			price: Price{Input: 1},
			usage: llm.Usage{InputTokens: 1},
			want:  1,
		},
		{name: "no tokens", price: opus, usage: llm.Usage{}, want: 0},
		{name: "free model", price: Price{}, usage: llm.Usage{InputTokens: 100, OutputTokens: 100}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.price.Cost(tt.usage)
			if err != nil {
				t.Fatalf("Cost() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPriceCostRejectsUnusableUsage(t *testing.T) {
	tests := []struct {
		name  string
		usage llm.Usage
	}{
		{"negative input", llm.Usage{InputTokens: -1}},
		{"negative output", llm.Usage{OutputTokens: -1}},
		{"negative cache read", llm.Usage{InputTokens: 10, CacheReadInputTokens: -1}},
		{"negative cache write", llm.Usage{InputTokens: 10, CacheWriteInputTokens: -1}},
		{"more cache reads than input", llm.Usage{InputTokens: 10, CacheReadInputTokens: 11}},
		{"more cached than input", llm.Usage{InputTokens: 10, CacheReadInputTokens: 6, CacheWriteInputTokens: 5}},
		{"overflow in one part", llm.Usage{OutputTokens: math.MaxInt}},
		{"overflow in the sum", llm.Usage{InputTokens: math.MaxInt64 / int(MaxRate), OutputTokens: math.MaxInt64 / int(MaxRate)}},
	}
	maxed := Price{Input: MaxRate, CacheRead: MaxRate, CacheWrite: MaxRate, Output: MaxRate}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := maxed.Cost(tt.usage); err == nil {
				t.Errorf("Cost(%+v) = %v, want error", tt.usage, got)
			}
		})
	}
}

func TestPricingCost(t *testing.T) {
	p, err := NewPricing(map[Model]Price{
		{"openai", "gpt-4o"}:             gpt4o,
		{"anthropic", "claude-opus-5-5"}: opus,
	})
	if err != nil {
		t.Fatalf("NewPricing() error = %v", err)
	}
	u := llm.Usage{InputTokens: 12, OutputTokens: 8}

	got, err := p.Cost(Model{"openai", "gpt-4o"}, u)
	if err != nil || got != 110_000_000 {
		t.Errorf("Cost(openai/gpt-4o) = %v, %v; want 0.000110000000", got, err)
	}

	for _, m := range []Model{
		{"openai", "gpt-4o-2024-08-06"}, // a reported model name, not the configured one
		{"anthropic", "gpt-4o"},
		{"", ""},
	} {
		if got, err := p.Cost(m, u); !errors.Is(err, ErrNoPrice) {
			t.Errorf("Cost(%+v) = %v, %v; want ErrNoPrice", m, got, err)
		}
	}

	if _, err := p.Cost(Model{"openai", "gpt-4o"}, llm.Usage{InputTokens: -1}); err == nil || errors.Is(err, ErrNoPrice) {
		t.Errorf("Cost() with inconsistent usage error = %v, want a usage error", err)
	}
}

func TestNewPricingValidates(t *testing.T) {
	tests := []struct {
		name   string
		prices map[Model]Price
	}{
		{"blank provider", map[Model]Price{{" ", "gpt-4o"}: gpt4o}},
		{"blank model", map[Model]Price{{"openai", ""}: gpt4o}},
		{"negative rate", map[Model]Price{{"openai", "gpt-4o"}: {Output: -1}}},
		{"rate above maximum", map[Model]Price{{"openai", "gpt-4o"}: {CacheWrite: MaxRate + 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewPricing(tt.prices); err == nil {
				t.Error("NewPricing() error = nil, want error")
			}
		})
	}

	if _, err := NewPricing(nil); err != nil {
		t.Errorf("NewPricing(nil) error = %v, want nil", err)
	}
}

func TestNewPricingCopiesPrices(t *testing.T) {
	prices := map[Model]Price{{"openai", "gpt-4o"}: gpt4o}
	p, err := NewPricing(prices)
	if err != nil {
		t.Fatalf("NewPricing() error = %v", err)
	}
	prices[Model{"openai", "gpt-4o"}] = Price{}
	delete(prices, Model{"openai", "gpt-4o"})

	if got, err := p.Cost(Model{"openai", "gpt-4o"}, llm.Usage{OutputTokens: 1}); err != nil || got != 10_000_000 {
		t.Errorf("Cost() after changing the input map = %v, %v; want 0.000010000000", got, err)
	}
}

func TestPricingConcurrentUse(t *testing.T) {
	p, err := NewPricing(map[Model]Price{{"openai", "gpt-4o"}: gpt4o})
	if err != nil {
		t.Fatalf("NewPricing() error = %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, err := p.Cost(Model{"openai", "gpt-4o"}, llm.Usage{InputTokens: 1, OutputTokens: 1}); err != nil {
					t.Errorf("Cost() error = %v", err)
				}
			}
		})
	}
	wg.Wait()
}
