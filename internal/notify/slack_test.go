// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestSlackDeliver(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewSlack(srv.URL).Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	text, _ := got["text"].(string)
	// The fallback is now the one-line fallbackText: verdict label + confidence,
	// not the full Format body.
	if text == "" || !contains(text, "Action required") {
		t.Fatalf("unexpected slack payload: %v", got)
	}
}

func TestSlackBotDeliver(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/chat.postMessage" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	if err := bot.Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if auth != "Bearer xoxb-test" {
		t.Fatalf("auth header = %q, want Bearer xoxb-test", auth)
	}
	if got["channel"] != "C123" {
		t.Fatalf("channel = %v, want C123", got["channel"])
	}
	if text, _ := got["text"].(string); !contains(text, "Action required") {
		t.Fatalf("unexpected payload: %v", got)
	}
}

func TestSlackBotAPIError(t *testing.T) {
	// chat.postMessage returns HTTP 200 with ok:false on logical failures
	// (e.g. not_in_channel) — Deliver must surface that as an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
	}))
	defer srv.Close()

	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	err := bot.Deliver(context.Background(), sampleInvestigation())
	if err == nil || !contains(err.Error(), "not_in_channel") {
		t.Fatalf("expected not_in_channel error, got %v", err)
	}
}

// TestSlackBotDeliverThreadsDetail proves the bot path posts a compact summary to
// the channel, then the full analysis as a thread reply keyed on the summary's ts.
func TestSlackBotDeliverThreadsDetail(t *testing.T) {
	var posts []map[string]any
	var raw []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = append(raw, string(b))
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		posts = append(posts, m)
		w.Header().Set("Content-Type", "application/json")
		if len(posts) == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	if err := bot.Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d POSTs, want 2", len(posts))
	}
	// First message: the summary, posted to the channel with no thread_ts.
	if _, ok := posts[0]["thread_ts"]; ok {
		t.Fatalf("summary must not carry thread_ts: %v", posts[0])
	}
	if !contains(raw[0], "Action required") {
		t.Fatalf("summary blocks missing verdict summary:\n%s", raw[0])
	}
	// Second message: the detail, threaded under the summary's ts.
	if posts[1]["thread_ts"] != "111.222" {
		t.Fatalf("detail thread_ts = %v, want 111.222", posts[1]["thread_ts"])
	}
	if !contains(raw[1], "Full analysis") {
		t.Fatalf("detail blocks missing full analysis:\n%s", raw[1])
	}
	// The detail reply's fallback text is a short pointer, not the full body.
	text, _ := posts[1]["text"].(string)
	if !strings.HasPrefix(text, "Full analysis") {
		t.Fatalf("detail fallback text = %q, want short 'Full analysis' pointer", text)
	}
	if len(text) > 200 {
		t.Fatalf("detail fallback text too long (%d chars): %q", len(text), text)
	}
}

// TestSlackBotDeliverNoThreadWhenNoDetail proves a minimal investigation (nothing
// beyond the summary) posts exactly one message — no empty thread reply.
func TestSlackBotDeliverNoThreadWhenNoDetail(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
	}))
	defer srv.Close()

	inv := providers.Investigation{
		Title:      "brief blip",
		Verdict:    providers.VerdictNoAction,
		Confidence: 0.5,
		RootCauses: []providers.Hypothesis{{Summary: "transient", Confidence: 0.5, Evidence: []string{"one"}}},
	}
	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	if err := bot.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if posts != 1 {
		t.Fatalf("got %d POSTs, want 1 (no detail thread)", posts)
	}
}

// TestSlackBotDeliverDetailFailureSurfaced proves a failed detail thread reply is
// surfaced as a wrapped error — but the wrapping records that the summary (the
// notification) already landed, so Multi logs it without implying total failure.
func TestSlackBotDeliverDetailFailureSurfaced(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.Header().Set("Content-Type", "application/json")
		if posts == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
		}
	}))
	defer srv.Close()

	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	err := bot.Deliver(context.Background(), sampleInvestigation())
	if err == nil {
		t.Fatal("a failed detail thread must surface an error")
	}
	if !contains(err.Error(), "summary delivered") || !contains(err.Error(), "ratelimited") {
		t.Fatalf("error should wrap the detail failure noting the summary landed, got %v", err)
	}
	if posts != 2 {
		t.Fatalf("got %d POSTs, want 2", posts)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// failingNotifier always errors.
type failingNotifier struct{}

func (failingNotifier) Deliver(context.Context, providers.Investigation) error {
	return io.ErrUnexpectedEOF
}

func TestSlackMessageButtons(t *testing.T) {
	// No ApprovalID → rich blocks, but no interactive Approve/Reject elements.
	m := slackMessage(providers.Investigation{Confidence: 0.5, RootCauses: []providers.Hypothesis{{Summary: "x"}}, Actions: []providers.Action{{Description: "x"}}})
	raw, _ := json.Marshal(m)
	if contains(string(raw), "runlore_approve") {
		t.Fatalf("did not expect Approve/Reject buttons without an ApprovalID:\n%s", raw)
	}
	// With ApprovalID → Block Kit Approve/Reject buttons carrying the id.
	m = slackMessage(providers.Investigation{Confidence: 0.9, Actions: []providers.Action{{Description: "suspend ks/apps", ApprovalID: "a7"}}})
	if _, ok := m["blocks"]; !ok {
		t.Fatal("expected interactive blocks for a pending action")
	}
	raw, _ = json.Marshal(m)
	for _, want := range []string{"runlore_approve", "runlore_reject", `"value":"a7"`, "Approve", "Reject"} {
		if !contains(string(raw), want) {
			t.Fatalf("rendered message missing %q:\n%s", want, raw)
		}
	}
}

func TestSlackBlocksLayout(t *testing.T) {
	inv := providers.Investigation{
		Title:      "VictoriaTracesDown",
		Confidence: 0, // model left top-level at 0 …
		RootCauses: []providers.Hypothesis{{
			Summary: "crds sync broke victoria-traces", Confidence: 0.8, // … but a root cause is 80%
			ChangeRef: "crds@abc123", Evidence: []string{"reconcile failed", "stalled resources"},
			SuggestedAction: "flux rollback hr/victoria-traces", Reversible: true,
		}},
		Unresolved: []string{"why the migration stalled"},
		CuratedURL: "https://github.com/o/r/issues/9",
	}
	blocks := append(summaryBlocks(inv), detailBlocks(inv)...)
	if blocks[0]["type"] != "header" {
		t.Fatalf("first block must be a header, got %v", blocks[0]["type"])
	}
	raw, _ := json.Marshal(blocks)
	s := string(raw)
	// Headline confidence falls back to the top root cause (80%, High), not the 0% top-level.
	for _, want := range []string{"VictoriaTracesDown", "High confidence", "80%", "What changed", "crds@abc123",
		"Suggested next steps", "flux rollback hr/victoria-traces", "(reversible)", "Open questions", "view entry"} {
		if !contains(s, want) {
			t.Fatalf("blocks missing %q:\n%s", want, s)
		}
	}
}

// TestSlackSummaryLayout proves the verdict-first summary: the header anchors on
// the alert name, the second block is the verdict section (NOT the old top
// confidence context line), confidence follows immediately in the header area,
// and the metadata fields (cluster + recurrence) land AFTER the answer/action —
// never before them.
func TestSlackSummaryLayout(t *testing.T) {
	inv := providers.Investigation{
		Title:       "harbor is degraded",
		Verdict:     providers.VerdictNoAction,
		Confidence:  0.8,
		AlertName:   "HarborDown",
		Severity:    "critical",
		Tenant:      "platform",
		Cluster:     "eu-west-1",
		Resource:    providers.Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"},
		StartedAt:   time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
		RootCauses:  []providers.Hypothesis{{Summary: "chart bump", Confidence: 0.8, ChangeRef: "c@1", Evidence: []string{"pg_up=0"}}},
		Occurrences: 3,
		Verified:    true,
		CuratedURL:  "https://github.com/o/r/issues/9",
	}
	blocks := summaryBlocks(inv)
	if blocks[0]["type"] != "header" {
		t.Fatalf("block[0] must be a header, got %v", blocks[0]["type"])
	}
	// Verdict owns the second slot: a section, not the old confidence context line.
	if blocks[1]["type"] != "section" {
		t.Fatalf("block[1] must be the verdict section, not %v (old top confidence line must be gone)", blocks[1]["type"])
	}
	// Confidence immediately follows the verdict — it owns the header area, not
	// the footer.
	if blocks[2]["type"] != "context" {
		t.Fatalf("block[2] must be the confidence context line, not %v", blocks[2]["type"])
	}
	raw, _ := json.Marshal(blocks)
	s := string(raw)
	for _, want := range []string{"HarborDown", "No action needed", "*Cluster:*", "*Recurrence:*", "confidence", "view entry"} {
		if !contains(s, want) {
			t.Fatalf("summary blocks missing %q:\n%s", want, s)
		}
	}
	// Finding #1: the verdict line must carry the verdict ALONE — the header
	// above already spelled out the title/alert name in full.
	texts := mrkdwnTexts(blocks)
	if strings.Contains(texts[0], "harbor is degraded") {
		t.Fatalf("verdict block must not restate the title:\n%s", texts[0])
	}
	// Finding #5: confidence and the agent identity must not repeat between the
	// header area and the footer.
	if strings.Count(s, "80%") != 1 {
		t.Fatalf("confidence (80%%) must render exactly once, got %d times:\n%s", strings.Count(s, "80%"), s)
	}
	if strings.Contains(s, "RunLore SRE agent") {
		t.Fatalf("agent self-identification must not render on the card:\n%s", s)
	}
	// Finding: metadata (Why comes first, then metadata fields — "Resource" et al
	// drop below the answer/action). The "fields" block must come after the "Why"
	// section.
	whyIdx, fieldsIdx := -1, -1
	for i, b := range blocks {
		if _, ok := b["fields"]; ok {
			fieldsIdx = i
		}
		if txt, ok := b["text"].(map[string]any); ok {
			if s, _ := txt["text"].(string); strings.HasPrefix(s, "*Why:*") {
				whyIdx = i
			}
		}
	}
	if whyIdx == -1 || fieldsIdx == -1 || fieldsIdx < whyIdx {
		t.Fatalf("metadata fields (idx %d) must come AFTER Why (idx %d)", fieldsIdx, whyIdx)
	}
}

// TestSlackSummaryOmitsRuledOutAndDataGaps proves findings #2/#3: an
// investigation with root causes, ruled-out hypotheses and data gaps renders
// NEITHER list in the summary card (both are already carried in full in the
// threaded detail — see TestSlackDetailBlocksCarriesRuledOutAndDataGaps) — the
// summary's fold space goes to Why and Suggested next steps instead.
func TestSlackSummaryOmitsRuledOutAndDataGaps(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "cause", Confidence: 0.8, SuggestedAction: "do x"}},
		RuledOut:   []string{"network partition"},
		DataGaps:   []string{"disk metrics unavailable"},
	}
	txt := blocksText(t, summaryBlocks(inv))
	for _, absent := range []string{"Ruled out", "Data gaps", "network partition", "disk metrics unavailable"} {
		if strings.Contains(txt, absent) {
			t.Errorf("summary must not render %q — it belongs only in the thread\n%s", absent, txt)
		}
	}
}

// TestSlackDetailBlocksCarriesRuledOutAndDataGaps proves the thread reply still
// carries the full Ruled-out and Data-gaps lists (nothing is lost — it just
// isn't duplicated in the summary).
func TestSlackDetailBlocksCarriesRuledOutAndDataGaps(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		RootCauses: []providers.Hypothesis{{Summary: "cause"}},
		RuledOut:   []string{"network partition"},
		DataGaps:   []string{"disk metrics unavailable"},
	}
	txt := blocksText(t, detailBlocks(inv))
	for _, want := range []string{"*❌ Ruled out:*", "network partition", "*⚠️ Data gaps:*", "disk metrics unavailable"} {
		if !strings.Contains(txt, want) {
			t.Errorf("detail thread missing %q\n%s", want, txt)
		}
	}
}

// TestSlackHypothesisPluralization proves finding #7: "1 more hypothesis" is
// singular, not "1 more hypotheses".
func TestSlackHypothesisPluralization(t *testing.T) {
	inv := providers.Investigation{
		Title: "t",
		RootCauses: []providers.Hypothesis{
			{Summary: "top cause", Confidence: 0.8},
			{Summary: "second cause", Confidence: 0.5},
		},
	}
	txt := blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "1 more hypothesis below") {
		t.Errorf("expected singular 'hypothesis' for a count of 1:\n%s", txt)
	}
	if strings.Contains(txt, "1 more hypotheses") {
		t.Errorf("must not use the plural for a count of 1:\n%s", txt)
	}

	inv.RootCauses = append(inv.RootCauses, providers.Hypothesis{Summary: "third cause"})
	txt = blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "2 more hypotheses below") {
		t.Errorf("expected plural 'hypotheses' for a count of 2:\n%s", txt)
	}
}

// TestSlackWhatChangedNone proves finding #7: an empty ChangeRef renders an
// informative clause, not the bare word "none" (which reads as a missing
// field on a woken-up phone screen).
func TestSlackWhatChangedNone(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		RootCauses: []providers.Hypothesis{{Summary: "infra fault"}}, // no ChangeRef
	}
	txt := blocksText(t, summaryBlocks(inv))
	if strings.Contains(txt, "*What changed:*\nnone") {
		t.Errorf("must not render the bare placeholder \"none\":\n%s", txt)
	}
	if !strings.Contains(txt, "No Git change") {
		t.Errorf("expected an informative clause in place of \"none\":\n%s", txt)
	}
}

// TestSlackRecallConfidenceDisagreement proves finding #4: when a recalled
// entry's own (top-hypothesis) confidence differs from the delivered,
// outcome-weighted confidence, the card discloses BOTH, labeled — never two
// bare, seemingly-contradictory percentages — and the number renders exactly
// once outside that explicit disclosure.
func TestSlackRecallConfidenceDisagreement(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		Recalled:   true,
		Confidence: 0.78,                                                                           // outcome-weighted delivered confidence
		RootCauses: []providers.Hypothesis{{Summary: "AccessKey hit IAM quota", Confidence: 0.95}}, // entry's own match confidence
	}
	txt := blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "78%") {
		t.Errorf("delivered confidence (78%%) must headline the card:\n%s", txt)
	}
	if !strings.Contains(txt, "entry's own confidence 95%, adjusted to 78% by its track record") {
		t.Errorf("must disclose the entry's own confidence and label it:\n%s", txt)
	}
	// The bare "95%" must never appear on its own — it only ever appears inside
	// the disambiguating sentence above.
	if strings.Count(txt, "95%") != 1 {
		t.Errorf("own confidence (95%%) must appear exactly once (inside the disclosure), got %d:\n%s",
			strings.Count(txt, "95%"), txt)
	}

	// When the two agree (post-rounding), no disclosure renders — nothing to
	// disambiguate.
	inv.RootCauses[0].Confidence = 0.78
	txt = blocksText(t, summaryBlocks(inv))
	if strings.Contains(txt, "entry's own confidence") {
		t.Errorf("must not disclose anything when the two confidences agree:\n%s", txt)
	}
}

// TestSlackFallbackOneLine proves the fallback is a single line carrying the
// verdict label, with a hostile title escaped inert.
func TestSlackFallbackOneLine(t *testing.T) {
	inv := sampleInvestigation()
	inv.Title = "<b>boom</b>"
	text, _ := slackMessage(inv)["text"].(string)
	if strings.Contains(text, "\n") {
		t.Fatalf("fallback text must be one line, got:\n%q", text)
	}
	if !strings.Contains(text, "Action required") {
		t.Fatalf("fallback text missing the verdict label:\n%s", text)
	}
	if !strings.Contains(text, "&lt;b&gt;boom&lt;/b&gt;") {
		t.Fatalf("hostile title not escaped in fallback:\n%s", text)
	}
}

// TestSlackDetailBlocksFullEvidence proves the summary caps the top root cause at
// three evidence bullets while the detail section carries all of them.
func TestSlackDetailBlocksFullEvidence(t *testing.T) {
	inv := providers.Investigation{
		Title: "t",
		RootCauses: []providers.Hypothesis{{
			Summary:  "x",
			Evidence: []string{"ev-one", "ev-two", "ev-three", "ev-four", "ev-five", "ev-six"},
		}},
	}
	summary := strings.Join(mrkdwnTexts(summaryBlocks(inv)), "\n")
	if !strings.Contains(summary, "ev-three") || strings.Contains(summary, "ev-four") {
		t.Fatalf("summary must show 3 evidence bullets, not more:\n%s", summary)
	}
	if !strings.Contains(summary, "…") {
		t.Fatalf("summary must flag the elided evidence with an ellipsis:\n%s", summary)
	}
	detail := detailBlocks(inv)
	if detail == nil {
		t.Fatal("a 6-evidence root cause must emit detail blocks")
	}
	joined := strings.Join(mrkdwnTexts(detail), "\n")
	for _, want := range []string{"ev-one", "ev-six"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("detail blocks missing full evidence %q:\n%s", want, joined)
		}
	}
}

// TestSlackNoVerdictFallsBack proves an investigation without a verdict (old /
// recall) renders no verdict badge but still surfaces confidence — the layout
// stays complete.
func TestSlackNoVerdictFallsBack(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "x", Confidence: 0.8}},
	}
	blocks := append(summaryBlocks(inv), detailBlocks(inv)...)
	joined := strings.Join(mrkdwnTexts(blocks), "\n")
	if strings.Contains(joined, "No action needed") || strings.Contains(joined, "Action required") {
		t.Fatalf("empty verdict must render no verdict badge:\n%s", joined)
	}
	if !strings.Contains(joined, "confidence") {
		t.Fatalf("layout must still render confidence when the verdict is empty:\n%s", joined)
	}
	// With no verdict the second block falls back to the confidence context line.
	if blocks[1]["type"] != "context" {
		t.Fatalf("block[1] must fall back to the confidence context line, got %v", blocks[1]["type"])
	}
}

func TestEscapeMrkdwn(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "no specials here", "no specials here"},
		{"link_injection", "<https://evil.example|click here to remediate>", "&lt;https://evil.example|click here to remediate&gt;"},
		{"amp_first", "a & b < c > d", "a &amp; b &lt; c &gt; d"},
		{"pre_escaped_stays_literal", "&lt;", "&amp;lt;"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeMrkdwn(tc.in); got != tc.want {
				t.Errorf("escapeMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// mrkdwnTexts collects every mrkdwn text the blocks would send to Slack, so
// tests assert on what Slack will actually parse (json.Marshal would obscure
// this by encoding < and > as </>).
func mrkdwnTexts(blocks []map[string]any) []string {
	var out []string
	grab := func(v any) {
		txt, _ := v.(map[string]any)
		if txt["type"] == "mrkdwn" {
			if s, _ := txt["text"].(string); s != "" {
				out = append(out, s)
			}
		}
	}
	for _, b := range blocks {
		grab(b["text"])
		if els, _ := b["elements"].([]map[string]any); els != nil {
			for _, el := range els {
				grab(el)
			}
		}
	}
	return out
}

// TestSlackBlocksEscapeUntrustedText proves that model/tool-derived fields
// (summaries, evidence quoting cluster logs, change refs, action descriptions,
// unresolved items) cannot inject Slack mrkdwn — most importantly the
// <url|text> link form, a phishing vector in incident notifications — while the
// formatter's own markup (bold, code, the KB link it constructs) keeps working.
func TestSlackBlocksEscapeUntrustedText(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{
			Summary:         "summary with <b> tag",
			Confidence:      0.9,
			ChangeRef:       "chart@<v2>",
			Evidence:        []string{"error: <https://evil.example|click here to remediate>"},
			SuggestedAction: "restart & verify",
			Reversible:      true,
		}},
		Unresolved: []string{"why <img> appears in logs"},
		RuledOut:   []string{"disproven by <script> in logs"},
		DataGaps:   []string{"metrics <unavailable> for db"},
		Actions:    []providers.Action{{Description: "scale down <deploy>", ApprovalID: "a1"}},
		CuratedURL: "https://github.com/o/r/issues/9",
	}
	joined := strings.Join(mrkdwnTexts(append(summaryBlocks(inv), detailBlocks(inv)...)), "\n")

	// The hostile log line must render inert, never as a clickable link.
	if strings.Contains(joined, "<https://evil.example") {
		t.Fatalf("hostile evidence rendered as live mrkdwn link:\n%s", joined)
	}
	for _, want := range []string{
		"&lt;https://evil.example|click here to remediate&gt;",
		"summary with &lt;b&gt; tag",
		"chart@&lt;v2&gt;",     // ChangeRef, surfaced in the metadata fields and the detail RC section
		"restart &amp; verify", // SuggestedAction, surfaced in next steps
		"why &lt;img&gt; appears in logs",
		"scale down &lt;deploy&gt;",
		// RuledOut/DataGaps no longer render in the SUMMARY (findings #2/#3) —
		// both are only in detailBlocks, which is included in `joined` above.
		"disproven by &lt;script&gt; in logs",
		"metrics &lt;unavailable&gt; for db",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("blocks missing escaped untrusted text %q:\n%s", want, joined)
		}
	}
	// Formatter-emitted markup must keep working: bold rank, code confidence,
	// reversibility italics, and the KB link the formatter constructs itself.
	for _, want := range []string{"*1. ", "`90%`", "_(reversible)_", "<https://github.com/o/r/issues/9|view entry>"} {
		if !strings.Contains(joined, want) {
			t.Errorf("blocks missing formatter-emitted markup %q:\n%s", want, joined)
		}
	}
}

// TestEntryLinkEscapesURL proves entryLink escapes its untrusted URL sources
// (PrevCuratedURL, MatchedKnowledge.URL) — the single footer "view entry" link
// (finding #6) must never let a hostile & split or corrupt the mrkdwn link.
func TestEntryLinkEscapesURL(t *testing.T) {
	got := entryLink(providers.Investigation{PrevCuratedURL: "https://kb.example/prev?a=1&b=2"})
	want := "📚 <https://kb.example/prev?a=1&amp;b=2|view entry>"
	if got != want {
		t.Errorf("entryLink(PrevCuratedURL) = %q, want %q", got, want)
	}
}

// TestSlackMessageFallbackEscaped proves the one-line fallback (Slack parses it
// as mrkdwn for notifications) escapes its one untrusted field — the model title
// — so a hostile title cannot inject a clickable phishing link, while the
// verdict label and confidence scaffolding survive untouched.
func TestSlackMessageFallbackEscaped(t *testing.T) {
	inv := sampleInvestigation()
	inv.Title = "<https://evil.example|click here to remediate>"
	text, _ := slackMessage(inv)["text"].(string)
	if strings.Contains(text, "<https://evil.example") {
		t.Fatalf("fallback text carries live mrkdwn link:\n%s", text)
	}
	if !strings.Contains(text, "&lt;https://evil.example|click here to remediate&gt;") {
		t.Fatalf("fallback text missing escaped title:\n%s", text)
	}
	if !strings.Contains(text, "Action required") {
		t.Fatalf("fallback text lost the verdict label:\n%s", text)
	}
}

func TestMultiBestEffort(t *testing.T) {
	var delivered int
	ok := notifierFunc(func(context.Context, providers.Investigation) error { delivered++; return nil })
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), failingNotifier{}, ok)
	// Best-effort: a failing notifier must not stop the good one — but the failure IS
	// surfaced to the caller (joined), so partial delivery is detectable.
	if err := m.Deliver(context.Background(), sampleInvestigation()); err == nil {
		t.Fatal("Multi.Deliver should surface the failing sink's error")
	}
	if delivered != 1 {
		t.Fatalf("good notifier called %d times, want 1", delivered)
	}
}

// notifierFunc adapts a func to providers.Notifier.
type notifierFunc func(context.Context, providers.Investigation) error

func (f notifierFunc) Deliver(ctx context.Context, inv providers.Investigation) error {
	return f(ctx, inv)
}

// TestSlackProgressRenderEscapesInterim proves an interim progress ping renders
// the untrusted model text inert: a hostile <url|text> line must not become a
// clickable link in either the blocks or the plain-text fallback.
func TestSlackProgressRenderEscapesInterim(t *testing.T) {
	up := providers.ProgressUpdate{
		Title:     "HarborDown",
		Step:      5,
		MaxSteps:  20,
		ToolsUsed: map[string]int{"what_changed": 2, "kb_search": 1},
		Interim:   "checking <https://evil.example|click here to remediate>",
	}
	msg := slackProgressMessage(up)
	joined := strings.Join(mrkdwnTexts(msg["blocks"].([]map[string]any)), "\n")
	text, _ := msg["text"].(string)
	for _, s := range []string{joined, text} {
		if strings.Contains(s, "<https://evil.example") {
			t.Fatalf("hostile interim rendered as a live mrkdwn link:\n%s", s)
		}
		if !strings.Contains(s, "&lt;https://evil.example|click here to remediate&gt;") {
			t.Fatalf("interim text was not escaped:\n%s", s)
		}
	}
	// Status context carries the step counter and the tools-used summary.
	if !strings.Contains(joined, "step 5/20") {
		t.Fatalf("progress blocks missing the step counter:\n%s", joined)
	}
	if !strings.Contains(joined, "kb_search×1") || !strings.Contains(joined, "what_changed×2") {
		t.Fatalf("progress blocks missing the tools-used summary:\n%s", joined)
	}
}

// captureProgress is a fake ProgressNotifier recording every ping (and optionally
// failing) — the test double for the capability fan-out.
type captureProgress struct {
	got  []providers.ProgressUpdate
	fail bool
}

func (c *captureProgress) Deliver(context.Context, providers.Investigation) error { return nil }
func (c *captureProgress) DeliverProgress(_ context.Context, up providers.ProgressUpdate) error {
	c.got = append(c.got, up)
	if c.fail {
		return errors.New("boom")
	}
	return nil
}

// TestMultiDeliverProgressCapability proves Multi fans a progress ping only to
// notifiers implementing ProgressNotifier (a plain notifier is skipped), and that
// a failing progress sink is swallowed — a ping never fails an investigation.
func TestMultiDeliverProgressCapability(t *testing.T) {
	plain := notifierFunc(func(context.Context, providers.Investigation) error {
		t.Fatal("a plain (non-progress) notifier must not receive Deliver on a progress ping")
		return nil
	})
	cap1 := &captureProgress{}
	cap2 := &captureProgress{fail: true} // failing sink: must be swallowed
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), plain, cap1, cap2)
	up := providers.ProgressUpdate{Title: "x", Step: 5, MaxSteps: 20}
	if err := m.DeliverProgress(context.Background(), up); err != nil {
		t.Fatalf("DeliverProgress must swallow sink errors, got %v", err)
	}
	if len(cap1.got) != 1 || cap1.got[0].Step != 5 {
		t.Fatalf("progress-capable notifier got %+v, want one ping at step 5", cap1.got)
	}
	if len(cap2.got) != 1 {
		t.Fatalf("a failing progress sink must still be attempted, got %d", len(cap2.got))
	}
}

// blocksText flattens summaryBlocks into one string for containment asserts.
// It encodes with SetEscapeHTML(false): plain json.Marshal would re-escape the
// '&'/'<'/'>' that escapeMrkdwn already turned into &amp;/&lt;/&gt; (e.g. "&lt;"
// becomes "&lt;"), corrupting the containment checks below — the same
// reason mrkdwnTexts exists elsewhere in this file.
func blocksText(t *testing.T, blocks []map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(blocks); err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return buf.String()
}

func TestSlackSummaryBlocksPriorKnowledge(t *testing.T) {
	inv := providers.Investigation{
		Title: "CrashLoopBackOff", Confidence: 0.86,
		Occurrences:    3,
		LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
		Prior: &providers.PriorKnowledge{
			Cause: "ConfigMap truncated <v5.4>", Resolution: "revert & pin 5.3.2",
			Recalls: 3, Resolved: 3,
		},
	}
	txt := blocksText(t, summaryBlocks(inv))
	for _, want := range []string{
		"📚 *Seen before ×3*",
		"*Prior cause:* ConfigMap truncated &lt;v5.4&gt;", // untrusted entry text is escaped
		"*Prior resolution:* revert &amp; pin 5.3.2",
		"resolve rate 3/3",              // track record
		"<https://kb/pr/12|view entry>", // the entry link now lives once, in the footer
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("summary blocks missing %q\n%s", want, txt)
		}
	}
	// The new block replaces the old pointers: no duplicate recurrence renders,
	// and the entry link is never inlined mid-prose (finding #6) — it lives only
	// in the footer.
	for _, absent := range []string{"Previously investigated", "*Recurrence:*", "previous entry"} {
		if strings.Contains(txt, absent) {
			t.Errorf("summary blocks must not still render %q when Prior is set\n%s", absent, txt)
		}
	}
}

// A recall short-circuit must be VISIBLE: the block leads with "⚡ Instant recall"
// (not "Seen before"), labels the quoted sections as the KNOWN answer, links the
// knowledge-base entry, and shows the resolve-rate track record — otherwise the
// notification reads as a low-confidence fresh investigation.
func TestSlackSummaryBlocksRecall(t *testing.T) {
	inv := providers.Investigation{
		Title: "HarborRegistryDown", Confidence: 0.6,
		Recalled:      true,
		RecalledEntry: "harbor-registry-down.md",
		Occurrences:   1, // fresh trigger key — recall framing must not depend on the counter
		Prior: &providers.PriorKnowledge{
			Cause: "AccessKey hit IAM quota", Resolution: "delete an unused access key",
			EntryPath: "harbor-registry-down.md", Recalls: 3, Resolved: 3,
		},
	}
	txt := blocksText(t, summaryBlocks(inv))
	for _, want := range []string{
		"⚡ *Instant recall*",
		"no investigation was run",
		"*Known cause:* AccessKey hit IAM quota",
		"*Validated resolution:* delete an unused access key",
		"resolve rate 3/3",
		"📚 `harbor-registry-down.md`", // entry path shown once, in the footer, when no curated URL
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("recall summary missing %q\n%s", want, txt)
		}
	}
	for _, absent := range []string{"Seen before", "Prior cause"} {
		if strings.Contains(txt, absent) {
			t.Errorf("recall block must not use the fresh-recurrence framing %q\n%s", absent, txt)
		}
	}
	// The bare path must never appear INLINE in the recall block's prose
	// (finding #6 — it is the most-clickable thing on the card, and Slack can
	// truncate a long section mid-word); it belongs only in the footer.
	texts := mrkdwnTexts(summaryBlocks(inv)) // [confidence ctx, prior block, footer]
	if len(texts) < 2 || strings.Contains(texts[1], "harbor-registry-down.md") {
		t.Errorf("entry path must not be inlined in the prior/recall block, got blocks: %v", texts)
	}
	// With a curated URL, the entry links instead of showing the bare path.
	inv.PrevCuratedURL = "https://kb/pr/42"
	txt = blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "<https://kb/pr/42|view entry>") {
		t.Errorf("recall with curated URL must link the knowledge-base entry\n%s", txt)
	}
	if strings.Contains(txt, "harbor-registry-down.md") {
		t.Errorf("a resolvable URL must replace the bare path, not add to it\n%s", txt)
	}
}

// Without Prior, the legacy counter field + context pointer stay untouched.
func TestSlackSummaryBlocksRecurrenceWithoutPrior(t *testing.T) {
	inv := providers.Investigation{
		Title: "CrashLoopBackOff", Confidence: 0.86,
		Occurrences: 2, LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
	}
	txt := blocksText(t, summaryBlocks(inv))
	// The previous-conclusion pointer now lives in the footer's single entry
	// link (finding #6), not as its own "Previously investigated" line.
	if !strings.Contains(txt, "<https://kb/pr/12|view entry>") {
		t.Errorf("footer entry link missing without Prior\n%s", txt)
	}
	if strings.Contains(txt, "Previously investigated") {
		t.Errorf("the legacy inline pointer must be gone (superseded by the footer link)\n%s", txt)
	}
	if strings.Contains(txt, "Seen before") {
		t.Errorf("Seen-before block must not render without Prior\n%s", txt)
	}
}

// The seen-before block's sub-lines are each independently optional: a partial
// PriorKnowledge must render only what it has — no empty labels, no dangling
// footer separator.
func TestSlackSummaryBlocksPriorKnowledgePartial(t *testing.T) {
	cases := []struct {
		label   string
		prior   *providers.PriorKnowledge
		prevURL string
		want    []string
		absent  []string
	}{
		{
			label: "cause only", prior: &providers.PriorKnowledge{Cause: "c"}, prevURL: "https://kb/pr/1",
			want:   []string{"*Prior cause:* c", "<https://kb/pr/1|view entry>"},
			absent: []string{"*Prior resolution:*", "resolve rate"},
		},
		{
			label: "resolution only", prior: &providers.PriorKnowledge{Resolution: "r"}, prevURL: "https://kb/pr/1",
			want:   []string{"*Prior resolution:* r", "<https://kb/pr/1|view entry>"},
			absent: []string{"*Prior cause:*", "resolve rate"},
		},
		{
			label: "track record without link", prior: &providers.PriorKnowledge{Cause: "c", Recalls: 2, Resolved: 1},
			want:   []string{"*Prior cause:* c", "resolve rate 1/2"},
			absent: []string{"view entry"},
		},
	}
	for _, c := range cases {
		inv := providers.Investigation{
			Title: "t", Confidence: 0.8,
			Occurrences:    2,
			LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
			PrevCuratedURL: c.prevURL,
			Prior:          c.prior,
		}
		txt := blocksText(t, summaryBlocks(inv))
		for _, w := range c.want {
			if !strings.Contains(txt, w) {
				t.Errorf("%s: blocks missing %q\n%s", c.label, w, txt)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(txt, a) {
				t.Errorf("%s: blocks must omit %q\n%s", c.label, a, txt)
			}
		}
	}
}

// TestSlackSummaryBlocksMatchedKnowledge covers the visibility fix: when a full
// investigation's kb_search matched a known runbook (MatchedKnowledge set, Prior nil),
// the summary renders a prominent "Matches known runbook" block with the escaped
// title + path, so the on-call sees RunLore already had knowledge for the incident.
func TestSlackSummaryBlocksMatchedKnowledge(t *testing.T) {
	inv := providers.Investigation{
		Title: "HarborProbeFailure", Confidence: 0.8,
		MatchedKnowledge: &providers.MatchedEntry{
			Title: "Harbor probe & TLS <runbook>", // untrusted entry text must be escaped
			Path:  "runbooks/harbor.md", Score: 6.2,
		},
	}
	txt := blocksText(t, summaryBlocks(inv))
	for _, want := range []string{
		"📚 *Matches known runbook:* Harbor probe &amp; TLS &lt;runbook&gt;",
		"`runbooks/harbor.md`",
		"RunLore has prior knowledge for this incident.",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("summary blocks missing %q\n%s", want, txt)
		}
	}
}

// TestSlackSummaryBlocksMatchedKnowledgeURL: when a web URL is derivable it
// renders as a link in the footer (finding #6 — never inlined as a path) instead
// of an inline code path.
func TestSlackSummaryBlocksMatchedKnowledgeURL(t *testing.T) {
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8,
		MatchedKnowledge: &providers.MatchedEntry{Title: "Runbook", Path: "runbooks/h.md", URL: "https://kb/runbooks/h.md", Score: 5},
	}
	txt := blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "<https://kb/runbooks/h.md|view entry>") {
		t.Errorf("expected a linked entry, got\n%s", txt)
	}
	if strings.Contains(txt, "runbooks/h.md|runbooks/h.md") {
		t.Errorf("must not use the path as the link label:\n%s", txt)
	}
}

// TestSlackSummaryBlocksMatchedKnowledgeOmitted: the block is absent when there is
// no match, and — crucially — suppressed when Prior is set, so recurrence and
// existing-KB never double-render.
func TestSlackSummaryBlocksMatchedKnowledgeOmitted(t *testing.T) {
	// No match at all.
	bare := blocksText(t, summaryBlocks(providers.Investigation{Title: "t", Confidence: 0.8}))
	if strings.Contains(bare, "Matches known runbook") {
		t.Errorf("must not render the block without MatchedKnowledge\n%s", bare)
	}
	// Match present but Prior set → the Seen-before block covers it; suppress this one.
	withPrior := providers.Investigation{
		Title: "t", Confidence: 0.8, Occurrences: 3,
		LastOccurrence:   time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		Prior:            &providers.PriorKnowledge{Cause: "c"},
		MatchedKnowledge: &providers.MatchedEntry{Title: "Runbook", Path: "runbooks/h.md", Score: 9},
	}
	txt := blocksText(t, summaryBlocks(withPrior))
	if strings.Contains(txt, "Matches known runbook") {
		t.Errorf("must NOT render the existing-KB block when Prior is set (double-render)\n%s", txt)
	}
	if !strings.Contains(txt, "Seen before") {
		t.Errorf("expected the recurrence block to render instead\n%s", txt)
	}
}

// marshalBlocks JSON-encodes a Slack payload for substring assertions on
// structures (action_ids, button values) the mrkdwn text helpers don't reach.
func marshalBlocks(t *testing.T, msg map[string]any) string {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestSlackFeedbackButtonsOptIn pins the opt-in contract: no feedback block by
// default; when enabled, both 👍/👎 buttons render (independently of any
// ApprovalID actions) keyed by TriggerKey, falling back to the fingerprint, and
// are omitted entirely when the investigation carries neither.
func TestSlackFeedbackButtonsOptIn(t *testing.T) {
	inv := providers.Investigation{Title: "t", TriggerKey: "k"}

	if s := marshalBlocks(t, slackMessage(inv)); strings.Contains(s, "runlore_feedback") {
		t.Fatal("feedback buttons must NOT render when the option is off (default)")
	}

	on := marshalBlocks(t, slackMessageWith(inv, true))
	if !strings.Contains(on, `"action_id":"runlore_feedback_up"`) ||
		!strings.Contains(on, `"action_id":"runlore_feedback_down"`) {
		t.Fatalf("both feedback buttons must render when opted in, got: %s", on)
	}
	if !strings.Contains(on, `"value":"k"`) {
		t.Fatalf("buttons must carry the TriggerKey as value, got: %s", on)
	}

	byFP := marshalBlocks(t, slackMessageWith(providers.Investigation{Title: "t", Fingerprint: "fp1"}, true))
	if !strings.Contains(byFP, `"value":"fp1"`) {
		t.Fatalf("buttons must fall back to the fingerprint, got: %s", byFP)
	}

	if s := marshalBlocks(t, slackMessageWith(providers.Investigation{Title: "t"}, true)); strings.Contains(s, "runlore_feedback") {
		t.Fatal("no TriggerKey and no fingerprint ⇒ nothing to attribute ⇒ no buttons")
	}
}

// TestSlackBotDeliverFeedbackButtons: with the option on, the bot path renders
// the buttons on the channel summary message (never in the detail thread).
func TestSlackBotDeliverFeedbackButtons(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
	}))
	defer srv.Close()

	bot := NewSlackBot("xoxb-t", "#ops")
	bot.baseURL = srv.URL
	bot.FeedbackButtons = true
	inv := sampleInvestigation()
	inv.TriggerKey = "k"
	if err := bot.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(bodies) == 0 {
		t.Fatal("no message posted")
	}
	if !strings.Contains(bodies[0], "runlore_feedback_up") {
		t.Fatalf("summary message must carry the feedback buttons, got: %s", bodies[0])
	}
	for _, b := range bodies[1:] {
		if strings.Contains(b, "runlore_feedback") {
			t.Fatalf("detail thread must not repeat the buttons, got: %s", b)
		}
	}
}
