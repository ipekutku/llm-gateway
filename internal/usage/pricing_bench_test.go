package usage

import (
	"testing"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

func BenchmarkPricingCost(b *testing.B) {
	model := Model{Provider: "anthropic", Model: "bench-model"}
	pricing, err := NewPricing(map[Model]Price{
		model: {Input: 1_000_000, CacheRead: 100_000, CacheWrite: 2_000_000, Output: 5_000_000},
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		usage llm.Usage
		want  Cost
	}{
		{"uncached", llm.Usage{InputTokens: 1000, OutputTokens: 200}, 2_000_000_000},
		{"cached", llm.Usage{InputTokens: 1000, CacheReadInputTokens: 600, CacheWriteInputTokens: 100, OutputTokens: 200}, 1_560_000_000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				cost, err := pricing.Cost(model, tc.usage)
				if err != nil || cost != tc.want {
					b.Fatalf("Cost: %v, error %v", cost, err)
				}
			}
		})
	}
}
