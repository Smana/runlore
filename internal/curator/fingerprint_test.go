package curator

import (
	"context"
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
