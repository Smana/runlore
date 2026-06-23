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
