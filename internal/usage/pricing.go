// Package usage estimates what requests cost from the token usage that
// providers report.
//
// Arithmetic is exact: prices are integers in millionths of a dollar per
// million tokens, so a price times a token count is an integer number of
// picodollars. There are no floating-point rounding errors to accumulate
// when costs are summed.
//
// Costs are estimates. They use the configured prices, which may be out of
// date, and cover only the usage a provider reported for a successful
// response.
package usage

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// ErrNoPrice reports that no price is configured for a provider and model.
var ErrNoPrice = errors.New("no price configured")

// Rate is a token price in millionths of a US dollar per million tokens:
// 2_500_000 is $2.50 per million tokens.
type Rate int64

// MaxRate bounds a Rate at $1,000,000 per million tokens, far above any
// real price, so that a mistyped price is rejected.
const MaxRate Rate = 1_000_000 * 1_000_000

// rateDecimals is the number of decimal places a Rate holds, in dollars.
const rateDecimals = 6

// ParseRate parses a price in US dollars per million tokens, such as "2.50"
// or "0.075", into a Rate. It accepts a non-negative decimal with up to six
// decimal places and no sign or exponent, which includes every JSON number
// a price would be written as.
func ParseRate(s string) (Rate, error) {
	whole, frac, hasPoint := strings.Cut(s, ".")
	switch {
	case !isDigits(whole):
		return 0, fmt.Errorf("price %q is not a decimal number of dollars", s)
	case hasPoint && !isDigits(frac):
		return 0, fmt.Errorf("price %q is not a decimal number of dollars", s)
	case len(frac) > rateDecimals:
		return 0, fmt.Errorf("price %q has more than %d decimal places", s, rateDecimals)
	}
	frac += strings.Repeat("0", rateDecimals-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil || Rate(n) > MaxRate {
		return 0, fmt.Errorf("price %q exceeds $1,000,000 per million tokens", s)
	}
	return Rate(n), nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Cost is an amount in picodollars, 10⁻¹² US dollars.
type Cost int64

// String formats c in dollars with all twelve decimal places, such as
// "0.000123450000".
func (c Cost) String() string {
	const perDollar = 1_000_000_000_000
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%012d", sign, c/perDollar, c%perDollar)
}

// Price is one model's token prices. Each kind of token has its own Rate,
// because providers charge less for prompt-cache reads and, for Anthropic,
// more for cache writes.
type Price struct {
	// Input is the price of input tokens not read from or written to the
	// prompt cache.
	Input      Rate
	CacheRead  Rate
	CacheWrite Rate
	Output     Rate
}

func (p Price) validate() error {
	for _, r := range []Rate{p.Input, p.CacheRead, p.CacheWrite, p.Output} {
		if r < 0 || r > MaxRate {
			return errors.New("rates must be from 0 to $1,000,000 per million tokens")
		}
	}
	return nil
}

// Cost returns the estimated cost of u. The cache counts are part of
// u.InputTokens, so only the rest is charged at the Input rate. It fails if
// u is inconsistent, with a negative count or more cached than input
// tokens, or so large that the cost would overflow.
func (p Price) Cost(u llm.Usage) (Cost, error) {
	if u.InputTokens < 0 || u.CacheReadInputTokens < 0 || u.CacheWriteInputTokens < 0 || u.OutputTokens < 0 ||
		u.CacheReadInputTokens > u.InputTokens-u.CacheWriteInputTokens {
		return 0, fmt.Errorf("inconsistent usage %+v", u)
	}
	uncached := u.InputTokens - u.CacheReadInputTokens - u.CacheWriteInputTokens

	var total int64
	for _, part := range []struct {
		tokens int
		rate   Rate
	}{
		{uncached, p.Input},
		{u.CacheReadInputTokens, p.CacheRead},
		{u.CacheWriteInputTokens, p.CacheWrite},
		{u.OutputTokens, p.Output},
	} {
		tokens, rate := int64(part.tokens), int64(part.rate)
		if rate != 0 && tokens > (math.MaxInt64-total)/rate {
			return 0, fmt.Errorf("cost of usage %+v overflows", u)
		}
		total += tokens * rate
	}
	return Cost(total), nil
}

// Model identifies a priced model: a provider and the model name configured
// for it.
type Model struct {
	Provider string
	Model    string
}

// Pricing holds the prices of the configured models. It is safe for
// concurrent use and never modified after NewPricing.
type Pricing struct {
	prices map[Model]Price
}

// NewPricing returns a Pricing for prices. The map is copied. Provider and
// model names must not be blank, and every rate must be from 0 to MaxRate.
func NewPricing(prices map[Model]Price) (*Pricing, error) {
	copied := make(map[Model]Price, len(prices))
	for m, p := range prices {
		if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.Model) == "" {
			return nil, fmt.Errorf("usage: blank provider or model in price for %q/%q", m.Provider, m.Model)
		}
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("usage: price for %s/%s: %w", m.Provider, m.Model, err)
		}
		copied[m] = p
	}
	return &Pricing{prices: copied}, nil
}

// Cost returns the estimated cost of u for model m. It returns an error
// wrapping ErrNoPrice if m has no price, and the error of Price.Cost if u
// cannot be priced. A cost that cannot be estimated is unknown, never zero.
func (p *Pricing) Cost(m Model, u llm.Usage) (Cost, error) {
	price, ok := p.prices[m]
	if !ok {
		return 0, fmt.Errorf("%w for %s/%s", ErrNoPrice, m.Provider, m.Model)
	}
	return price.Cost(u)
}
