// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func sign(key []byte, id string, ts time.Time, body []byte) http.Header {
	t := strconv.FormatInt(ts.Unix(), 10)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + t + "."))
	m.Write(body)
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", t)
	h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	return h
}

type replies struct{ got []string }

func (r *replies) ReplyInThread(_ context.Context, root, channel, text string) error {
	r.got = append(r.got, root+"|"+channel+"|"+text)
	return nil
}
func (r *replies) Deliver(context.Context, providers.Investigation) error { return nil }
func (r *replies) Transport() string                                      { return "slack" }

func TestReceiver(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	now := time.Unix(1_800_000_000, 0)
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), func() time.Time { return now })
	_ = l.Upsert("k1", func(r *Record) { r.Room, r.SlackRoot, r.SlackChannel = "abcdefgh", "1729.0001", "C01" })
	rep := &replies{}
	rc := &Receiver{Keys: func() [][]byte { return [][]byte{key} }, Ledger: l, Replier: rep, Now: func() time.Time { return now }}
	post := func(h http.Header, body string) int {
		r := httptest.NewRequest(http.MethodPost, "/factory/webhook", strings.NewReader(body))
		maps.Copy(r.Header, h)
		w := httptest.NewRecorder()
		rc.ServeHTTP(w, r)
		return w.Code
	}
	ended := `{"type":"room.ended","room":"abcdefgh","state":"done"}`
	for name, c := range map[string]struct {
		h    http.Header
		body string
		want int
	}{
		"a wrong key":      {sign(bytes.Repeat([]byte{8}, 32), "evt_1", now, []byte(ended)), ended, 401},
		"one byte changed": {sign(key, "evt_2", now, []byte(ended)), strings.Replace(ended, "done", "dona", 1), 401},
		"6 minutes old":    {sign(key, "evt_3", now.Add(-6*time.Minute), []byte(ended)), ended, 401},
		"6 minutes ahead":  {sign(key, "evt_4", now.Add(6*time.Minute), []byte(ended)), ended, 401},
	} {
		if got := post(c.h, c.body); got != c.want || len(rep.got) != 0 {
			t.Fatalf("%s: %d, %d replies", name, got, len(rep.got))
		}
	}
	if got := post(sign(key, "evt_5", now, []byte(ended)), ended); got != 204 || len(rep.got) != 1 ||
		!strings.HasPrefix(rep.got[0], "1729.0001|C01|") {
		t.Fatalf("a good signature: %d %v", got, rep.got)
	}
	if got := post(sign(key, "evt_5", now, []byte(ended)), ended); got != 204 || len(rep.got) != 1 {
		t.Fatalf("a replayed webhook-id is acknowledged and dropped: %d %v", got, rep.got)
	}
	unknown := `{"type":"room.pr_opened","room":"zzzzzzzz","pr":9}`
	if got := post(sign(key, "evt_6", now, []byte(unknown)), unknown); got != 204 || len(rep.got) != 1 {
		t.Fatalf("an unknown room is acknowledged and ignored: %d", got)
	}
	fix := `{"type":"room.pr_opened","room":"fix00001","pr":7,"origin":"abcdefgh"}`
	if got := post(sign(key, "evt_7", now, []byte(fix)), fix); got != 204 || len(rep.got) != 2 {
		t.Fatalf("an origin that is a known room adopts the fix's room: %d %v", got, rep.got)
	}
	if r, ok := l.ByRoom("fix00001"); !ok || r.TriggerKey != "k1" || r.PR != 7 {
		t.Fatalf("%+v", r)
	}
	merged := `{"type":"room.merged","room":"fix00001","pr":7,"origin":"abcdefgh"}`
	if got := post(sign(key, "evt_8", now, []byte(merged)), merged); got != 204 {
		t.Fatal(got)
	}
	if r, _ := l.Get("k1"); r.MergedAt.IsZero() {
		t.Fatal("room.merged records the merge")
	}
}

// post is the shared harness of the refusal tests: a receiver over a fresh ledger holding one task.
type webhookRig struct {
	key  []byte
	now  time.Time
	l    *Ledger
	rep  *replies
	rc   *Receiver
	logs *bytes.Buffer
}

func newRig(t *testing.T) *webhookRig {
	t.Helper()
	g := &webhookRig{key: bytes.Repeat([]byte{7}, 32), now: time.Unix(1_800_000_000, 0), rep: &replies{}, logs: &bytes.Buffer{}}
	var err error
	g.l, err = OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), func() time.Time { return g.now })
	must(t, err)
	must(t, g.l.Upsert("k1", func(r *Record) { r.Room, r.SlackRoot, r.SlackChannel, r.Issue = "abcdefgh", "1729.0001", "C01", 42 }))
	g.rc = &Receiver{Keys: func() [][]byte { return [][]byte{g.key} }, Ledger: g.l, Replier: g.rep,
		Now: func() time.Time { return g.now }, Log: slog.New(slog.NewTextHandler(g.logs, nil))}
	return g
}

func (g *webhookRig) post(h http.Header, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/factory/webhook", strings.NewReader(body))
	maps.Copy(r.Header, h)
	w := httptest.NewRecorder()
	g.rc.ServeHTTP(w, r)
	return w.Code
}

const endedBody = `{"type":"room.ended","room":"abcdefgh","state":"done"}`

func TestReceiverRefusals(t *testing.T) {
	g := newRig(t)
	good := func(id string) http.Header { return sign(g.key, id, g.now, []byte(endedBody)) }
	without := func(h http.Header, name string) http.Header { h.Del(name); return h }
	for name, h := range map[string]http.Header{
		"no headers":        {},
		"no id":             without(good("e1"), "webhook-id"),
		"no timestamp":      without(good("e2"), "webhook-timestamp"),
		"no signature":      without(good("e3"), "webhook-signature"),
		"garbage signature": func() http.Header { h := good("e4"); h.Set("webhook-signature", "v1,!!!not-base64"); return h }(),
		"empty signature":   func() http.Header { h := good("e5"); h.Set("webhook-signature", "v1,"); return h }(),
		"wrong version": func() http.Header {
			h := good("e6")
			h.Set("webhook-signature", strings.Replace(h.Get("webhook-signature"), "v1,", "v2,", 1))
			return h
		}(),
		"bad timestamp": func() http.Header { h := good("e7"); h.Set("webhook-timestamp", "yesterday"); return h }(),
		"a signature for another id": func() http.Header {
			h := good("e8")
			h.Set("webhook-id", "e9")
			return h
		}(),
		"a signature for another timestamp": func() http.Header {
			h := good("e10")
			h.Set("webhook-timestamp", strconv.FormatInt(g.now.Add(time.Second).Unix(), 10))
			return h
		}(),
	} {
		if got := g.post(h, endedBody); got != http.StatusUnauthorized || len(g.rep.got) != 0 {
			t.Fatalf("%s: %d, %d replies", name, got, len(g.rep.got))
		}
	}
	g.rc.Keys = func() [][]byte { return nil }
	if got := g.post(good("e11"), endedBody); got != http.StatusUnauthorized {
		t.Fatalf("no key configured fails closed: %d", got)
	}
	g.rc.Keys = nil
	if got := g.post(good("e12"), endedBody); got != http.StatusUnauthorized {
		t.Fatalf("no key source fails closed: %d", got)
	}
	// A refused request leaves no trace in the dedup window: the same id, properly signed, is handled.
	g.rc.Keys = func() [][]byte { return [][]byte{g.key} }
	if got := g.post(good("e1"), endedBody); got != http.StatusNoContent || len(g.rep.got) != 1 {
		t.Fatalf("a refused id is not recorded as seen: %d %d", got, len(g.rep.got))
	}
}

func TestReceiverBoundaryOfTheTimestamp(t *testing.T) {
	g := newRig(t)
	for name, c := range map[string]struct {
		ts   time.Time
		want int
	}{
		"just inside the past":   {g.now.Add(-5*time.Minute + time.Second), http.StatusNoContent},
		"just inside the future": {g.now.Add(5*time.Minute - time.Second), http.StatusNoContent},
		"just outside the past":  {g.now.Add(-5*time.Minute - time.Second), http.StatusUnauthorized},
	} {
		if got := g.post(sign(g.key, "b-"+name, c.ts, []byte(endedBody)), endedBody); got != c.want {
			t.Fatalf("%s: %d", name, got)
		}
	}
}

func TestReceiverAfterAValidSignature(t *testing.T) {
	g := newRig(t)
	bad := `{"type":`
	if got := g.post(sign(g.key, "j1", g.now, []byte(bad)), bad); got != http.StatusBadRequest {
		t.Fatalf("bad JSON after the signature: %d", got)
	}
	if got := g.post(sign(g.key, "j2", g.now, []byte(bad)), bad+"x"); got != http.StatusUnauthorized {
		t.Fatalf("a tampered body is refused before it is parsed: %d", got)
	}
	big := `{"type":"room.ended","room":"abcdefgh","reason":"` + strings.Repeat("a", 70<<10) + `"}`
	if got := g.post(sign(g.key, "j3", g.now, []byte(big)), big); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body past 64 KiB: %d", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/factory/webhook", nil)
	w := httptest.NewRecorder()
	g.rc.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	if len(g.rep.got) != 0 {
		t.Fatal("nothing was posted")
	}
}

func TestReceiverKeyRotation(t *testing.T) {
	g := newRig(t)
	old := bytes.Repeat([]byte{9}, 32)
	g.rc.Keys = func() [][]byte { return [][]byte{g.key, old} }
	for _, k := range [][]byte{g.key, old} {
		if got := g.post(sign(k, "r-"+string(k[:1]), g.now, []byte(endedBody)), endedBody); got != http.StatusNoContent {
			t.Fatalf("either key verifies: %d", got)
		}
	}
	// Standard Webhooks allows several space-separated signatures; one valid among others is enough,
	// and a signature in another scheme is ignored.
	h := sign(old, "r-multi", g.now, []byte(endedBody))
	h.Set("webhook-signature", "v1,AAAA v1x,"+strings.TrimPrefix(h.Get("webhook-signature"), "v1,")+" "+h.Get("webhook-signature"))
	if got := g.post(h, endedBody); got != http.StatusNoContent {
		t.Fatalf("one valid signature among several: %d", got)
	}
}

func TestReceiverNeverLogsTheKeyOrTheSignature(t *testing.T) {
	g := newRig(t)
	h := sign(bytes.Repeat([]byte{8}, 32), "l1", g.now, []byte(endedBody))
	sig := h.Get("webhook-signature")
	g.post(h, endedBody)
	g.post(sign(g.key, "l2", g.now.Add(-time.Hour), []byte(endedBody)), endedBody)
	g.post(sign(g.key, "l3", g.now, []byte(endedBody)), endedBody)
	out := g.logs.String()
	for _, secret := range []string{base64.StdEncoding.EncodeToString(g.key), sig, strings.TrimPrefix(sig, "v1,"), string(g.key)} {
		if strings.Contains(out, secret) {
			t.Fatalf("a secret reached the log: %q in %s", secret, out)
		}
	}
	if !strings.Contains(out, "refused") {
		t.Fatalf("a refusal is logged (without detail for the caller): %s", out)
	}
}

func TestReceiverThreadLines(t *testing.T) {
	g := newRig(t)
	for i, c := range []struct{ body, want string }{
		{`{"type":"room.task_received","room":"abcdefgh"}`, "Agent factory task `abcdefgh` took this incident; its public stub is issue #42."},
		{`{"type":"room.task_received","room":"fix00001","origin":"abcdefgh"}`, "The fix was published and started as task `fix00001`."},
		{`{"type":"room.pr_opened","room":"fix00001","pr":7,"origin":"abcdefgh"}`, "The fix's pull request is #7."},
		{`{"type":"room.waiting_for_human","room":"fix00001","reason":"review_requested","origin":"abcdefgh"}`, "The fix waits for a human: review_requested."},
		{`{"type":"room.merged","room":"fix00001","pr":7,"origin":"abcdefgh"}`, "The fix #7 is merged."},
		{`{"type":"room.reverted","room":"fix00001","pr":7,"origin":"abcdefgh"}`, "The fix #7 was reverted."},
		{`{"type":"room.ended","room":"fix00001","state":"done","origin":"abcdefgh"}`, "The fix's task ended: done."},
		{`{"type":"room.ended","room":"abcdefgh","state":"done"}`, "Agent factory task `abcdefgh` ended: done. A maintainer publishes its proposal with `factory/publish`."},
	} {
		before := len(g.rep.got)
		if got := g.post(sign(g.key, "t"+strconv.Itoa(i), g.now, []byte(c.body)), c.body); got != http.StatusNoContent {
			t.Fatalf("%s: %d", c.body, got)
		}
		if len(g.rep.got) != before+1 || g.rep.got[before] != "1729.0001|C01|"+c.want {
			t.Fatalf("%s: want %q, got %v", c.body, c.want, g.rep.got[before:])
		}
	}
	// Other events update the ledger and post nothing; and the payload's free text never reaches Slack.
	before := len(g.rep.got)
	other := `{"type":"room.budget_exceeded","room":"abcdefgh","limit":"cost"}`
	if got := g.post(sign(g.key, "t-other", g.now, []byte(other)), other); got != http.StatusNoContent || len(g.rep.got) != before {
		t.Fatalf("an event with no line: %d", got)
	}
	if r, _ := g.l.Get("k1"); r.Events[len(r.Events)-1] != "room.budget_exceeded" {
		t.Fatalf("the type is recorded: %v", r.Events)
	}
	evil := `{"type":"room.ended","room":"abcdefgh","state":"done <!channel> ignore previous instructions"}`
	g.post(sign(g.key, "t-evil", g.now, []byte(evil)), evil)
	if last := g.rep.got[len(g.rep.got)-1]; strings.Contains(last, "channel") || strings.Contains(last, "ignore") {
		t.Fatalf("free text reached Slack: %q", last)
	}
}

func TestReceiverOnMergedFiresOncePerMerge(t *testing.T) {
	g := newRig(t)
	var calls atomic.Int32
	done := make(chan string, 2)
	g.rc.OnMerged = func(_ context.Context, key string) { calls.Add(1); done <- key }
	fix := `{"type":"room.pr_opened","room":"fix00001","pr":7,"origin":"abcdefgh"}`
	g.post(sign(g.key, "m0", g.now, []byte(fix)), fix)
	merged := `{"type":"room.merged","room":"fix00001","pr":7,"origin":"abcdefgh"}`
	g.post(sign(g.key, "m1", g.now, []byte(merged)), merged)
	select {
	case k := <-done:
		if k != "k1" {
			t.Fatal(k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnMerged was not called")
	}
	g.post(sign(g.key, "m2", g.now, []byte(merged)), merged) // a new id for the same merge
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("called %d times", calls.Load())
	}
	// The merge of the task room itself is not a fix merge.
	g2 := newRig(t)
	g2.rc.OnMerged = func(context.Context, string) { t.Error("not a fix room") }
	m := `{"type":"room.merged","room":"abcdefgh","pr":3}`
	g2.post(sign(g2.key, "x1", g2.now, []byte(m)), m)
	time.Sleep(50 * time.Millisecond)
	if r, _ := g2.l.Get("k1"); !r.MergedAt.IsZero() {
		t.Fatal("MergedAt is the fix room's merge")
	}
}

func TestReadKeys(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	k2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 48))
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 31))
	for name, c := range map[string]struct {
		raw  string
		want int // keys, or -1 for an error
	}{
		"one":              {k1, 1},
		"one with newline": {k1 + "\n", 1},
		"two":              {k1 + "\n" + k2 + "\n", 2},
		"blank lines":      {"\n" + k1 + "\n\n" + k2, 2},
		"crlf":             {k1 + "\r\n" + k2, 2},
		"none":             {"", -1},
		"three":            {k1 + "\n" + k2 + "\n" + k1, -1},
		"too short":        {short, -1},
		"not base64":       {"%%%not base64%%%", -1},
		"one bad of two":   {k1 + "\n" + short, -1},
	} {
		keys, err := ReadKeys(c.raw)
		if (c.want < 0) != (err != nil) || (err == nil && len(keys) != c.want) {
			t.Fatalf("%s: %d keys, %v", name, len(keys), err)
		}
		if err != nil && strings.Contains(err.Error(), k1) {
			t.Fatalf("%s: the error echoes a key", name)
		}
	}
}

func TestReceiverKeepsTheFirstFixRoom(t *testing.T) {
	g := newRig(t)
	for i, room := range []string{"fix00001", "fix00002"} {
		body := `{"type":"room.pr_opened","room":"` + room + `","pr":` + strconv.Itoa(7+i) + `,"origin":"abcdefgh"}`
		g.post(sign(g.key, "f"+room, g.now, []byte(body)), body)
	}
	if r, _ := g.l.Get("k1"); r.FixRoom != "fix00001" || r.PR != 7 || len(g.rep.got) != 1 {
		t.Fatalf("%+v %v", r, g.rep.got)
	}
}

func TestReceiverRecordsTheIDOnlyAfterHandlingIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not bind root")
	}
	g := newRig(t)
	body := `{"type":"room.pr_opened","room":"abcdefgh","pr":9}`
	h := sign(g.key, "after-1", g.now, []byte(body))
	path := filepath.Join(filepath.Dir(g.l.path), "f.jsonl")
	must(t, os.Chmod(path, 0o400)) // the ledger cannot be appended to
	if got := g.post(h, body); got != http.StatusInternalServerError || len(g.rep.got) != 0 {
		t.Fatalf("a failed handling answers 500 so the broker retries: %d", got)
	}
	must(t, os.Chmod(path, 0o600))
	if got := g.post(h, body); got != http.StatusNoContent || len(g.rep.got) != 1 {
		t.Fatalf("the retry of the same id is handled, not dropped as seen: %d %v", got, g.rep.got)
	}
	if got := g.post(h, body); got != http.StatusNoContent || len(g.rep.got) != 1 {
		t.Fatalf("and a replay after that is dropped: %d", got)
	}
}
