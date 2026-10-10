package main

import (
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
)

// logFormatVar selects the log format: text, the default, or json.
const logFormatVar = "GATEWAY_LOG_FORMAT"

// newLogger returns the gateway's logger, writing to w in the format named
// by GATEWAY_LOG_FORMAT. Records logged while handling a request carry its
// request and client IDs. It is read before the rest of the configuration,
// so configuration errors are logged in the chosen format.
func newLogger(w io.Writer, getenv func(string) string) (*slog.Logger, error) {
	var h slog.Handler
	switch strings.TrimSpace(getenv(logFormatVar)) {
	case "", "text":
		h = slog.NewTextHandler(w, nil)
	case "json":
		h = slog.NewJSONHandler(w, nil)
	default:
		return nil, errors.New(logFormatVar + " must be text or json")
	}
	return slog.New(httpapi.NewLogHandler(h)), nil
}
