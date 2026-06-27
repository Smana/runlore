package curator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

func TestFingerprintIncludesTitleRootAndWorkload(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		RootCauses: []providers.Hypothesis{{Summary: "IAM AccessKeysPerUser quota exceeded"}},
		Changes:    []providers.Change{{Workload: providers.Workload{Namespace: "tooling", Name: "harbor-registry"}}},
	}
	fp := Fingerprint(inv)
	for _, want := range []string{"HarborRegistryDown", "IAM AccessKeysPerUser quota", "tooling", "harbor-registry"} {
		if !strings.Contains(fp, want) {
			t.Fatalf("fingerprint %q missing %q", fp, want)
		}
	}
}

// fakeScored is a catalog.ScoredSearcher returning a fixed hit (or an error).
type fakeScored struct {
	score float64
	title string
	err   error
}

func (f fakeScored) SearchScored(_ string, _ int) ([]catalog.ScoredEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.title == "" {
		return nil, nil
	}
	return []catalog.ScoredEntry{{Entry: catalog.Entry{Title: f.title}, Score: f.score}}, nil
}

func TestNoveltyDuplicateAboveThreshold(t *testing.T) {
	inv := providers.Investigation{Title: "HarborRegistryDown"}
	n := Novelty{Catalog: fakeScored{score: 9.0, title: "HarborRegistryDown"}, DupScore: 5.0}
	dup, hit, err := n.IsDuplicate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if !dup || hit.Title != "HarborRegistryDown" {
		t.Fatalf("expected duplicate hit, got dup=%v hit=%+v", dup, hit)
	}
}

func TestNoveltyBelowThresholdIsNovel(t *testing.T) {
	inv := providers.Investigation{Title: "BrandNewThing"}
	n := Novelty{Catalog: fakeScored{score: 1.0, title: "Unrelated"}, DupScore: 5.0}
	dup, _, err := n.IsDuplicate(context.Background(), inv)
	if err != nil || dup {
		t.Fatalf("expected novel, got dup=%v err=%v", dup, err)
	}
}

func TestNoveltyNilCatalogIsNovel(t *testing.T) {
	n := Novelty{Catalog: nil, DupScore: 5.0}
	dup, _, err := n.IsDuplicate(context.Background(), providers.Investigation{Title: "x"})
	if err != nil || dup {
		t.Fatalf("nil catalog must be novel, got dup=%v err=%v", dup, err)
	}
}

func TestTopHitReturnsScore(t *testing.T) {
	// TopHit surfaces the top hit + score regardless of the DupScore threshold,
	// so the caller can both observe the score and apply the threshold itself.
	n := Novelty{Catalog: fakeScored{score: 2.0, title: "Below threshold"}, DupScore: 5.0}
	top, ok, err := n.TopHit(context.Background(), providers.Investigation{Title: "x"})
	if err != nil || !ok {
		t.Fatalf("want a hit, got ok=%v err=%v", ok, err)
	}
	if top.Score != 2.0 || top.Entry.Title != "Below threshold" {
		t.Fatalf("unexpected top hit %+v", top)
	}
}

func TestTopHitNilCatalog(t *testing.T) {
	_, ok, err := Novelty{Catalog: nil}.TopHit(context.Background(), providers.Investigation{Title: "x"})
	if ok || err != nil {
		t.Fatalf("nil catalog: want ok=false err=nil, got ok=%v err=%v", ok, err)
	}
}

func TestTopHitSearchError(t *testing.T) {
	n := Novelty{Catalog: fakeScored{err: errors.New("index unavailable")}}
	_, ok, err := n.TopHit(context.Background(), providers.Investigation{Title: "x"})
	if ok || err == nil {
		t.Fatalf("want ok=false and a non-nil error, got ok=%v err=%v", ok, err)
	}
}

func TestDupFingerprintStableAcrossTitlePhrasing(t *testing.T) {
	a := providers.Investigation{
		Title:      "Pod apps/web crash looping",
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke the readiness probe"}},
	}
	b := providers.Investigation{
		Title:      "apps/web is down after a deploy", // different prose
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "the readiness probe broke when the image tag rollout happened"}},
	}
	fa, fb := DupFingerprint(a), DupFingerprint(b)
	if fa == "" || fa != fb {
		t.Fatalf("same resource+cause must hash alike across phrasing: %q vs %q", fa, fb)
	}
}

func TestDupFingerprintDiffersByResource(t *testing.T) {
	base := providers.Investigation{
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "connection pool exhausted"}},
	}
	other := base
	other.Resource = providers.Workload{Namespace: "apps", Name: "worker"}
	if DupFingerprint(base) == DupFingerprint(other) {
		t.Fatal("different affected resource must change the fingerprint")
	}
}

func TestDupFingerprintDiffersByCause(t *testing.T) {
	base := providers.Investigation{
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "connection pool exhausted"}},
	}
	other := base
	other.RootCauses = []providers.Hypothesis{{Summary: "expired TLS certificate blocked startup"}}
	if DupFingerprint(base) == DupFingerprint(other) {
		t.Fatal("disjoint cause token-sets must change the fingerprint")
	}
}

func TestDupFingerprintEmptyWhenNoResourceOrCause(t *testing.T) {
	if got := DupFingerprint(providers.Investigation{Title: "something"}); got != "" {
		t.Fatalf("no resource and no cause must yield empty fingerprint, got %q", got)
	}
}

// TestDupFingerprintGoldenValue pins the exact canonical fingerprint to guard
// canonicalization stability. Markers in open PRs depend on this hash being
// stable across the "|" separator, token order, stopword set, and hashing
// algorithm. Any accidental change to the canonicalization must fail this test
// rather than silently invalidating already-open PR markers.
func TestDupFingerprintGoldenValue(t *testing.T) {
	inv := providers.Investigation{
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness probe"}},
	}
	const expected = "215ab128868422ea9e8f8cf247cbc79be9cd11aa0f2e8c634e8a997273ae2701"
	if got := DupFingerprint(inv); got != expected {
		t.Fatalf("golden fingerprint mismatch: expected %q, got %q", expected, got)
	}
}

// TestDupFingerprintDiffersByTerseAcronymCause guards against a false-collision:
// two different terse/acronym causes on the SAME resource whose tokens all filter
// out (sub-3-char / stopwords) must not hash to the same fingerprint (which would
// silently coalesce unrelated incidents). The raw-summary fallback prevents it.
func TestDupFingerprintDiffersByTerseAcronymCause(t *testing.T) {
	res := providers.Workload{Namespace: "apps", Name: "web"}
	a := providers.Investigation{Resource: res, RootCauses: []providers.Hypothesis{{Summary: "IO GC"}}}
	b := providers.Investigation{Resource: res, RootCauses: []providers.Hypothesis{{Summary: "db up"}}}
	fa, fb := DupFingerprint(a), DupFingerprint(b)
	if fa == "" || fb == "" {
		t.Fatalf("a terse cause on a known resource must still produce a fingerprint: %q %q", fa, fb)
	}
	if fa == fb {
		t.Fatalf("different terse causes on the same resource must not collide: both %q", fa)
	}
}

// TestDupFingerprintStableAcrossProseWhenTriggerKeyPresent is the #137 regression:
// re-investigations of one ongoing incident reword the LLM root cause, but the
// trigger that fired them (a failing resource + condition, or an alert fingerprint)
// is identical. Keying on that deterministic trigger makes the fingerprints match,
// so the curator updates the one open PR instead of opening a new PR per retry.
func TestDupFingerprintStableAcrossProseWhenTriggerKeyPresent(t *testing.T) {
	res := providers.Workload{Kind: "Application", Namespace: "argocd", Name: "airflow"}
	const tk = "argocd/airflow:Degraded"
	a := providers.Investigation{
		Resource:   res,
		TriggerKey: tk,
		RootCauses: []providers.Hypothesis{{Summary: "ArgoCD git repository authentication failure"}},
	}
	b := providers.Investigation{
		Resource:   res,
		TriggerKey: tk,
		RootCauses: []providers.Hypothesis{{Summary: "Missing ExternalSecret for database credentials: Secret does not exist"}},
	}
	fa, fb := DupFingerprint(a), DupFingerprint(b)
	if fa == "" || fa != fb {
		t.Fatalf("same trigger key must hash alike across reworded causes: %q vs %q", fa, fb)
	}
}

// TestDupFingerprintDiffersByTriggerKey guards against over-coalescing: genuinely
// different triggers on the same resource (e.g. a Degraded vs an OutOfSync
// condition) must not collapse into one fingerprint.
func TestDupFingerprintDiffersByTriggerKey(t *testing.T) {
	res := providers.Workload{Namespace: "argocd", Name: "airflow"}
	a := providers.Investigation{Resource: res, TriggerKey: "argocd/airflow:Degraded", RootCauses: []providers.Hypothesis{{Summary: "x"}}}
	b := providers.Investigation{Resource: res, TriggerKey: "argocd/airflow:OutOfSync", RootCauses: []providers.Hypothesis{{Summary: "x"}}}
	if DupFingerprint(a) == DupFingerprint(b) {
		t.Fatal("different trigger keys on the same resource must not coalesce")
	}
}
