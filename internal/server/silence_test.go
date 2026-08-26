// SPDX-License-Identifier: Apache-2.0

package server

import (
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
