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
	el, blockID := findOverflow(t, feedbackBlocks(inv, silenceWindows()))
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
	el, _ := findOverflow(t, feedbackBlocks(inv, silenceWindows()))
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
	blocks := feedbackBlocks(inv, silenceWindows())
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
	if el, _ := findOverflow(t, feedbackBlocks(inv, nil)); el != nil {
		t.Error("silence element rendered with no configured windows")
	}
}

// TestSilenceOnlyRendersWithoutFeedbackButtons pins a deployment
// config.Validate already allows: notify.slack.silence_button on its own,
// feedback_buttons left off. A rating and a suppression are independent
// capabilities enabled by separate flags (see feedbackBlocks' doc comment), so
// gating the whole row on FeedbackButtons alone would silently drop the 🔕
// control for exactly the config that turned it on. Both delivery paths must
// still render it.
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
	})
}
