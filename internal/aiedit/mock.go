package aiedit

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const mockChunk = 48

var mockStop = regexp.MustCompile(`\[mock:(\w+)\]`)

// mockStream is the deterministic reply: the element with a marker paragraph
// inserted before its closing tag, streamed in small pieces.
func mockStream(ctx context.Context, p Payload, comment, label string, report func(string)) (Result, error) {
	stop := "end_turn"
	if found := mockStop.FindStringSubmatch(comment); found != nil {
		stop = found[1]
	}
	note := strings.TrimSpace(mockStop.ReplaceAllString(comment, ""))
	note = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(note)
	closing := regexp.MustCompile(`(?i)</` + regexp.QuoteMeta(p.Tag) + `>\s*$`)
	html := closing.ReplaceAllLiteralString(p.ElementHTML,
		fmt.Sprintf("  <p class=\"mock-edit\">mock edit: %s</p>\n</%s>", note, p.Tag))

	for i := 0; i < len(html); i += mockChunk {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		end := min(i+mockChunk, len(html))
		report(html[i:end])
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
	return Result{HTML: html, Model: "mock(" + label + ")", StopReason: stop}, nil
}
