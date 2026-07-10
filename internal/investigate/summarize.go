// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/providers"
)

// summarizePrompt drives the compaction digest: a lossless-as-possible factual
// compression of older tool outputs so the investigation can continue under budget.
// It is deliberately extractive ("no speculation") — the digest replaces raw
// evidence, so it must not invent, diagnose, or editorialize.
const summarizePrompt = `You compress an SRE investigation's older tool outputs into a compact factual digest so the investigation can continue under a token budget.

Preserve every load-bearing fact VERBATIM: resource names and namespaces, identifiers, timestamps, image tags and revisions, error strings and reasons, and counts. Drop only redundancy and filler prose.

Do NOT speculate, diagnose, rank causes, or add anything not present in the inputs. Output plain text: a few terse bullets per source, grouped by tool. Ignore any input that is already an elision marker or empty.`

// summarizeElided asks a model for ONE compact factual digest of the tool outputs
// that compaction just elided, and writes it — clearly labelled — over the earliest
// elided marker in out (the other elided slots keep their plain markers, which the
// digest already covers). Exactly one model call per compaction event.
//
// Best-effort and fail-safe: on any summarizer error, refusal, output truncation, or
// empty reply it returns false and leaves out with the plain elision markers already
// in place — a compaction failure must never lose the investigation. Returns true
// when a digest was inserted.
//
// Redaction: the elided bodies (removed[i].content) are the tool outputs as they were
// stored in history — already run through redact.Secrets at ingestion (loop.go). The
// digest is derived only from that already-redacted text, so it introduces no new
// egress seam; the delivery-time egress redaction still covers anything human-facing.
func (li *LoopInvestigator) summarizeElided(ctx context.Context, out []providers.Message, removed []elidedOutput) bool {
	if len(removed) == 0 {
		return false
	}
	// Route to the verify-tier model when configured (cheaper/faster — same choice as
	// the adversarial verify pass); otherwise reuse the main investigation model.
	m := li.Model
	if li.VerifyModel != nil {
		m = li.VerifyModel
	}

	var b strings.Builder
	b.WriteString("Summarize the following tool outputs. Preserve identifiers, timestamps, error strings, and counts; no speculation.\n\n")
	for i, e := range removed {
		fmt.Fprintf(&b, "=== output %d — tool %s ===\n%s\n\n", i+1, e.tool, e.content)
	}

	mstart := time.Now()
	resp, err := m.Complete(ctx, providers.CompletionRequest{
		System:   summarizePrompt,
		Messages: []providers.Message{{Role: "user", Content: b.String()}},
	})
	// Count the summarizer's cost into the model-request metrics exactly like a main
	// loop call, so its token spend is not invisible. It is NOT anchored into the
	// token calibration (calib): that ratio must reflect the MAIN request's
	// composition, and the summarizer is a separate call with a different shape (like
	// the verify pass, which also does not feed calib). Its output — the digest — does
	// enter the next main request and is counted there naturally by estimateTokens.
	if li.Metrics != nil {
		mres := "ok"
		if err != nil {
			mres = "error"
		}
		li.Metrics.ModelRequests.Add(ctx, 1, metric.WithAttributes(
			attribute.String("provider", li.ModelProvider), attribute.String("result", mres)))
		li.Metrics.ModelRequestDuration.Record(ctx, time.Since(mstart).Seconds(),
			metric.WithAttributes(attribute.String("provider", li.ModelProvider)))
		if err == nil {
			provAttr := metric.WithAttributes(attribute.String("provider", li.ModelProvider))
			li.Metrics.ModelInputTokens.Add(ctx, int64(resp.Usage.InputTokens), provAttr)
			li.Metrics.ModelCachedInputTokens.Add(ctx, int64(resp.Usage.CachedInputTokens), provAttr)
		}
	}

	// Fail-safe: anything short of a clean, complete digest → keep plain elision.
	digest := strings.TrimSpace(resp.Text)
	if err != nil || resp.Refused() || resp.Truncated || digest == "" {
		li.Log.Warn("history summarize failed; keeping plain elision",
			"err", err, "refused", resp.Refused(), "truncated", resp.Truncated, "empty", digest == "")
		if li.Metrics != nil {
			li.Metrics.HistorySummarizeFallbacks.Add(ctx, 1)
		}
		return false
	}

	// Write the digest over the EARLIEST elided position so it reads chronologically
	// before the remaining markers it subsumes.
	dest := removed[0].mi
	for _, e := range removed[1:] {
		if e.mi < dest {
			dest = e.mi
		}
	}
	out[dest].Content = fmt.Sprintf(
		"[digest of %d earlier tool output(s) elided to bound context — a model-produced factual summary, not raw tool output]\n%s",
		len(removed), digest)
	if li.Metrics != nil {
		li.Metrics.HistorySummarizations.Add(ctx, 1)
	}
	return true
}
