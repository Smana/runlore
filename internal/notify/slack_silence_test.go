// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func silenceWindows() []time.Duration {
	return []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}
}

// findOverflow returns the silence overflow element from a rendered block set,
// plus the block_id of the actions block carrying it.
func findOverflow(t *testing.T, blocks []map[string]any) (map[string]any, string) {
	t.Helper()
	for _, b := range blocks {
		els, ok := b["elements"].([]map[string]any)
		if !ok {
			continue
		}
		for _, el := range els {
			if el["action_id"] == silenceActionID {
				id, _ := b["block_id"].(string)
				return el, id
			}
		}
	}
	return nil, ""
}

// TestSilenceOverflowCarriesTheKeyInTheBlockID pins the encoding: Slack caps an
// overflow option value at 75 chars, so the TriggerKey CANNOT live there.
func TestSilenceOverflowCarriesTheKeyInTheBlockID(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: "production/payments-api:ProgressDeadlineExceeded"}
	el, blockID := findOverflow(t, feedbackBlocks(inv, true, silenceWindows()))
	if el == nil {
		t.Fatal("no silence overflow element rendered")
	}
	if want := silenceBlockIDPrefix + inv.TriggerKey; blockID != want {
		t.Errorf("block_id = %q, want %q", blockID, want)
	}
	opts, ok := el["options"].([]map[string]any)
	if !ok || len(opts) != 3 {
		t.Fatalf("options = %v, want 3 entries", el["options"])
	}
	for _, o := range opts {
		v, _ := o["value"].(string)
		if len(v) > 75 {
			t.Errorf("option value %q is %d chars; Slack caps it at 75", v, len(v))
		}
		if _, err := time.ParseDuration(v); err != nil {
			t.Errorf("option value %q is not a duration: %v", v, err)
		}
	}
}

// TestSilenceOverflowEveryValueIsUnder75 is the one that would have caught the
// original design: a realistic long GitOps key must not reach an OVERFLOW
// OPTION value.
//
// This deliberately inspects the overflow's own options rather than grepping
// the whole marshaled block set for the key: the 👍/👎 buttons legitimately
// carry the full, untruncated key as their "value" (a button.value is capped at
// 2000 chars, not 75), so a blob-wide substring search would flag that safe,
// intentional placement as if it were the bug this test exists to catch.
func TestSilenceOverflowEveryValueIsUnder75(t *testing.T) {
	long := "kube-system/nginx-ingress-controller-cluster-wide-abcdef123456:ProgressDeadlineExceeded"
	inv := providers.Investigation{Title: "boom", TriggerKey: long}
	el, _ := findOverflow(t, feedbackBlocks(inv, true, silenceWindows()))
	if el == nil {
		t.Fatal("no silence overflow element rendered")
	}
	opts, ok := el["options"].([]map[string]any)
	if !ok {
		t.Fatalf("options = %v, not a []map[string]any", el["options"])
	}
	for _, o := range opts {
		v, _ := o["value"].(string)
		if strings.Contains(v, long) {
			t.Errorf("option value %q carries the TriggerKey; Slack would reject the whole message", v)
		}
	}
}

// TestSilenceOmittedWhenTheBlockIDWouldOverflow: a pathological resource name
// degrades ONE control, never the card. Mirrors feedbackBlocks' existing posture
// of rendering no buttons when there is nothing to attribute.
func TestSilenceOmittedWhenTheBlockIDWouldOverflow(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: strings.Repeat("x", 300)}
	blocks := feedbackBlocks(inv, true, silenceWindows())
	if el, _ := findOverflow(t, blocks); el != nil {
		t.Error("silence element rendered with an over-long block_id")
	}
	if len(blocks) == 0 {
		t.Error("the whole actions block was dropped; only the silence element should be")
	}
}

// TestSilenceAbsentWithoutWindows: no configured presets means the capability is
// off, and nothing should render.
func TestSilenceAbsentWithoutWindows(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: "k"}
	if el, _ := findOverflow(t, feedbackBlocks(inv, true, nil)); el != nil {
		t.Error("silence element rendered with no configured windows")
	}
}

// TestSilenceOnlyRendersWithoutFeedbackButtons pins a deployment
// config.Validate already allows: notify.slack.silence_button on its own,
// feedback_buttons left off. A rating and a suppression are independent
// capabilities enabled by separate flags (see feedbackBlocks' doc comment), so
// the card must show EXACTLY the 🔕 control — rendering 👍/👎 too would be two
// controls that dead-end at handleSlackInteraction's `s.feedback == nil` ack.
// Both delivery paths are checked.
func TestSilenceOnlyRendersWithoutFeedbackButtons(t *testing.T) {
	inv := providers.Investigation{Title: "t", TriggerKey: "k"}

	t.Run("webhook", func(t *testing.T) {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		s := NewSlack(srv.URL)
		s.SilenceWindows = silenceWindows() // FeedbackButtons left false
		if err := s.Deliver(context.Background(), inv); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !strings.Contains(got, silenceActionID) {
			t.Fatalf("silence-only webhook delivery must still render the 🔕 overflow, got: %s", got)
		}
		if strings.Contains(got, feedbackUpActionID) || strings.Contains(got, feedbackDownActionID) {
			t.Fatalf("silence-only webhook delivery must NOT render 👍/👎 — they would dead-end (feedback_buttons is off), got: %s", got)
		}
	})

	t.Run("bot", func(t *testing.T) {
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
		bot.SilenceWindows = silenceWindows() // FeedbackButtons left false
		if err := bot.Deliver(context.Background(), inv); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if len(bodies) == 0 || !strings.Contains(bodies[0], silenceActionID) {
			t.Fatalf("silence-only bot summary must still render the 🔕 overflow, got: %v", bodies)
		}
		if strings.Contains(bodies[0], feedbackUpActionID) || strings.Contains(bodies[0], feedbackDownActionID) {
			t.Fatalf("silence-only bot summary must NOT render 👍/👎 — they would dead-end (feedback_buttons is off), got: %v", bodies)
		}
	})
}

// TestFeedbackOnlyRendersWithoutSilenceOverflow is the mirror of
// TestSilenceOnlyRendersWithoutFeedbackButtons: feedback_buttons on its own,
// silence_button off (no SilenceWindows configured) must render 👍/👎 and NOT
// the 🔕 overflow — a control with nothing behind it, since s.silence would be
// nil on that deployment.
func TestFeedbackOnlyRendersWithoutSilenceOverflow(t *testing.T) {
	inv := providers.Investigation{Title: "t", TriggerKey: "k"}

	t.Run("webhook", func(t *testing.T) {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		s := NewSlack(srv.URL)
		s.FeedbackButtons = true // SilenceWindows left nil
		if err := s.Deliver(context.Background(), inv); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !strings.Contains(got, feedbackUpActionID) || !strings.Contains(got, feedbackDownActionID) {
			t.Fatalf("feedback-only webhook delivery must render 👍/👎, got: %s", got)
		}
		if strings.Contains(got, silenceActionID) {
			t.Fatalf("feedback-only webhook delivery must NOT render the 🔕 overflow — silence_button is off, got: %s", got)
		}
	})

	t.Run("bot", func(t *testing.T) {
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
		bot.FeedbackButtons = true // SilenceWindows left nil
		if err := bot.Deliver(context.Background(), inv); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if len(bodies) == 0 || !strings.Contains(bodies[0], feedbackUpActionID) || !strings.Contains(bodies[0], feedbackDownActionID) {
			t.Fatalf("feedback-only bot summary must render 👍/👎, got: %v", bodies)
		}
		if strings.Contains(bodies[0], silenceActionID) {
			t.Fatalf("feedback-only bot summary must NOT render the 🔕 overflow — silence_button is off, got: %v", bodies)
		}
	})
}

// TestSilenceOmittedWithoutTriggerKey pins the CRITICAL fix: a card whose
// Investigation carries only a Fingerprint (TriggerKey == "") must render
// 👍/👎 (fingerprint is a legitimate attribution for feedback, recorded for
// analytics regardless) but must NOT render the 🔕 silence overflow. A
// fingerprint is never a trigger key — alert fingerprints are hex, trigger
// keys are "alertname/ns/name" or "ns/name:Reason" — so a silence stored
// under a fingerprint (RecurrenceGate reads l.silences[req.TriggerKey], and
// decide() returns recurrenceOff outright when req.TriggerKey == "") could
// never be read back. Recording that click would ack "RunLore will NOT
// investigate this incident" while doing nothing: the exact "record the
// click, ack success, then ignore it" failure the feature must never cause.
//
// This is exactly the shape budgetKillResult/timeoutResult/refusalResult
// produce: they set Fingerprint but never call stampRequestFacts, so
// TriggerKey is "".
func TestSilenceOmittedWithoutTriggerKey(t *testing.T) {
	inv := providers.Investigation{Title: "boom", Fingerprint: "fp-9a1"}
	blocks := feedbackBlocks(inv, true, silenceWindows())

	if el, _ := findOverflow(t, blocks); el != nil {
		t.Errorf("silence overflow rendered for a card with no TriggerKey: %v", el)
	}
	var sawUp, sawDown bool
	for _, b := range blocks {
		els, ok := b["elements"].([]map[string]any)
		if !ok {
			continue
		}
		for _, el := range els {
			switch el["action_id"] {
			case feedbackUpActionID:
				sawUp = true
			case feedbackDownActionID:
				sawDown = true
			}
		}
	}
	if !sawUp || !sawDown {
		t.Errorf("expected 👍/👎 to still render off the fingerprint fallback; sawUp=%v sawDown=%v", sawUp, sawDown)
	}
}

// TestSilenceOverflowLabelsReadLikeTheDocs pins the human-facing half of the
// overflow. The label came from time.Duration.String(), so a preset the docs and
// values.yaml both write as `1h` rendered as "🔕 Silence 1h0m0s" on the card.
// The option VALUE is a machine token and deliberately keeps whatever spelling
// round-trips through time.ParseDuration.
func TestSilenceOverflowLabelsReadLikeTheDocs(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: "ns/app:CrashLoop"}
	el, _ := findOverflow(t, feedbackBlocks(inv, true, []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}))
	if el == nil {
		t.Fatal("no silence overflow element rendered")
	}
	opts, ok := el["options"].([]map[string]any)
	if !ok {
		t.Fatalf("options = %v, not a []map[string]any", el["options"])
	}
	want := []string{"🔕 Silence 1h", "🔕 Silence 4h", "🔕 Silence 24h"}
	if len(opts) != len(want) {
		t.Fatalf("rendered %d options, want %d", len(opts), len(want))
	}
	for i, o := range opts {
		txt, _ := o["text"].(map[string]any)
		got, _ := txt["text"].(string)
		if got != want[i] {
			t.Errorf("option %d label = %q, want %q", i, got, want[i])
		}
	}
}

// TestASingleSilenceWindowRendersNoOverflow is the render-site half of the
// minimum-2 rule. Slack's overflow element requires between 2 and 5 options, and a
// 1-option overflow does not merely fail to draw: chat.postMessage returns
// invalid_blocks and the WHOLE finding goes undelivered.
//
// Config validation rejects `windows: [4h]` with slack.silence_button on, so this
// is defence in depth — but the render site is the one place that knows how many
// options it is about to emit, and dropping one control is a failure mode this card
// already accepts (see the over-long block_id branch) while losing the notification
// is not. The 👍/👎 buttons must survive: they are gated independently.
func TestASingleSilenceWindowRendersNoOverflow(t *testing.T) {
	inv := providers.Investigation{Title: "boom", TriggerKey: "tooling/harbor:HelmUpgradeFailed"}

	blocks := feedbackBlocks(inv, true, []time.Duration{4 * time.Hour})
	if el, blockID := findOverflow(t, blocks); el != nil {
		t.Errorf("a single window rendered an overflow with %v — Slack rejects the entire message", el["options"])
	} else if blockID != "" {
		t.Errorf("block_id = %q with no overflow to carry a key for", blockID)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v, want the actions block with the feedback buttons still on it", blocks)
	}
	if _, ok := blocks[0]["block_id"]; ok {
		t.Errorf("actions block kept a block_id with no overflow: %v", blocks[0])
	}
	els, _ := blocks[0]["elements"].([]map[string]any)
	if len(els) != 2 {
		t.Fatalf("elements = %v, want the two feedback buttons alone", els)
	}

	// Two is enough, and is what the config's own floor allows.
	if el, _ := findOverflow(t, feedbackBlocks(inv, true, []time.Duration{time.Hour, 4 * time.Hour})); el == nil {
		t.Error("two windows rendered no overflow: the guard is off by one")
	}

	// With no feedback buttons either, a single window leaves nothing to draw.
	if got := feedbackBlocks(inv, false, []time.Duration{4 * time.Hour}); got != nil {
		t.Errorf("feedbackBlocks = %v, want nil — no buttons and no renderable overflow", got)
	}
}
