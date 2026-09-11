package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/panphora/htmlclay/internal/helper"
)

const helperMaxStatus = 4 << 10

type HelperSpec = helper.Spec
type HelperEvent = helper.Event

func Run(ctx context.Context, spec HelperSpec, emit func(HelperEvent)) {
	helper.Run(ctx, spec, emit)
}

func runDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

func structuredContextError(err error) HelperEvent {
	if errors.Is(err, context.DeadlineExceeded) {
		return structuredHostError("helper_timeout", "helper timed out", nil)
	}
	return structuredHostError("helper_cancelled", "helper cancelled", nil)
}

func structuredHostError(code, message string, details json.RawMessage) HelperEvent {
	return HelperEvent{Kind: "error", Text: message, Code: code, Details: details, Source: "host"}
}

func helperFailureDetails(field, reason string) json.RawMessage {
	return rawJSON(struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	}{Field: field, Reason: reason})
}

func helperLimitDetails(field string, limit int) json.RawMessage {
	return rawJSON(struct {
		Field string `json:"field"`
		Limit int    `json:"limit"`
	}{Field: field, Limit: limit})
}

func helperReadRawLine(r *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if room := max - b.Len(); room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			b.Write(chunk)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && b.Len() == 0 {
			return "", io.EOF
		}
		return strings.TrimRight(b.String(), "\r\n"), err
	}
}
