// Package observability sets up logging (and, from Phase 3, metrics and
// health reporting).
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns a slog.Logger writing to w. level is debug, info, warn
// or error; format is json or text.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("log format %q: want json or text", format)
	}
}
