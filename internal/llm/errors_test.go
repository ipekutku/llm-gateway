package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestProviderErrorMessage(t *testing.T) {
	cause := errors.New("malformed response")

	tests := []struct {
		name string
		err  *ProviderError
		want string
	}{
		{"status and cause", &ProviderError{Provider: "openai", StatusCode: 500, Err: cause}, "openai: upstream status 500: malformed response"},
		{"status only", &ProviderError{Provider: "openai", StatusCode: 429}, "openai: upstream status 429"},
		{"cause only", &ProviderError{Provider: "anthropic", Err: cause}, "anthropic: malformed response"},
		{"neither", &ProviderError{Provider: "anthropic"}, "anthropic: upstream failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProviderErrorUnwrap(t *testing.T) {
	err := fmt.Errorf("routing: %w", &ProviderError{Provider: "openai", Err: context.Canceled})

	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is(err, context.Canceled) = false, want true")
	}

	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatal("errors.As(err, *ProviderError) = false, want true")
	}
	if pe.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", pe.Provider, "openai")
	}
}
