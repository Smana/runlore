// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type recordedSilence struct {
	key    string
	window time.Duration
	user   string
}

type recordSilence struct {
	got []recordedSilence
	err error
}

func (r *recordSilence) Silence(key string, window time.Duration, user string, _ time.Time) error {
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, recordedSilence{key: key, window: window, user: user})
	return nil
}

// sendSilence posts a signed menu-selection interaction. Note the payload shape:
// the 🔕 menu carries its choice in selected_option.value, NOT in value, and the
// TriggerKey rides in the action's block_id.
func sendSilence(t *testing.T, srv *Server, secret, blockID, value string) *httptest.ResponseRecorder {
	t.Helper()
	payload := `{"user":{"id":"U9","username":"bob"},"actions":[{"action_id":"runlore_silence",` +
		`"block_id":"` + blockID + `","selected_option":{"value":"` + value + `"}}]}`
	body := "payload=" + url.QueryEscape(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSlackSilenceInteraction(t *testing.T) {
	rec := &recordSilence{}
	const secret = "shh"
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)

	if rr := sendSilence(t, srv, secret, "sil:ns/app:CrashLoop", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	want := recordedSilence{key: "ns/app:CrashLoop", window: 4 * time.Hour, user: "U9"}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Fatalf("recorded = %+v, want exactly one %+v", rec.got, want)
	}
}

// TestSlackSilenceMalformedPayloadsRecordNothing: every rejected shape must be
// acked (200, so Slack does not retry) and must record nothing.
func TestSlackSilenceMalformedPayloadsRecordNothing(t *testing.T) {
	const secret = "shh"
	for _, tc := range []struct {
		name    string
		blockID string
		value   string
	}{
		{"block_id without the sil: prefix", "ns/app:CrashLoop", "4h"},
		{"empty key after the prefix", "sil:", "4h"},
		{"a value that is not a duration", "sil:k", "forever"},
		{"an empty value", "sil:k", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordSilence{}
			srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
			if rr := sendSilence(t, srv, secret, tc.blockID, tc.value); rr.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (acked, not retried)", rr.Code)
			}
			if len(rec.got) != 0 {
				t.Errorf("recorded %+v, want nothing", rec.got)
			}
		})
	}
}

// TestSlackSilenceDisabledIsAckedNotFatal: silencing off must ack the click
// rather than 404, panic, or silently succeed.
func TestSlackSilenceDisabledIsAckedNotFatal(t *testing.T) {
	const secret = "shh"
	// A feedback-only server: the endpoint is up, but s.silence is nil.
	srv := New(nil, Actions{Feedback: &recordFeedback{}, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

// TestSlackSilenceEndpointUpWithSilenceAlone: a deployment that enabled ONLY
// silencing must get a live endpoint. Guards the enablement condition in
// handleSlackInteraction, which previously required approvals or feedback.
func TestSlackSilenceEndpointUpWithSilenceAlone(t *testing.T) {
	const secret = "shh"
	srv := New(nil, Actions{Silence: &recordSilence{}, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "1h"); rr.Code == http.StatusNotFound {
		t.Fatal("endpoint 404s with silencing as the only enabled capability")
	}
}

// TestSlackSilenceRecorderErrorIsAcked: a ledger write failure must reach the
// human as a message, never as a 500 Slack will retry.
func TestSlackSilenceRecorderErrorIsAcked(t *testing.T) {
	const secret = "shh"
	rec := &recordSilence{err: errTestSilence}
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

var errTestSilence = errors.New("ledger is full of bees")

// TestSilenceReadsAStaticSelectPayload pins the reason the 2026-08-26 element
// swap needed no migration: Slack delivers the chosen option at
// actions[0].selected_option.value for a static_select exactly as it did for an
// overflow, so cards posted before the swap stay clickable after it. That claim
// was verified against Slack's payload docs during design and is the whole reason
// there is no dual-shape parsing here — if Slack ever diverges, it must fail in
// this test rather than as a silently dead control on every card still in
// scrollback.
func TestSilenceReadsAStaticSelectPayload(t *testing.T) {
	rec := &recordSilence{}
	const secret = "shh"
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)

	if rr := sendSilence(t, srv, secret, "sil:k", "24h"); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	want := recordedSilence{key: "k", window: 24 * time.Hour, user: "U9"}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Fatalf("recorded = %+v, want exactly one %+v", rec.got, want)
	}
}

// TestSilencedCardDropsTheControlAndKeepsTheFinding pins the rebuild: the 🔕 menu
// goes (it has served its purpose, and a second click on a card that already says
// it is silenced is pure confusion), 👍/👎 stay (a 👎 is the documented way to
// lift a silence early, so removing it would strand the escape hatch the ack
// promises), and the marker is appended.
func TestSilencedCardDropsTheControlAndKeepsTheFinding(t *testing.T) {
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Why:* something broke"}},
		{"type": "actions", "block_id": "sil:k", "elements": []any{
			map[string]any{"type": "button", "action_id": "runlore_feedback_up"},
			map[string]any{"type": "button", "action_id": "runlore_feedback_down"},
			map[string]any{"type": "static_select", "action_id": "runlore_silence"},
		}},
	}
	got, ok := silencedCard(blocks, "runlore_silence", "🔕 Silenced by @x until y · 48h")
	if !ok {
		t.Fatal("rebuild refused a well-formed card")
	}
	dump, _ := json.Marshal(got)
	if strings.Contains(string(dump), "runlore_silence") {
		t.Error("the silence control survived the rebuild")
	}
	for _, keep := range []string{"runlore_feedback_up", "runlore_feedback_down", "something broke"} {
		if !strings.Contains(string(dump), keep) {
			t.Errorf("the rebuild dropped %q — it must only remove the silence control", keep)
		}
	}
	if !strings.Contains(string(dump), "Silenced by @x") {
		t.Error("the marker was not appended")
	}
}

// TestSilencedCardRefusesRatherThanBlanksTheCard is the regression test for the
// one hard invariant of this feature. The rebuilt card is posted with
// replace_original: true, which OVERWRITES the Slack message — so a rebuild that
// cannot be done from what the payload actually carries must report failure and
// let the caller fall back to an ephemeral note. Blanking the card loses the
// investigation the silence is about, which is strictly worse than having no
// marker: refusing costs a marker, guessing costs the finding.
func TestSilencedCardRefusesRatherThanBlanksTheCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []map[string]any
	}{
		{"no blocks in the payload at all", nil},
		{"an empty block list", []map[string]any{}},
		{
			"a lone actions block, so removing the control leaves nothing to post",
			[]map[string]any{{"type": "actions", "elements": []any{
				map[string]any{"type": "static_select", "action_id": "runlore_silence"},
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := silencedCard(tc.blocks, "runlore_silence", "marker"); ok {
				t.Errorf("rebuild claimed success on %s: %v", tc.name, got)
			}
		})
	}
}
