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
		// B5: the span is anchored to explicit first→last times, not slice order.
		if !strings.Contains(out, "(x3, first 2026-07-01T14:00:00Z → last 2026-07-01T14:02:00Z)") {
			t.Fatalf("repeat count + first→last span missing, got:\n%s", out)
		}
		// First occurrence keeps its own timestamp as the row's leading time.
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

// TestRenderLogLinesStreamIdentity covers B4+B5: query_logs lines carry stream
// fields (kubernetes.pod_name/container_name) but no identity in the message, and
// the backend returns them NEWEST-first. The renderer must (a) prefix a compact
// stream identity per group, (b) count DISTINCT pods, and (c) anchor first→last
// from the min/max Time regardless of input order.
func TestRenderLogLinesStreamIdentity(t *testing.T) {
	t0 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	mk := func(offsetSec int, pod, container, msg string) providers.LogLine {
		return providers.LogLine{
			Time:    t0.Add(time.Duration(offsetSec) * time.Second),
			Message: msg,
			Fields: map[string]string{
				"kubernetes.pod_name":       pod,
				"kubernetes.container_name": container,
			},
		}
	}

	t.Run("stream identity prefix, distinct-pod count, and first→last from shuffled input", func(t *testing.T) {
		// The same error on THREE distinct harbor-core pods, timestamps deliberately
		// out of order (newest-first, as VictoriaLogs returns) to prove the renderer
		// tracks min/max Time rather than trusting slice order.
		lines := providers.LogResult{
			mk(3597, "harbor-core-6f9d5c8b7-zzzzz", "core", "connection refused to postgres:5432"), // last (10:59:57)
			mk(161, "harbor-core-6f9d5c8b7-abcde", "core", "connection refused to postgres:5432"),  // first (10:02:41)
			mk(1800, "harbor-core-6f9d5c8b7-fghij", "core", "connection refused to postgres:5432"),
		}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		// Compact stream identity from the well-known fields: pod truncated at the
		// hash + container.
		if !strings.Contains(out, "[harbor-core-6f9d…/core]") {
			t.Fatalf("stream-identity prefix missing, got:\n%s", out)
		}
		// Distinct pods counted, not raw lines.
		if !strings.Contains(out, "across 3 pods") {
			t.Fatalf("distinct-pod count missing, got:\n%s", out)
		}
		// First-seen anchored to the MIN time and last-seen to the MAX time,
		// independent of input order.
		if !strings.Contains(out, "first 2026-07-11T10:02:41Z") || !strings.Contains(out, "last 2026-07-11T10:59:57Z") {
			t.Fatalf("first→last span must come from min/max Time, not slice order, got:\n%s", out)
		}
	})

	t.Run("single pod does not render an across-N-pods suffix", func(t *testing.T) {
		lines := providers.LogResult{
			mk(10, "harbor-core-6f9d5c8b7-abcde", "core", "boot"),
			mk(20, "harbor-core-6f9d5c8b7-abcde", "core", "boot"),
		}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		if strings.Contains(out, "across") {
			t.Fatalf("a single pod must not render an across-N-pods count, got:\n%s", out)
		}
		if !strings.Contains(out, "[harbor-core-6f9d…/core]") {
			t.Fatalf("single-pod line still needs its stream identity, got:\n%s", out)
		}
	})

	t.Run("pod_logs identity in the message is not double-prefixed", func(t *testing.T) {
		// pod_logs bakes "<pod>: " into the Message and carries NO stream Fields.
		// The renderer must leave it as-is (no bracketed prefix).
		lines := providers.LogResult{
			{Time: t0, Message: "web-abc: serving on :8080"},
		}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		if strings.Contains(out, "[") {
			t.Fatalf("pod_logs lines already carry identity in the message; must not add a bracketed prefix, got:\n%s", out)
		}
		if !strings.Contains(out, "web-abc: serving on :8080") {
			t.Fatalf("message must render verbatim, got:\n%s", out)
		}
	})

	t.Run("network lines with identity-in-message get no bracketed prefix", func(t *testing.T) {
		// Hubble/VPC lines bake src->dst into Message; their Fields carry source/
		// destination/verdict, NOT kubernetes.pod_name — so no bracketed prefix.
		lines := providers.LogResult{
			{Time: t0, Message: "apps/web -> apps/db DROPPED (POLICY_DENIED)", Fields: map[string]string{
				"source": "apps/web", "destination": "apps/db", "verdict": "DROPPED",
			}},
		}
		var b strings.Builder
		renderLogLines(&b, lines, "more lines")
		out := b.String()
		if strings.Contains(out, "[") {
			t.Fatalf("network lines carry identity in the message; must not add a bracketed prefix, got:\n%s", out)
		}
	})
}
