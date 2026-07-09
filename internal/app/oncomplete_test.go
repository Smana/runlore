// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// captureNotifier records the last Investigation it was asked to deliver, so a
// test can assert what the post-investigation pipeline stamped onto it.
type captureNotifier struct{ got providers.Investigation }

func (c *captureNotifier) Deliver(_ context.Context, inv providers.Investigation) error {
	c.got = inv
	return nil
}

// stubCurator stands in for *curator.Curator: it always "opens" the same KB link,
// so the test can prove the outcome open records the URL curate produced (i.e. that
// curate runs BEFORE the ledger open).
type stubCurator struct{ url string }

func (s stubCurator) Curate(context.Context, providers.Investigation) (providers.Ref, error) {
	return providers.Ref{URL: s.url}, nil
}

// openLine is the subset of a ledger open event the recurrence pointers read back.
type openLine struct {
	Event      string `json:"event"`
	TriggerKey string `json:"trigger_key"`
	CuratedURL string `json:"curated_url"`
	Verdict    string `json:"verdict"`
}

// lastOpen scans the JSONL ledger file and returns the last "open" event.
func lastOpen(t *testing.T, path string) openLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open ledger file: %v", err)
	}
	defer func() { _ = f.Close() }()
	var last openLine
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e openLine
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("decode ledger line: %v", err)
		}
		if e.Event == "open" {
			last, found = e, true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan ledger file: %v", err)
	}
	if !found {
		t.Fatal("no open event in ledger file")
	}
	return last
}

// TestOnCompleteStampsRecurrenceAndPersistsOpen pins the reordered pipeline: a
// TriggerKey seen once before must deliver Occurrences==2 with the prior KB link,
// and the freshly appended open must carry this run's TriggerKey + the KB link that
// curate produced (proving curate precedes the ledger open).
func TestOnCompleteStampsRecurrenceAndPersistsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	// Pre-seed one prior open for TriggerKey "k" with its KB link, 4h ago.
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fp0",
		TriggerKey:  "k",
		CuratedURL:  "https://kb/prev",
		At:          time.Now().Add(-4 * time.Hour),
	}); err != nil {
		t.Fatalf("seed open: %v", err)
	}

	sink := &captureNotifier{}
	notifier := notify.NewMulti(discardLog(), sink)
	cur := stubCurator{url: "https://kb/new"}

	found := providers.Investigation{
		Title:       "disk pressure",
		Fingerprint: "fp1",
		TriggerKey:  "k",
		Verdict:     providers.VerdictActionRequired,
	}
	onInvestigationComplete(context.Background(), found, ledger, nil, cur, notifier, nil, nil, nil, discardLog())

	// Delivered investigation carries the recurrence facts queried BEFORE this open.
	if sink.got.Occurrences != 2 {
		t.Errorf("delivered Occurrences = %d, want 2", sink.got.Occurrences)
	}
	if sink.got.PrevCuratedURL != "https://kb/prev" {
		t.Errorf("delivered PrevCuratedURL = %q, want %q", sink.got.PrevCuratedURL, "https://kb/prev")
	}
	if sink.got.LastOccurrence.IsZero() {
		t.Error("delivered LastOccurrence is zero, want the prior occurrence time")
	}
	if sink.got.CuratedURL != "https://kb/new" {
		t.Errorf("delivered CuratedURL = %q, want %q", sink.got.CuratedURL, "https://kb/new")
	}

	// The newly appended open durably records this run's trigger key + fresh KB link.
	last := lastOpen(t, path)
	if last.TriggerKey != "k" {
		t.Errorf("last open TriggerKey = %q, want %q", last.TriggerKey, "k")
	}
	if last.CuratedURL != "https://kb/new" {
		t.Errorf("last open CuratedURL = %q, want %q", last.CuratedURL, "https://kb/new")
	}
	if last.Verdict != string(providers.VerdictActionRequired) {
		t.Errorf("last open Verdict = %q, want %q", last.Verdict, providers.VerdictActionRequired)
	}
}

// TestOnCompleteCountsOneOccurrencePerInvestigation pins the coalesced-batch fix: an
// investigation carrying N constituent fingerprints records N per-fingerprint opens
// (for resolve-webhook pairing), but must contribute exactly ONE TriggerKey
// occurrence — not N. Otherwise a single investigation of a coalesced batch would
// inflate Occurrences by N and the recurrence line would over-count.
func TestOnCompleteCountsOneOccurrencePerInvestigation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}

	sink := &captureNotifier{}
	notifier := notify.NewMulti(discardLog(), sink)

	found := providers.Investigation{
		Title:        "coalesced batch",
		Fingerprints: []string{"f1", "f2", "f3"},
		TriggerKey:   "k",
	}
	onInvestigationComplete(context.Background(), found, ledger, nil, nil, notifier, nil, nil, nil, discardLog())

	// One investigation ⇒ exactly one TriggerKey occurrence, despite 3 opens.
	if n, _, _ := ledger.Occurrences("k"); n != 1 {
		t.Errorf("Occurrences(k) = %d, want 1 (one investigation, not per-fingerprint)", n)
	}
	if sink.got.Occurrences != 1 {
		t.Errorf("delivered Occurrences = %d, want 1 (first occurrence)", sink.got.Occurrences)
	}

	// A second investigation of the same key bumps the count to exactly 2.
	onInvestigationComplete(context.Background(), found, ledger, nil, nil, notifier, nil, nil, nil, discardLog())
	if n, _, _ := ledger.Occurrences("k"); n != 2 {
		t.Errorf("Occurrences(k) after 2nd run = %d, want 2", n)
	}
	if sink.got.Occurrences != 2 {
		t.Errorf("delivered Occurrences on 2nd run = %d, want 2", sink.got.Occurrences)
	}
}

// TestOnCompleteRecordsGitOpsIncident pins Defect 1's fix: a GitOps-failure incident
// (no external alert fingerprint, only a derived gitops:<hash> id) must record an
// outcome open and show up in Occurrences and Episodes — previously the open-emission
// guard skipped it entirely, so pure-GitOps patterns were invisible to the loop.
func TestOnCompleteRecordsGitOpsIncident(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	sink := &captureNotifier{}
	notifier := notify.NewMulti(discardLog(), sink)

	// What FromFailureEvent+the loop produce for a GitOps failure: a synthetic
	// fingerprint and a trigger key, no Alertmanager id.
	fp := outcome.DeriveFingerprint(outcome.GitOpsFingerprintPrefix, "argocd/airflow:Degraded")
	found := providers.Investigation{
		Title:        "airflow Degraded",
		Fingerprint:  fp,
		Fingerprints: []string{fp},
		TriggerKey:   "argocd/airflow:Degraded",
	}
	onInvestigationComplete(context.Background(), found, ledger, nil, nil, notifier, nil, nil, nil, discardLog())

	// The GitOps incident is now captured: an open was recorded (Occurrences ≥ 1) ...
	if n, _, _ := ledger.Occurrences("argocd/airflow:Degraded"); n != 1 {
		t.Fatalf("GitOps incident must record one occurrence, got %d", n)
	}
	// ... and appears in Episodes (so the Phase-2 Recurrence pass can see it).
	eps, err := ledger.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || eps[0].Title != "airflow Degraded" {
		t.Fatalf("GitOps open must appear as one episode, got %+v", eps)
	}
}

// TestOnCompleteGitOpsRecallNotCountedTowardDecay pins Defect 2 at the emission site:
// a RECALL open for a GitOps incident (synthetic fingerprint, no resolve channel) is
// marked non-resolvable, so it does NOT increment the entry's Recalls (which could
// otherwise never be balanced by a Resolved and would decay a correct entry forever) —
// while an equivalent Alertmanager recall (real fingerprint) does count.
func TestOnCompleteGitOpsRecallNotCountedTowardDecay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	notifier := notify.NewMulti(discardLog(), &captureNotifier{})

	gitopsFP := outcome.DeriveFingerprint(outcome.GitOpsFingerprintPrefix, "argocd/airflow:Degraded")
	gitops := providers.Investigation{
		Title:         "airflow Degraded",
		Fingerprint:   gitopsFP,
		Fingerprints:  []string{gitopsFP},
		TriggerKey:    "argocd/airflow:Degraded",
		Recalled:      true,
		RecalledEntry: "airflow.md",
	}
	onInvestigationComplete(context.Background(), gitops, ledger, nil, nil, notifier, nil, nil, nil, discardLog())

	if c, _ := ledger.OpenCounts(); c["airflow.md"].Recalls != 0 {
		t.Fatalf("a non-resolvable GitOps recall must not count toward Recalls, got %+v", c["airflow.md"])
	}

	// An Alertmanager recall of a different entry (real fingerprint) DOES count.
	alert := providers.Investigation{
		Title:         "harbor down",
		Fingerprint:   "a1b2c3d4",
		Fingerprints:  []string{"a1b2c3d4"},
		Recalled:      true,
		RecalledEntry: "harbor.md",
	}
	onInvestigationComplete(context.Background(), alert, ledger, nil, nil, notifier, nil, nil, nil, discardLog())
	if c, _ := ledger.OpenCounts(); c["harbor.md"].Recalls != 1 {
		t.Fatalf("a resolvable Alertmanager recall must count toward Recalls, got %+v", c["harbor.md"])
	}
}

// fakePrior stubs the catalog's exact-identity lookup so the completion
// pipeline can be tested without building a bleve index.
type fakePrior struct {
	e  catalog.Entry
	ok bool
}

func (f fakePrior) FindFingerprint(string) (catalog.Entry, bool) { return f.e, f.ok }

// TestOnCompleteStampsPriorKnowledge: a recurring fresh investigation whose
// merged KB entry is findable by dup-fingerprint must deliver Prior with the
// entry's Cause/Resolution excerpts and its recall track record.
func TestOnCompleteStampsPriorKnowledge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	// Prior open for the same trigger key ⇒ this run is occurrence #2.
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fp0", TriggerKey: "k", CuratedURL: "https://kb/prev",
		At: time.Now().Add(-4 * time.Hour),
	}); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	// Track record for the merged entry: one resolvable recall that resolved.
	rv := true
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fpr", Kind: "recall", Entry: "incidents/e.md",
		Resolvable: &rv, At: time.Now().Add(-3 * time.Hour),
	}); err != nil {
		t.Fatalf("seed recall open: %v", err)
	}
	if _, _, err := ledger.Resolve("fpr", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	entry := catalog.Entry{
		Path: "incidents/e.md",
		Body: "## Cause\n\n1. **bad kustomize bump** (85%)\n\n## Resolution\n\n- revert and pin 5.3.2\n",
	}
	sink := &captureNotifier{}
	notifier := notify.NewMulti(discardLog(), sink)
	found := providers.Investigation{Title: "disk pressure", Fingerprint: "fp1", TriggerKey: "k"}
	onInvestigationComplete(context.Background(), found, ledger, fakePrior{e: entry, ok: true}, nil, notifier, nil, nil, nil, discardLog())

	p := sink.got.Prior
	if p == nil {
		t.Fatal("Prior not stamped on a recurring fresh investigation")
	}
	if p.Cause != "1. bad kustomize bump (85%)" {
		t.Errorf("Prior.Cause = %q", p.Cause)
	}
	if p.Resolution != "- revert and pin 5.3.2" {
		t.Errorf("Prior.Resolution = %q", p.Resolution)
	}
	if p.EntryPath != "incidents/e.md" {
		t.Errorf("Prior.EntryPath = %q", p.EntryPath)
	}
	if p.Recalls != 1 || p.Resolved != 1 {
		t.Errorf("Prior track record = %d/%d, want 1/1", p.Resolved, p.Recalls)
	}
}

// Recalled investigations must NOT get Prior (the recalled entry IS the
// delivered answer), and a first sighting or a fingerprint miss leaves it nil.
func TestOnCompletePriorKnowledgeSkips(t *testing.T) {
	entry := catalog.Entry{Path: "incidents/e.md", Body: "## Cause\n\nc\n\n## Resolution\n\nr\n"}
	cases := []struct {
		label    string
		seed     bool // seed a prior open (⇒ Occurrences 2)
		recalled bool
		found    bool // FindFingerprint hit
	}{
		{"recall path", true, true, true},
		{"first sighting", false, false, true},
		{"no merged entry", true, false, false},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "outcomes.jsonl")
		ledger, err := outcome.New(path)
		if err != nil {
			t.Fatalf("%s: new ledger: %v", c.label, err)
		}
		if c.seed {
			if err := ledger.Open(outcome.Event{Fingerprint: "fp0", TriggerKey: "k", At: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatalf("%s: seed: %v", c.label, err)
			}
		}
		sink := &captureNotifier{}
		notifier := notify.NewMulti(discardLog(), sink)
		found := providers.Investigation{Title: "t", Fingerprint: "fp1", TriggerKey: "k", Recalled: c.recalled}
		onInvestigationComplete(context.Background(), found, ledger, fakePrior{e: entry, ok: c.found}, nil, notifier, nil, nil, nil, discardLog())
		if sink.got.Prior != nil {
			t.Errorf("%s: Prior = %+v, want nil", c.label, sink.got.Prior)
		}
	}
}
