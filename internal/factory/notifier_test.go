// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func testInvestigation() providers.Investigation {
	return providers.Investigation{
		TriggerKey: "k1", AlertName: "KubePodCrashLooping",
		Resource: providers.Workload{Namespace: "apps", Kind: "Deployment", Name: "web"},
		Verdict:  "action_required", Confidence: 0.9, Severity: "critical", Cluster: "gcp-0",
		Title: "web crash loops",
	}
}

func newTestNotifier(t *testing.T, script []int) (*Notifier, *fakeFactory, *Ledger, *bytes.Buffer) {
	t.Helper()
	clock := time.Unix(1_800_000_000, 0)
	f := &fakeFactory{ttl: time.Hour, script: script}
	c := newTestClient(t, f, &clock)
	l, err := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), func() time.Time { return clock })
	must(t, err)
	var logs bytes.Buffer
	return &Notifier{Client: c, Ledger: l, Repository: "Smana/cloud-native-ref", MaxCostUSD: 2,
		Log: slog.New(slog.NewTextHandler(&logs, nil))}, f, l, &logs
}

func TestDeliverFilesTheTaskAndRecordsIt(t *testing.T) {
	n, f, l, _ := newTestNotifier(t, []int{201})
	if err := n.Deliver(context.Background(), testInvestigation()); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 1 || f.keys[0] != "k1" {
		t.Fatalf("keys %v bodies %v", f.keys, f.bodies)
	}
	b := f.bodies[0]
	if b.Incident.Resource != "apps/web" || b.DataClass != "internal" || b.Repository != "Smana/cloud-native-ref" ||
		b.MaxCostUSD != 2 || b.Incident.Alert != "KubePodCrashLooping" || b.Incident.Severity != "critical" ||
		b.Incident.Cluster != "gcp-0" || b.Incident.Verdict != "action_required" || b.Incident.Confidence != 0.9 || b.Text == "" {
		t.Fatalf("%+v", b)
	}
	r, ok := l.Get("k1")
	if !ok || r.Task != "abcdefgh" || r.Room != "abcdefgh" || r.Issue != 42 || r.Alert != "KubePodCrashLooping" ||
		r.Namespace != "apps" || r.Workload != "Deployment/web" {
		t.Fatalf("the ledger holds task, room, issue: %+v", r)
	}
}

func TestDeliverDuplicateIsRecordedToo(t *testing.T) {
	n, _, l, _ := newTestNotifier(t, []int{200})
	must(t, n.Deliver(context.Background(), testInvestigation()))
	if r, _ := l.Get("k1"); r.Room != "abcdefgh" {
		t.Fatalf("%+v", r)
	}
}

func TestDeliverRefusalsWriteNothing(t *testing.T) {
	n, _, l, _ := newTestNotifier(t, []int{202})
	if err := n.Deliver(context.Background(), testInvestigation()); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Get("k1"); ok {
		t.Fatal("a 202 writes nothing")
	}
	n, _, l, logs := newTestNotifier(t, []int{429})
	if err := n.Deliver(context.Background(), testInvestigation()); err != nil {
		t.Fatalf("a 429 is logged, not an error: %v", err)
	}
	if _, ok := l.Get("k1"); ok || !strings.Contains(logs.String(), "principal_daily_tasks") || !strings.Contains(logs.String(), "level=INFO") {
		t.Fatalf("429: ledger written or reason not logged: %s", logs.String())
	}
}

func TestDeliverThree503sIsAnError(t *testing.T) {
	n, _, _, _ := newTestNotifier(t, []int{503, 503, 503})
	if err := n.Deliver(context.Background(), testInvestigation()); err == nil {
		t.Fatal("three 503s return an error")
	}
}

func TestDeliverWithoutATriggerKeyPostsNothing(t *testing.T) {
	n, f, _, _ := newTestNotifier(t, []int{201})
	inv := testInvestigation()
	inv.TriggerKey = ""
	if err := n.Deliver(context.Background(), inv); err != nil || len(f.keys) != 0 {
		t.Fatalf("%v %v", err, f.keys)
	}
}

func TestDeliverANewGenerationDoesNotInheritTheOldState(t *testing.T) {
	n, f, l, _ := newTestNotifier(t, []int{201, 201})
	ctx := context.Background()
	must(t, n.Deliver(ctx, testInvestigation()))
	l.Register("slack", "1729.0001", "C01", providers.Investigation{TriggerKey: "k1"})
	must(t, l.Upsert("k1", func(r *Record) {
		r.FixRoom, r.PR, r.MergedAt, r.SoakUntil = "fix00001", 7, time.Unix(1_800_000_000, 0), time.Unix(1_800_001_800, 0)
		r.Verdict, r.Outcome, r.FiringAtMerge, r.Events = "resolved", "done", true, []string{"room.merged"}
	}))
	f.script = []int{201} // the incident re-fires: the factory answers 201 with a new room
	f.newRoom = "bcdefghi"
	must(t, n.Deliver(ctx, testInvestigation()))
	r, _ := l.Get("k1")
	if r.Room != "bcdefghi" || r.FixRoom != "" || r.PR != 0 || !r.MergedAt.IsZero() || !r.SoakUntil.IsZero() ||
		r.Verdict != "" || r.Outcome != "" || r.FiringAtMerge || len(r.Events) != 0 {
		t.Fatalf("the new generation starts clean: %+v", r)
	}
	if r.SlackRoot != "1729.0001" {
		t.Fatalf("the Slack root belongs to the delivery, not the generation: %+v", r)
	}
	if _, ok := l.ByRoom("fix00001"); ok {
		t.Fatal("the old fix room no longer resolves to the incident")
	}
}

func TestDeliverADuplicateKeepsTheState(t *testing.T) {
	n, _, l, _ := newTestNotifier(t, []int{201, 200})
	ctx := context.Background()
	must(t, n.Deliver(ctx, testInvestigation()))
	must(t, l.Upsert("k1", func(r *Record) { r.FixRoom, r.PR = "fix00001", 7 }))
	must(t, n.Deliver(ctx, testInvestigation()))
	if r, _ := l.Get("k1"); r.FixRoom != "fix00001" || r.PR != 7 {
		t.Fatalf("the same room keeps its state: %+v", r)
	}
}

func TestDeliverHasItsOwnDeadline(t *testing.T) {
	n, _, _, _ := newTestNotifier(t, []int{201})
	var got time.Duration
	n.Client.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if d, ok := r.Context().Deadline(); ok {
			got = time.Until(d)
		}
		return nil, errors.New("stop")
	})
	_ = n.Deliver(context.Background(), testInvestigation())
	if got <= 0 || got > deliverTimeout {
		t.Fatalf("Deliver bounds the investigation path: deadline in %s", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeliverOtherRefusalsLogAtWarn(t *testing.T) {
	n, _, l, logs := newTestNotifier(t, []int{400})
	if err := n.Deliver(context.Background(), testInvestigation()); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Get("k1"); ok || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("a refusal that is not 202 or 429 is a warning: %s", logs.String())
	}
}
