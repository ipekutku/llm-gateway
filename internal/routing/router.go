// Package routing selects a provider for a request by exact model name.
package routing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// Router routes chat requests to providers using a static model table.
// Its routing table is safe for concurrent reads and never modified after New.
// Configured providers must support concurrent calls as required by llm.Provider.
type Router struct {
	routes map[string]llm.Provider
}

var _ llm.Provider = (*Router)(nil)

// New returns a Router for the given model-to-provider table. The table is
// copied, so later changes to routes do not affect the Router.
func New(routes map[string]llm.Provider) (*Router, error) {
	if len(routes) == 0 {
		return nil, errors.New("routing: no routes configured")
	}

	copied := make(map[string]llm.Provider, len(routes))
	for model, p := range routes {
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("routing: blank model name")
		}
		if p == nil {
			return nil, fmt.Errorf("routing: nil provider for model %q", model)
		}
		copied[model] = p
	}
	return &Router{routes: copied}, nil
}

// Chat forwards req and ctx unchanged to the provider configured for
// req.Model. It returns an error wrapping llm.ErrUnknownModel if no provider
// is configured. Provider errors are returned as is.
func (r *Router) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p, ok := r.routes[req.Model]
	if !ok {
		return llm.ChatResponse{}, fmt.Errorf("%w: %q", llm.ErrUnknownModel, req.Model)
	}
	return p.Chat(ctx, req)
}
