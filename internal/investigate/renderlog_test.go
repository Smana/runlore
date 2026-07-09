// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestRenderLogLinesDedup(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
	mk := func(offsetSec int, msg string) providers.LogLine {
		return providers.LogLine{Time: t0.Add(time.Duration(offsetSec) * time.Second), Message: msg}
	}

	t.Run("collapses a repeated line into one row with count and span", func(t *testing.T) {
		// A crash loop: the same panic line repeats across restarts, interleaved
		// with a distinct line. Only 2 distinct rows must render, each counted.
		lines := providers.LogResult{
			mk(0, "panic: connection refused"),
			mk(1, "restarting"),
			mk(60, "panic: connection refused"),
			mk(120, "panic: connection refused"),
		}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		if strings.Count(out, "panic: connection refused") != 1 {
			t.Fatalf("repeated message must render once, got:\n%s", out)
		}
		if !strings.Contains(out, "(x3, last 2026-07-01T14:02:00Z)") {
			t.Fatalf("repeat count + last-seen missing, got:\n%s", out)
		}
		// First occurrence keeps its own timestamp.
		if !strings.Contains(out, "2026-07-01T14:00:00Z panic: connection refused") {
			t.Fatalf("first-occurrence timestamp missing, got:\n%s", out)
		}
		// The single-occurrence line renders plain (no count suffix).
		if !strings.Contains(out, "restarting\n") || strings.Contains(out, "restarting (x") {
			t.Fatalf("unique line must render without a count, got:\n%s", out)
		}
	})

	t.Run("omits timestamps when the source has none", func(t *testing.T) {
		lines := providers.LogResult{{Message: "web-1: started"}, {Message: "web-1: started"}}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		if strings.Contains(out, "0001-01-01") {
			t.Fatalf("zero timestamps must not render, got:\n%s", out)
		}
		if !strings.Contains(out, "web-1: started (x2)") {
			t.Fatalf("count without timestamps expected, got:\n%s", out)
		}
	})

	t.Run("caps at maxToolRows DISTINCT messages", func(t *testing.T) {
		var lines providers.LogResult
		// 60 distinct messages, each repeated twice: dedup first, then cap — so the
		// cap counts distinct messages, not raw lines.
		for i := 0; i < 60; i++ {
			msg := fmt.Sprintf("distinct line %02d", i)
			lines = append(lines, providers.LogLine{Message: msg}, providers.LogLine{Message: msg})
		}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		if !strings.Contains(out, "distinct line 49") {
			t.Fatalf("the 50th distinct message must render, got tail:\n%s", out[len(out)-200:])
		}
		if strings.Contains(out, "distinct line 50") {
			t.Fatalf("the 51st distinct message must be capped, got:\n%s", out)
		}
		if !strings.Contains(out, "(10 more lines)") {
			t.Fatalf("truncation note must count remaining DISTINCT messages, got tail:\n%s", out[len(out)-200:])
		}
	})
}
