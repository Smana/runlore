package app

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	onInvestigationComplete(context.Background(), found, ledger, cur, notifier, nil, nil, nil, discardLog())

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
	onInvestigationComplete(context.Background(), found, ledger, nil, notifier, nil, nil, nil, discardLog())

	// One investigation ⇒ exactly one TriggerKey occurrence, despite 3 opens.
	if n, _, _ := ledger.Occurrences("k"); n != 1 {
		t.Errorf("Occurrences(k) = %d, want 1 (one investigation, not per-fingerprint)", n)
	}
	if sink.got.Occurrences != 1 {
		t.Errorf("delivered Occurrences = %d, want 1 (first occurrence)", sink.got.Occurrences)
	}

	// A second investigation of the same key bumps the count to exactly 2.
	onInvestigationComplete(context.Background(), found, ledger, nil, notifier, nil, nil, nil, discardLog())
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
	onInvestigationComplete(context.Background(), found, ledger, nil, notifier, nil, nil, nil, discardLog())

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
