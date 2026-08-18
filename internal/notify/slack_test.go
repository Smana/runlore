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

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
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
	// Finding #1, as corrected: the verdict line carries the verdict plus the
	// investigation's own title, because this fixture sets AlertName — so the
	// header anchored on "HarborDown", the alert, and NOT on the title.
	//
	// The original assertion here was "the verdict line must carry the verdict
	// ALONE — the header above already spelled out the title/alert name in full".
	// That holds only when no alert named the investigation. With an alert, the
	// header shows the question that woke the on-call and the title is the answer
	// RunLore reached; suppressing it left the card never stating its conclusion.
	// TestSlackCardShowsConclusionOnAlertPath pins both directions.
	texts := mrkdwnTexts(blocks)
	if !strings.Contains(texts[0], "harbor is degraded") {
		t.Fatalf("verdict block must carry the title when the header anchored on the alert name:\n%s", texts[0])
	}
	if strings.Count(s, "harbor is degraded") != 1 {
		t.Fatalf("the title must render exactly once on the card, got %d:\n%s", strings.Count(s, "harbor is degraded"), s)
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
//
// The rule is "the summary's fold space goes to the ANSWER", so it holds exactly
// while there IS an answer — which is why this fixture carries a root cause and is
// not merely an incidental detail of it. On an `inconclusive` card with no cause,
// block 5b promotes the data gaps (and, failing those, the ruled-out list) into the
// summary precisely because nothing is being pushed below the fold; that arm is
// pinned by TestInconclusiveSummaryCardAccountsForItself in
// inconclusive_account_test.go. Keep the root cause here when editing this fixture,
// or this test stops proving anything.
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

// TestSlackWhatChangedOmittedWhenUnknown: an absent ChangeRef renders NO
// "What changed" field at all.
//
// This test previously asserted the opposite — that the card printed
// "No Git change identified — likely infrastructure-induced". That was a
// fabricated conclusion. change_ref is OPTIONAL in submit_findings, and
// WhatChangedTool is only registered when a GitOps provider is configured, so in
// a deployment without Flux/Argo the clause fired on EVERY card, and on recall
// cards where no investigation ran at all. An empty optional field is not
// evidence of absence, and "likely infrastructure-induced" is an inference the
// investigation never made — it points a woken-up on-call away from the recent
// deploy, the first thing they should check.
//
// The fixture's own "infra fault" summary shows the unsound inference had been
// baked into the test as well as the renderer.
func TestSlackWhatChangedOmittedWhenUnknown(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		RootCauses: []providers.Hypothesis{{Summary: "some cause"}}, // no ChangeRef
	}
	txt := blocksText(t, summaryBlocks(inv))
	if strings.Contains(txt, "What changed") {
		t.Errorf("the card must not render a \"What changed\" field when the investigation never established one:\n%s", txt)
	}
	if strings.Contains(txt, "infrastructure-induced") {
		t.Errorf("the card must not infer a cause from an absent optional field:\n%s", txt)
	}

	// Control: when the investigation DID establish a change, it is rendered.
	inv.RootCauses[0].ChangeRef = "apps/harbor/values.yaml: tag 1.14.2 -> 1.15.0"
	txt = blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "What changed") || !strings.Contains(txt, "1.15.0") {
		t.Errorf("an established change must still be rendered:\n%s", txt)
	}
}

// TestSlackCardDoesNotRestateTheAlertAsItsConclusion: inv.Title falls back to
// req.Title, and for Alertmanager and Grafana req.Title IS labels.alertname —
// and every synthesised terminal result (non-convergence, budget, timeout,
// refusal) sets Title: req.Title unconditionally. Without a guard the header and
// the verdict line print the same string twice, which is #399's original finding
// and #409's stated failure, on precisely the inconclusive cards where restating
// the alert as the answer misleads most.
func TestSlackCardDoesNotRestateTheAlertAsItsConclusion(t *testing.T) {
	inv := providers.Investigation{
		AlertName:  "KubePodCrashLooping",
		Title:      "KubePodCrashLooping", // the loop's fallback: same string
		Verdict:    "action_required",
		Confidence: 0.8,
	}
	txt := blocksText(t, summaryBlocks(inv))
	// The header and the *Alert:* metadata field are both legitimate occurrences.
	// The defect is specifically the VERDICT line appending it as the conclusion.
	if strings.Contains(txt, "Action required*\u0026mdash;") || strings.Contains(txt, `Action required* \u2014 KubePodCrashLooping`) {
		t.Errorf("the verdict line restated the alert name as the card's own conclusion:\n%s", txt)
	}
	if strings.Contains(txt, "*Action required* — KubePodCrashLooping") {
		t.Errorf("the verdict line restated the alert name as the card's own conclusion:\n%s", txt)
	}

	// Control: a genuine conclusion distinct from the alert IS shown.
	inv.Title = "Harbor chart bump deadlocked the DB migration"
	txt = blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "Harbor chart bump deadlocked") {
		t.Errorf("a real conclusion must still be rendered alongside the verdict:\n%s", txt)
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
// this by encoding < and > as \u003c/\u003e).
//
// It walks text, elements AND fields. Omitting fields made the entire
// metadataFields block — Severity, AlertName, Tenant, Cluster, Resource,
// ChangeRef, all of them untrusted — invisible to every test using this helper,
// including the escaping test. Deleting escapeMrkdwn from any of those sites left
// the suite green.
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
	// Slack block bodies are built as []map[string]any here but decode as []any
	// through JSON; accept both so the helper can't go blind on a shape change.
	grabList := func(v any) {
		switch els := v.(type) {
		case []map[string]any:
			for _, el := range els {
				grab(el)
			}
		case []any:
			for _, el := range els {
				grab(el)
			}
		}
	}
	for _, b := range blocks {
		grab(b["text"])
		grabList(b["elements"])
		grabList(b["fields"])
	}
	return out
}

// TestSlackBlocksEscapeUntrustedText proves that model/tool-derived fields
// (summaries, evidence quoting cluster logs, change refs, action descriptions,
// unresolved items, and every metadata field) cannot inject Slack mrkdwn — most
// importantly the <url|text> link form, a phishing vector in incident
// notifications — while the formatter's own markup keeps working.
//
// Every untrusted field carries its OWN marker, and every marker is asserted in
// BOTH directions: the raw form absent, the escaped form present.
//
// That pairing is the point. Most of these fields render at two sites (the summary
// card and the detail blocks, or a metadata field and a section), so a
// "the escaped form appears somewhere in the joined payload" assertion passes when
// escapeMrkdwn is deleted at ONE of them — the other site still supplies it. With a
// per-field marker and a negative assertion, a single-site regression leaves its raw
// marker in the payload and fails. #412 caught exactly that regression for Title
// after it shipped; this is the shape that catches it for the other eleven sites.
func TestSlackBlocksEscapeUntrustedText(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.9,
		// AlertName + Title together exercise the card's conclusion line. Title must
		// differ from AlertName or the conclusion line is (correctly) suppressed as a
		// restatement of the alert.
		AlertName: "KubePodCrashLooping<alertname>",
		Title:     "root cause: <https://evil.example|click here to remediate>",
		Verdict:   providers.VerdictActionRequired,
		Severity:  "critical<sev>",
		Tenant:    "team<tenant>",
		Cluster:   "prod<cluster>",
		Resource:  providers.Workload{Kind: "Deployment", Namespace: "apps<ns>", Name: "web<name>"},
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

	// Each untrusted field, checked BOTH ways. raw must be absent everywhere;
	// escaped must be present. A single-site escaping regression fails on `raw`.
	for _, tc := range []struct{ name, raw, escaped string }{
		{"Title (conclusion line)", "<https://evil.example|click", "&lt;https://evil.example|click here to remediate&gt;"},
		{"root-cause Summary", "<b> tag", "summary with &lt;b&gt; tag"},
		{"ChangeRef (metadata field)", "chart@<v2>", "chart@&lt;v2&gt;"},
		{"SuggestedAction (next steps)", "restart & verify", "restart &amp; verify"},
		{"Unresolved", "<img> appears", "why &lt;img&gt; appears in logs"},
		{"RuledOut", "<script> in logs", "disproven by &lt;script&gt; in logs"},
		{"DataGaps", "<unavailable> for db", "metrics &lt;unavailable&gt; for db"},
		{"Action.Description", "<deploy>", "scale down &lt;deploy&gt;"},
		{"Severity (metadata field)", "critical<sev>", "critical&lt;sev&gt;"},
		{"AlertName (metadata field)", "KubePodCrashLooping<alertname>", "KubePodCrashLooping&lt;alertname&gt;"},
		{"Tenant (metadata field)", "team<tenant>", "team&lt;tenant&gt;"},
		{"Cluster (metadata field)", "prod<cluster>", "prod&lt;cluster&gt;"},
		{"Resource namespace (metadata field)", "apps<ns>", "apps&lt;ns&gt;"},
		{"Resource name (metadata field)", "web<name>", "web&lt;name&gt;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(joined, tc.raw) {
				t.Errorf("%s reached Slack UNESCAPED (raw %q present):\n%s", tc.name, tc.raw, joined)
			}
			if !strings.Contains(joined, tc.escaped) {
				t.Errorf("%s missing its escaped form %q:\n%s", tc.name, tc.escaped, joined)
			}
		})
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

// TestSlackCardShowsCurateFailure pins the SLACK surface of #506 — the one the
// issue actually reports ("The delivered Slack card carried no indication of
// this"). It is a separate test from the notify.Format one on purpose: nothing on
// the Slack path calls Format. Slack.Deliver and SlackBot.Deliver both go through
// slackMessageWith → summaryBlocks + detailBlocks + fallbackText, so a curate
// failure rendered only by Format is rendered nowhere a Slack reader can see it.
//
// The failure line and the "view entry" link are asserted TOGETHER: on a recurring
// incident entryLink still resolves the PRIOR entry, so the two coexist, and a card
// showing a stale link with no warning is the exact ambiguity #506 is about.
func TestSlackCardShowsCurateFailure(t *testing.T) {
	inv := sampleInvestigation()
	inv.CurateError = "open PR: github GET /repos/o/r/git/ref/heads/main: status 403: Resource not accessible by integration"
	joined := strings.Join(mrkdwnTexts(summaryBlocks(inv)), "\n")
	if !strings.Contains(joined, "could not save to the knowledge base") {
		t.Fatalf("a failed knowledge write is invisible on the Slack card:\n%s", joined)
	}
	if !strings.Contains(joined, "403") {
		t.Fatalf("the reason must say WHICH failure it was, so an operator can act:\n%s", joined)
	}
	if !strings.Contains(joined, "view entry") {
		t.Fatalf("the prior entry link must survive alongside the warning:\n%s", joined)
	}
	// The line is provenance, so it belongs in the footer context block beside
	// entryLink — not in the answer, which the woken-up on-call reads first.
	if !strings.Contains(joined, "view entry>  ·  ⚠️ could not save to the knowledge base") {
		t.Fatalf("the warning is not on the footer line beside the entry link:\n%s", joined)
	}
}

// TestSlackCurateFailureIsEscaped is mandatory rather than defensive: the curate
// reason is forge-supplied text, and without escapeMrkdwn it would be the ONLY
// unescaped untrusted span on the card — a 403 body echoing
// <https://evil.example|click here> would render as a live link in the incident
// channel, which is a phishing vector wearing RunLore's own warning glyph.
func TestSlackCurateFailureIsEscaped(t *testing.T) {
	const hostile = "<https://evil.example|click here to fix it>"
	inv := sampleInvestigation()
	inv.CurateError = "open PR: " + hostile
	joined := strings.Join(mrkdwnTexts(summaryBlocks(inv)), "\n")
	if strings.Contains(joined, hostile) {
		t.Fatalf("forge error text rendered as a LIVE mrkdwn link on the card:\n%s", joined)
	}
	if !strings.Contains(joined, "&lt;https://evil.example|click here to fix it&gt;") {
		t.Fatalf("curate reason neither escaped nor present:\n%s", joined)
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

// TestSlackCardShowsConclusionOnAlertPath pins the investigation's own title onto
// the card for alert-triggered incidents.
//
// The header anchors on the ALERT NAME whenever the source supplied one, so on the
// Alertmanager path — the primary trigger — it renders the question that woke the
// on-call, not the answer RunLore reached. A layout change once dropped the title
// on the reasoning that "the header already shows it", which is true only for the
// alert-less path. The result was a card that never stated its own conclusion.
func TestSlackCardShowsConclusionOnAlertPath(t *testing.T) {
	const conclusion = "api crash-looping after the payments/api chart bump"

	t.Run("alert named: header shows the alert, so the title must appear too", func(t *testing.T) {
		inv := providers.Investigation{
			Title:     conclusion,
			AlertName: "KubePodCrashLooping",
			Verdict:   providers.VerdictActionRequired,
			Resource:  providers.Workload{Kind: "Deployment", Name: "api", Namespace: "payments"},
		}
		body := renderSlackBlocks(t, inv)
		if !strings.Contains(body, conclusion) {
			t.Errorf("the card never states its own conclusion — %q is absent:\n%s", conclusion, body)
		}
		if !strings.Contains(body, "KubePodCrashLooping") {
			t.Errorf("the alert name should still anchor the header:\n%s", body)
		}
	})

	t.Run("no alert: header IS the title, so it must not be repeated", func(t *testing.T) {
		inv := providers.Investigation{
			Title:    conclusion,
			Verdict:  providers.VerdictActionRequired,
			Resource: providers.Workload{Kind: "Deployment", Name: "api", Namespace: "payments"},
		}
		body := renderSlackBlocks(t, inv)
		if n := strings.Count(body, conclusion); n != 1 {
			t.Errorf("title should appear exactly once (header only), got %d:\n%s", n, body)
		}
	})

	t.Run("no verdict: the conclusion still appears", func(t *testing.T) {
		inv := providers.Investigation{
			Title:     conclusion,
			AlertName: "KubePodCrashLooping",
			Resource:  providers.Workload{Kind: "Deployment", Name: "api", Namespace: "payments"},
		}
		body := renderSlackBlocks(t, inv)
		if !strings.Contains(body, conclusion) {
			t.Errorf("a verdict-less investigation still has a conclusion; %q is absent:\n%s", conclusion, body)
		}
	})
}

// renderSlackBlocks marshals just the summary blocks, so a title appearing only in
// the detail thread or the push-notification fallback cannot satisfy the assertions
// above — the card itself has to carry it.
func renderSlackBlocks(t *testing.T, inv providers.Investigation) string {
	t.Helper()
	b, err := json.Marshal(summaryBlocks(inv))
	if err != nil {
		t.Fatalf("marshal summary blocks: %v", err)
	}
	return string(b)
}

// TestSlackCardEscapesTitleOnBothBranches covers the two places the card can put
// the model's title: appended to the verdict line, and standing alone when the
// model gave no verdict. Both carry untrusted model output.
//
// This exists because the first attempt to cover it was VACUOUS: the untrusted-text
// fixture carried no Verdict, so only the standalone branch ever ran, and deleting
// escapeMrkdwn from the verdict line passed the entire suite.
func TestSlackCardEscapesTitleOnBothBranches(t *testing.T) {
	const hostile = "<https://evil.example|click here to remediate>"
	for _, tc := range []struct {
		name    string
		verdict providers.Verdict
	}{
		{"verdict present, title appended to the verdict line", providers.VerdictActionRequired},
		{"no verdict, title stands alone", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := providers.Investigation{
				AlertName: "KubePodCrashLooping",
				Title:     hostile,
				Verdict:   tc.verdict,
			}
			joined := strings.Join(mrkdwnTexts(summaryBlocks(inv)), "\n")
			if strings.Contains(joined, hostile) {
				t.Fatalf("model title rendered as a LIVE mrkdwn link — phishing vector on the card:\n%s", joined)
			}
			if !strings.Contains(joined, "&lt;https://evil.example|click here to remediate&gt;") {
				t.Fatalf("title neither escaped nor present:\n%s", joined)
			}
		})
	}
}

// TestSlackDeliveryTarget pins what the Slack builder resolves against the
// environment — the fact a startup guard needs in order to warn instead of
// announcing feedback buttons nobody will ever see. The set-but-EMPTY cases are
// the live gap: an unmounted secret leaves the env var present and empty, the
// builder returns nil, the notifier is skipped and nothing is delivered.
type recordingThreadSink struct {
	transport, root, channel string
	inv                      providers.Investigation
	calls                    int
}

func (r *recordingThreadSink) Register(transport, root, channel string, inv providers.Investigation) {
	r.transport, r.root, r.channel, r.inv, r.calls = transport, root, channel, inv, r.calls+1
}

// TestSlackBotRegistersTheThreadRoot proves the registered thread root is the
// SUMMARY post's ts, never the detail reply's — the mock deliberately returns a
// different ts per call (111.222 then 999.888) so a regression that captured the
// detail reply's ts instead (e.g. registration moved to after the detail post,
// reusing an overwritten ts variable) is caught: with a single shared ts for
// both calls, that regression would be invisible.
func TestSlackBotRegistersTheThreadRoot(t *testing.T) {
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		posts = append(posts, m)
		if len(posts) == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":true,"ts":"999.888"}`))
		}
	}))
	defer srv.Close()

	sink := &recordingThreadSink{}
	b := NewSlackBot("xoxb-test", "C1")
	b.baseURL = srv.URL
	b.Threads = sink

	// Unresolved forces a non-empty detailBlocks, so Deliver actually posts a
	// second (threaded) message — without it this investigation has no detail
	// beyond the summary and the discriminating half of this test never runs.
	inv := providers.Investigation{
		Title:      "OOMKilled",
		TriggerKey: "tk-1",
		CuratedURL: "https://github.com/o/r/pull/42",
		Unresolved: []string{"why the pod restarted a second time"},
	}
	if err := b.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if sink.calls != 1 {
		t.Fatalf("Register calls = %d, want 1 (the summary root only, never the detail reply)", sink.calls)
	}
	if sink.root != "111.222" {
		t.Errorf("root = %q, want 111.222 (the summary's ts, not the detail reply's 999.888)", sink.root)
	}
	if sink.channel != "C1" {
		t.Errorf("channel = %q, want C1", sink.channel)
	}
	if sink.transport != "slack" {
		t.Errorf("transport = %q, want slack", sink.transport)
	}
	if sink.inv.TriggerKey != "tk-1" {
		t.Errorf("investigation not passed through: %+v", sink.inv)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d POSTs, want 2 (summary + detail reply)", len(posts))
	}
	if posts[1]["thread_ts"] != "111.222" {
		t.Errorf("detail reply thread_ts = %v, want 111.222 (the summary's ts) — registration must not disturb the detail post", posts[1]["thread_ts"])
	}
}

// TestSlackBotRegistersThreadRootDespiteFailedDetailPost proves the requirement
// TestSlackBotRegistersTheThreadRoot cannot prove on its own: registration must
// happen BEFORE the detail post, so a failed detail post cannot cost it. With
// both mocked posts succeeding (as in every other test in this file), a
// regression that moved the Register call to AFTER the detail-post block —
// while still referencing the same ts variable — would leave calls==1,
// root=="111.222", 2 POSTs and thread_ts=="111.222" all still passing: nothing
// exercises the failure the ordering exists to protect against.
//
// Here the mock's SECOND post (the detail reply) returns a Slack logical
// failure ({"ok":false}, which SlackBot.post treats as an error just like a
// non-2xx status). Deliver must still have registered the summary's ts exactly
// once: with Register before the detail post that holds regardless of the
// detail post's outcome; with Register moved after it, the early return on the
// detail post's error means Register is never reached and calls stays 0.
func TestSlackBotRegistersThreadRootDespiteFailedDetailPost(t *testing.T) {
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

	sink := &recordingThreadSink{}
	b := NewSlackBot("xoxb-test", "C1")
	b.baseURL = srv.URL
	b.Threads = sink

	// Unresolved forces a non-empty detailBlocks, so Deliver actually attempts
	// the second (threaded) post that then fails.
	inv := providers.Investigation{
		Title:      "OOMKilled",
		TriggerKey: "tk-1",
		Unresolved: []string{"why the pod restarted a second time"},
	}
	err := b.Deliver(context.Background(), inv)
	if err == nil {
		t.Fatal("a failed detail post must surface an error")
	}
	if posts != 2 {
		t.Fatalf("got %d POSTs, want 2 (summary + the failed detail attempt)", posts)
	}
	if sink.calls != 1 {
		t.Fatalf("Register calls = %d, want 1 — the summary's thread root must be registered even though the detail post failed", sink.calls)
	}
	if sink.root != "111.222" {
		t.Errorf("root = %q, want 111.222 (the summary's ts)", sink.root)
	}
}

func TestSlackBotDeliverSucceedsWithNoThreadSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
	}))
	defer srv.Close()

	b := NewSlackBot("xoxb-test", "C1")
	b.baseURL = srv.URL
	// Threads deliberately left nil.
	if err := b.Deliver(context.Background(), providers.Investigation{Title: "OOMKilled"}); err != nil {
		t.Fatalf("Deliver with a nil thread sink: %v", err)
	}
}

func TestSlackBotReplyInThread(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"333.444"}`))
	}))
	defer srv.Close()

	b := NewSlackBot("xoxb-test", "C-default")
	b.baseURL = srv.URL
	if err := b.ReplyInThread(context.Background(), "111.222", "C-live", "📝 Noted"); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	if got["thread_ts"] != "111.222" {
		t.Errorf("thread_ts = %v, want 111.222", got["thread_ts"])
	}
	if got["channel"] != "C-live" {
		t.Errorf("channel = %v, want C-live (the live event's channel, not the configured default)", got["channel"])
	}
	if got["text"] != "📝 Noted" {
		t.Errorf("text = %v, want the reply text", got["text"])
	}
}

func TestMultiThreadRepliersAreTransportScoped(t *testing.T) {
	bot := NewSlackBot("xoxb-test", "C1")
	mx := NewMatrix("https://hs.example.org", "!room:example.org", "tok")
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewSlack("https://hooks.slack.com/x"), bot, mx)

	got := m.ThreadRepliers()
	if len(got) != 2 {
		t.Fatalf("ThreadRepliers = %d entries, want 2", len(got))
	}
	if got["slack"] != providers.ThreadNotifier(bot) {
		t.Errorf("slack replier = %v, want the SlackBot", got["slack"])
	}
	if got["matrix"] != providers.ThreadNotifier(mx) {
		t.Errorf("matrix replier = %v, want the Matrix notifier", got["matrix"])
	}
}

func TestMultiThreadRepliersEmptyWhenNoneCanReply(t *testing.T) {
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewSlack("https://hooks.slack.com/x"))
	if got := m.ThreadRepliers(); len(got) != 0 {
		t.Fatalf("ThreadRepliers = %v, want empty", got)
	}
}

// TestMultiThreadRepliersLogsTransportCollision pins the collision-handling
// requirement: two notifiers reporting the same transport must not silently
// drop one (the same class of bug the transport-keyed lookup exists to fix).
// The first-registered notifier wins and the collision is logged, naming the
// transport.
func TestMultiThreadRepliersLogsTransportCollision(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	first := NewSlackBot("xoxb-first", "C1")
	second := NewSlackBot("xoxb-second", "C2")
	m := NewMulti(log, first, second)

	got := m.ThreadRepliers()
	if len(got) != 1 {
		t.Fatalf("ThreadRepliers = %d entries, want 1 (collision keeps only the first)", len(got))
	}
	if got["slack"] != providers.ThreadNotifier(first) {
		t.Errorf("slack replier = %v, want the first-registered SlackBot", got["slack"])
	}
	out := buf.String()
	if !strings.Contains(out, "slack") {
		t.Fatalf("must log a warning naming the colliding transport; got: %s", out)
	}
}

func TestSlackDeliveryTarget(t *testing.T) {
	tests := []struct {
		name string
		sl   config.SlackNotify
		env  map[string]string
		want string
	}{
		{name: "nothing configured", want: ""},
		{
			name: "incoming webhook, env present",
			sl:   config.SlackNotify{WebhookURLEnv: "TEST_SLACK_WEBHOOK_URL"},
			env:  map[string]string{"TEST_SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/x"},
			want: "webhook",
		},
		{
			name: "incoming webhook, env set but empty",
			sl:   config.SlackNotify{WebhookURLEnv: "TEST_SLACK_WEBHOOK_URL"},
			env:  map[string]string{"TEST_SLACK_WEBHOOK_URL": ""},
			want: "",
		},
		{
			name: "bot token + channel, env present",
			sl:   config.SlackNotify{BotTokenEnv: "TEST_SLACK_BOT_TOKEN", Channel: "C123"},
			env:  map[string]string{"TEST_SLACK_BOT_TOKEN": "xoxb-test"},
			want: "bot",
		},
		{
			// The bot token wins when configured, so an empty one does NOT silently
			// fall back to a configured webhook — pinned, because that asymmetry is
			// exactly the shape an operator misreads as "the webhook still works".
			name: "bot token set but empty, webhook configured",
			sl:   config.SlackNotify{BotTokenEnv: "TEST_SLACK_BOT_TOKEN", Channel: "C123", WebhookURLEnv: "TEST_SLACK_WEBHOOK_URL"},
			env:  map[string]string{"TEST_SLACK_BOT_TOKEN": "", "TEST_SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/x"},
			want: "",
		},
		{
			name: "bot token without a channel falls through to the webhook",
			sl:   config.SlackNotify{BotTokenEnv: "TEST_SLACK_BOT_TOKEN", WebhookURLEnv: "TEST_SLACK_WEBHOOK_URL"},
			env:  map[string]string{"TEST_SLACK_BOT_TOKEN": "xoxb-test", "TEST_SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/x"},
			want: "webhook",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := SlackDeliveryTarget(tc.sl); got != tc.want {
				t.Fatalf("SlackDeliveryTarget = %q, want %q", got, tc.want)
			}
			// The builder must agree with the guard: a resolved target builds a
			// notifier, an unresolved one is skipped.
			cfg := &config.Config{}
			cfg.Notify.Slack = tc.sl
			n, err := registry["slack"].Build(Deps{Cfg: cfg, Log: discardLog})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if (n != nil) != (tc.want != "") {
				t.Fatalf("builder returned notifier=%v, but SlackDeliveryTarget said %q", n != nil, tc.want)
			}
		})
	}
}

// TestSlackBotReplyInThreadEscapesUntrustedSpansOnly closes the finding that
// ReplyInThread posted its text raw while every other Slack text path escaped
// first. Thread replies now carry model prose — and the model composes Slack
// markup: "<https://evil.example/reauth|https://github.com/acme/kb/pull/7>"
// renders as a clickable link whose VISIBLE text is a trusted knowledge-base
// URL, in the very thread where RunLore posts genuine ones, and "<!channel>"
// pings everyone in the room.
//
// Both halves are asserted, because escaping the assembled message would trade
// one bug for another: mrkdwnEscaper turns ">" into "&gt;", and the reply's
// blockquote markers — the control that keeps model words distinguishable from
// RunLore's own status lines — are ">" characters RunLore itself wrote. So is
// the "<text>" inside FreeformNotRecordedReply's example. Only the spans
// thread.Untrusted marked may be escaped.
func TestSlackBotReplyInThreadEscapesUntrustedSpansOnly(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"333.444"}`))
	}))
	defer srv.Close()

	b := NewSlackBot("xoxb-test", "C-default")
	b.baseURL = srv.URL
	reply := "> " + thread.Untrusted("Re-auth: <https://evil.example/reauth|https://github.com/acme/kb/pull/7>") + "\n" +
		"> " + thread.Untrusted("<!channel>") + "\n" +
		"📝 Noted on the knowledge-base PR #42 — https://github.com/o/r/pull/42\n" +
		thread.FreeformNotRecordedReply
	if err := b.ReplyInThread(context.Background(), "111.222", "C-live", reply); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	text, _ := got["text"].(string)
	// The untrusted spans are inert.
	for _, live := range []string{"<https://evil.example/reauth|", "<!channel>"} {
		if strings.Contains(text, live) {
			t.Errorf("untrusted model prose reached Slack as live mrkdwn %q:\n%s", live, text)
		}
	}
	for _, want := range []string{
		"&lt;https://evil.example/reauth|https://github.com/acme/kb/pull/7&gt;",
		"&lt;!channel&gt;",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("untrusted span was not escaped, want %q in:\n%s", want, text)
		}
	}
	// RunLore's own framing still renders: the blockquote markers, the real
	// knowledge-base URL, and the backticked example with its angle brackets.
	for _, line := range strings.Split(text, "\n")[:2] {
		if !strings.HasPrefix(line, "> ") {
			t.Errorf("a blockquote marker was escaped away, so model prose reads as RunLore's own: %q", line)
		}
	}
	if !strings.Contains(text, "📝 Noted on the knowledge-base PR #42 — https://github.com/o/r/pull/42") {
		t.Errorf("RunLore's own status line must survive verbatim:\n%s", text)
	}
	if !strings.Contains(text, "`note: <text>`") {
		t.Errorf("RunLore's own backticked example must survive verbatim:\n%s", text)
	}
}

// TestThreadRepliersLeaveNoUntrustedMarksInThePostedText is the guard on the
// in-band signal itself: thread.Untrusted marks spans with a private-use rune
// that must never reach a human's screen. A Replier that forgets to call
// thread.RenderReply would post it. Both transports are asserted here, from
// the providers.ThreadNotifier surface, so a third one added later fails this
// the moment it is wired into Multi.
func TestThreadRepliersLeaveNoUntrustedMarksInThePostedText(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, string(body))
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1","event_id":"$1"}`))
	}))
	defer srv.Close()

	bot := NewSlackBot("xoxb-test", "C1")
	bot.baseURL = srv.URL
	mx := NewMatrix(srv.URL, "!room:example.org", "tok")
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), bot, mx)

	repliers := m.ThreadRepliers()
	if len(repliers) == 0 {
		t.Fatal("test setup: no thread repliers to check")
	}
	for transport, tn := range repliers {
		sent = nil
		if err := tn.ReplyInThread(context.Background(), "$root", "", "> "+thread.Untrusted("hello")); err != nil {
			t.Fatalf("%s ReplyInThread: %v", transport, err)
		}
		if len(sent) != 1 {
			t.Fatalf("%s: sent %d requests, want 1", transport, len(sent))
		}
		// The mark is JSON-encoded as a \ue000 escape, so both forms are checked.
		for _, mark := range []string{"\ue000", `\ue000`} {
			if strings.Contains(sent[0], mark) {
				t.Errorf("%s posted an untrusted-span mark to the chat system — ReplyInThread must call thread.RenderReply:\n%s", transport, sent[0])
			}
		}
	}
}

// captureKBUpdate is a fake KBUpdateNotifier recording every announcement (and
// optionally failing) — the test double for the KB-update capability fan-out,
// mirroring captureProgress above.
type captureKBUpdate struct {
	got  []providers.KBUpdate
	fail bool
}

func (c *captureKBUpdate) Deliver(context.Context, providers.Investigation) error { return nil }
func (c *captureKBUpdate) DeliverKBUpdate(_ context.Context, up providers.KBUpdate) error {
	c.got = append(c.got, up)
	if c.fail {
		return errors.New("boom")
	}
	return nil
}

// TestMultiDeliverKBUpdateCapability proves Multi announces a knowledge-base
// write only to notifiers implementing KBUpdateNotifier — a plain notifier is
// SKIPPED, not called and not crashed on — and that a failing sink is attempted
// and then swallowed. The write already landed on the forge before this call:
// a broadcast that propagated an error would report a failure for something
// that in fact succeeded.
func TestMultiDeliverKBUpdateCapability(t *testing.T) {
	plain := notifierFunc(func(context.Context, providers.Investigation) error {
		t.Fatal("a plain (non-KB-update) notifier must not receive Deliver on a KB-update announcement")
		return nil
	})
	cap1 := &captureKBUpdate{}
	cap2 := &captureKBUpdate{fail: true} // failing sink: must be swallowed
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), plain, cap1, cap2)
	up := providers.KBUpdate{
		Transport: "slack", Root: "111.222",
		Route: providers.KBRouteOpenPR, PR: 12,
		URL:    "https://github.com/o/r/pull/12",
		Title:  "Operator note: HarborDown",
		Author: "sre-jane",
		Note:   "the registry PVC was full",
		At:     time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}
	if err := m.DeliverKBUpdate(context.Background(), up); err != nil {
		t.Fatalf("DeliverKBUpdate must swallow sink errors, got %v", err)
	}
	if len(cap1.got) != 1 || cap1.got[0] != up {
		t.Fatalf("KB-update-capable notifier got %+v, want exactly one announcement equal to %+v", cap1.got, up)
	}
	if len(cap2.got) != 1 {
		t.Fatalf("a failing KB-update sink must still be attempted, got %d", len(cap2.got))
	}
}

// TestMultiDeliverKBUpdateNilAndEmptyAreNoOps pins the nil-safe contract: the
// announcement is opt-in, so an unwired or notifier-less Multi is a silent no-op
// rather than a panic on the path that just wrote to the knowledge base.
func TestMultiDeliverKBUpdateNilAndEmptyAreNoOps(t *testing.T) {
	var nilMulti *Multi
	if err := nilMulti.DeliverKBUpdate(context.Background(), providers.KBUpdate{}); err != nil {
		t.Fatalf("nil Multi: got %v, want nil", err)
	}
	empty := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := empty.DeliverKBUpdate(context.Background(), providers.KBUpdate{}); err != nil {
		t.Fatalf("empty Multi: got %v, want nil", err)
	}
}
