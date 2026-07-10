// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// fakeForge records OpenPR / Comment calls and serves a fixed open-PR list.
type fakeForge struct {
	openedPR  *providers.KBEntry
	commented []int
	openPRs   []providers.CuratedIssue
}

func (f *fakeForge) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	f.openedPR = &e
	return providers.Ref{URL: "https://github.com/x/y/pull/2"}, nil
}
func (f *fakeForge) ListPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	return f.openPRs, nil
}
func (f *fakeForge) Comment(_ context.Context, n int, _ string) error {
	f.commented = append(f.commented, n)
	return nil
}

func newCurator(f *fakeForge, cat catalog.ScoredSearcher) *Curator {
	return &Curator{
		Forge: f, Catalog: cat, DupScore: 5.0, MinConfidence: 0.75,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func goodFinding() providers.Investigation {
	return providers.Investigation{
		Title: "HarborRegistryDown", Confidence: 0.9, Verified: true,
		RootCauses: []providers.Hypothesis{{
			Summary: "IAM quota exceeded", Confidence: 0.9,
			Evidence: []string{"CreateContainerConfigError"}, ChangeRef: "xplane-harbor", SuggestedAction: "delete a key",
		}},
	}
}

func TestCurateNovelHighQualityOpensPR(t *testing.T) {
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil || ref.URL == "" {
		t.Fatalf("expected a PR, got %+v / %s", f.openedPR, ref.URL)
	}
}

func TestCurateSkippedVerdictDraftsNothing(t *testing.T) {
	// A no_action finding is otherwise novel + high-quality, but the operator has
	// configured no_action to stay out of the KB review queue: no PR, no coalesce
	// comment, no repo artifact at all.
	inv := goodFinding()
	inv.Verdict = providers.VerdictNoAction
	c := newCurator(&fakeForge{}, fakeScored{})
	c.SkipVerdicts = map[providers.Verdict]bool{providers.VerdictNoAction: true}
	ref, err := c.Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if ref.URL != "" || c.Forge.(*fakeForge).openedPR != nil {
		t.Fatalf("a skipped verdict must draft nothing, got ref=%q pr=%+v", ref.URL, c.Forge.(*fakeForge).openedPR)
	}
}

func TestCurateNonSkippedVerdictUnaffected(t *testing.T) {
	// The same skip config, but an action_required finding is NOT in the skip set,
	// so it drafts a PR as usual.
	inv := goodFinding()
	inv.Verdict = providers.VerdictActionRequired
	c := newCurator(&fakeForge{}, fakeScored{})
	c.SkipVerdicts = map[providers.Verdict]bool{providers.VerdictNoAction: true}
	ref, err := c.Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if ref.URL == "" || c.Forge.(*fakeForge).openedPR == nil {
		t.Fatalf("a non-skipped verdict must draft a PR, got ref=%q", ref.URL)
	}
}

func TestCurateNoSkipConfigDraftsEveryVerdict(t *testing.T) {
	// With no SkipVerdicts configured (nil map), even a no_action verdict drafts a
	// PR — the backward-compatible default preserves pre-gate behaviour.
	inv := goodFinding()
	inv.Verdict = providers.VerdictNoAction
	c := newCurator(&fakeForge{}, fakeScored{}) // SkipVerdicts left nil
	ref, err := c.Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if ref.URL == "" || c.Forge.(*fakeForge).openedPR == nil {
		t.Fatalf("empty skip config must draft every verdict, got ref=%q", ref.URL)
	}
}

func TestCurateDuplicateCoalescesNoPR(t *testing.T) {
	// An open PR already covers this incident (matching fingerprint marker in body),
	// and the catalog does NOT (fakeScored{} returns no hit) — so we fall through to
	// the open-PR coalesce path, which comments on the existing PR rather than filing
	// a new one.
	inv := goodFinding()
	openPR := providers.CuratedIssue{
		Number: 48,
		Title:  "KB: HarborRegistryDown",
		Body:   "Drafted by RunLore\n\n" + providers.FingerprintMarker(DupFingerprint(inv)),
	}
	f := &fakeForge{openPRs: []providers.CuratedIssue{openPR}}
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil {
		t.Fatalf("duplicate must NOT open a PR, got %+v", f.openedPR)
	}
	if len(f.commented) == 0 || f.commented[0] != 48 {
		t.Fatalf("duplicate should coalesce by commenting on PR #48, got %v", f.commented)
	}
	if ref.URL != "" {
		t.Fatalf("duplicate ref should be empty, got %s", ref.URL)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCurateDistinctTitleSameFingerprintCoalesces(t *testing.T) {
	inv := providers.Investigation{
		Title:      "freshly reworded title the LLM produced this time",
		Confidence: 0.9,
		Verified:   true,
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness", Evidence: []string{"e"}, ChangeRef: "a-change"}},
	}
	openPR := providers.CuratedIssue{
		Number: 7,
		Title:  "KB: a completely different earlier title",
		Body:   "Drafted by RunLore\n\n" + providers.FingerprintMarker(DupFingerprint(inv)),
	}
	f := &fakeForge{openPRs: []providers.CuratedIssue{openPR}}
	c := &Curator{Forge: f, MinConfidence: 0.5, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR != nil {
		t.Fatal("a same-fingerprint finding must coalesce, not open a second PR")
	}
	if len(f.commented) != 1 || f.commented[0] != 7 {
		t.Fatalf("expected a coalesce comment on PR 7, got %+v", f.commented)
	}
}

func TestCurateDifferentFingerprintOpensSecondPR(t *testing.T) {
	inv := providers.Investigation{
		Title:      "apps/web readiness failure",
		Confidence: 0.9,
		Verified:   true,
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness", Evidence: []string{"e"}, ChangeRef: "a-change"}},
	}
	openPR := providers.CuratedIssue{
		Number: 7, Title: "KB: unrelated",
		Body: "Drafted by RunLore\n\n" + providers.FingerprintMarker("0000ffff_a_different_hash"),
	}
	f := &fakeForge{openPRs: []providers.CuratedIssue{openPR}}
	c := &Curator{Forge: f, MinConfidence: 0.5, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR == nil {
		t.Fatal("a different-fingerprint finding must open its own PR")
	}
}

func TestCurateCatalogDuplicateDropsSilently(t *testing.T) {
	// The catalog already has this knowledge → drop without filing OR commenting.
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{score: 9, title: "HarborRegistryDown"}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || len(f.commented) != 0 || ref.URL != "" {
		t.Fatalf("catalog duplicate must drop silently, got pr=%+v comment=%v ref=%s", f.openedPR, f.commented, ref.URL)
	}
}

// fakeFingerprinted is a catalog fake with NO BM25 hits but an exact fingerprint
// match — proving the deterministic dedup path is independent of the BM25
// threshold.
type fakeFingerprinted struct {
	fakeScored
	fp string
}

func (f fakeFingerprinted) FindFingerprint(fp string) (catalog.Entry, bool) {
	if fp != "" && fp == f.fp {
		return catalog.Entry{Title: "already merged", Fingerprint: fp}, true
	}
	return catalog.Entry{}, false
}

func TestCurateCatalogFingerprintMatchDropsSilently(t *testing.T) {
	// The same incident was already merged into the catalog (its persisted
	// fingerprint matches) even though BM25 returns nothing → drop without filing
	// or commenting, exactly like the score-threshold duplicate path.
	inv := goodFinding()
	inv.Resource = providers.Workload{Namespace: "apps", Name: "web"}
	f := &fakeForge{}
	cat := fakeFingerprinted{fp: DupFingerprint(inv)}
	ref, err := newCurator(f, cat).Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || len(f.commented) != 0 || ref.URL != "" {
		t.Fatalf("catalog fingerprint duplicate must drop silently, got pr=%+v comment=%v ref=%s", f.openedPR, f.commented, ref.URL)
	}
}

func TestCurateCatalogErrorFallsThroughToPR(t *testing.T) {
	// A catalog search error must NOT block curation: it logs a warning and falls
	// through (fail-open) to the open-PR dedup + quality gate. With no matching open
	// PR and a good finding, that yields a PR.
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{err: errors.New("index unavailable")}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil || ref.URL == "" {
		t.Fatalf("catalog error should fall through to a PR, got %+v / %s", f.openedPR, ref.URL)
	}
}

func TestCurateLowQualityDropsNoArtifact(t *testing.T) {
	f := &fakeForge{}
	weak := providers.Investigation{Title: "vague", Confidence: 0.3} // below bar: no root cause, low conf
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), weak)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || len(f.commented) != 0 || ref.URL != "" {
		t.Fatalf("low-quality finding must produce no repo artifact, got pr=%+v comment=%v ref=%s", f.openedPR, f.commented, ref.URL)
	}
}

func TestCurateUnverifiedDropsNoArtifact(t *testing.T) {
	inv := goodFinding()
	inv.Verified = false // identical to the happy-path finding, but verify did not confirm it
	// An open PR carrying this finding's exact fingerprint marker: an unverified
	// finding must NOT coalesce a comment onto it — a comment is a repo artifact too.
	f := &fakeForge{openPRs: []providers.CuratedIssue{{
		Number: 7,
		Title:  "KB: prior",
		Body:   "Drafted by RunLore\n\n" + providers.FingerprintMarker(DupFingerprint(inv)),
	}}}
	c := &Curator{Forge: f, MinConfidence: 0.75, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR != nil {
		t.Fatal("an unverified finding must not draft a KB PR")
	}
	if len(f.commented) != 0 {
		t.Fatalf("an unverified finding must not coalesce a comment onto an open PR, got %v", f.commented)
	}
}

func TestCurateSymptomOnlyDropsNoArtifact(t *testing.T) {
	inv := goodFinding()
	inv.Verified = true
	inv.RootCauses[0].ChangeRef = ""       // no causing-change anchor
	inv.RootCauses[0].SuggestedAction = "" // no fixing-action anchor
	f := &fakeForge{}
	c := &Curator{Forge: f, MinConfidence: 0.75, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR != nil {
		t.Fatal("a symptom-only finding (no provenance) must not draft a KB PR")
	}
}

func TestCurateVerifiedWithSuggestedActionOnlyOpensPR(t *testing.T) {
	inv := goodFinding()
	inv.Verified = true
	inv.RootCauses[0].ChangeRef = ""               // no GitOps change...
	inv.RootCauses[0].SuggestedAction = "scale up" // ...but a fixing action anchors it
	f := &fakeForge{}
	c := &Curator{Forge: f, MinConfidence: 0.75, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR == nil {
		t.Fatal("a verified finding with a fixing action must curate (provenance is OR)")
	}
}

func TestCurateSkipsRecalled(t *testing.T) {
	f := &fakeForge{}
	inv := goodFinding() // high quality: would normally open a PR
	inv.Recalled = true  // but it's a cache hit, not novel
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || ref.URL != "" {
		t.Fatalf("a recalled finding must not be curated, got pr=%+v ref=%s", f.openedPR, ref.URL)
	}
}

func TestCurateDedupScoreNilMetricsSafe(t *testing.T) {
	// newCurator leaves Metrics nil; recording the dedup score must not panic.
	c := newCurator(&fakeForge{}, fakeScored{score: 2.0, title: "Some entry"})
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}
}

// multiScored returns a fixed multi-hit result for any query — the shape the
// related-knowledge section consumes.
type multiScored struct{ hits []catalog.ScoredEntry }

func (m multiScored) SearchScored(string, int) ([]catalog.ScoredEntry, error) { return m.hits, nil }

func TestCurateAttachesRelatedKnowledge(t *testing.T) {
	f := &fakeForge{}
	cat := multiScored{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Path: "incidents/a.md", Title: "A", Resource: "apps/web"}, Score: 2.5},
		{Entry: catalog.Entry{Path: "incidents/b.md", Title: "B"}, Score: 0.9},
		{Entry: catalog.Entry{Path: "incidents/noise.md", Title: "noise"}, Score: 0.05}, // below the floor
	}}
	inv := goodFinding()
	inv.Occurrences = 3
	inv.PrevCuratedURL = "https://kb/pr/12"
	if _, err := newCurator(f, cat).Curate(context.Background(), inv); err != nil {
		t.Fatalf("curate: %v", err)
	}
	if f.openedPR == nil {
		t.Fatal("no PR opened")
	}
	e := *f.openedPR
	if len(e.Related) != 2 {
		t.Fatalf("Related = %+v, want the 2 hits above the noise floor", e.Related)
	}
	if e.Related[0].Path != "incidents/a.md" || e.Related[0].Score != 2.5 || e.Related[0].Resource != "apps/web" {
		t.Errorf("Related[0] = %+v", e.Related[0])
	}
	if e.Occurrences != 3 || e.PrevCuratedURL != "https://kb/pr/12" {
		t.Errorf("recurrence facts not stamped: occ=%d prev=%q", e.Occurrences, e.PrevCuratedURL)
	}
}

// The dup decision is unchanged: a top hit at/above DupScore still skips the PR
// (no Related work happens for a duplicate).
func TestCurateDupStillSkipsWithMultiHits(t *testing.T) {
	f := &fakeForge{}
	cat := multiScored{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Path: "incidents/dup.md", Title: "dup"}, Score: 9.0}, // ≥ DupScore 5.0
	}}
	ref, err := newCurator(f, cat).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if ref.URL != "" || f.openedPR != nil {
		t.Fatal("duplicate finding must not open a PR")
	}
}

func TestCurateRecordsDedupScore(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	c := newCurator(&fakeForge{}, fakeScored{score: 2.0, title: "Some entry"}) // below DupScore → records, then continues
	c.Metrics = telemetry.NewMetrics()
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "runlore_curation_dedup_score") {
		t.Fatalf("runlore_curation_dedup_score not found in metrics output:\n%s", rec.Body.String())
	}
}
