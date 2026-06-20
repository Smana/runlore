package curator

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeIssues records which route was taken.
type fakeIssues struct {
	issue *providers.Investigation
	pr    *providers.KBEntry
}

func (f *fakeIssues) OpenIssue(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	f.issue = &inv
	return providers.Ref{URL: "https://github.com/x/y/issues/1"}, nil
}

func (f *fakeIssues) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	f.pr = &e
	return providers.Ref{URL: "https://github.com/x/y/pull/2"}, nil
}

func sampleInv(conf float64) providers.Investigation {
	return providers.Investigation{
		Title:      "Harbor probe failures after chart bump",
		Confidence: conf,
		RootCauses: []providers.Hypothesis{{
			Summary:    "chart 1.15 added a DB migration; harbor-db CrashLoopBackOff",
			Confidence: conf, Evidence: []string{"pg_up=0"}, SuggestedAction: "flux rollback hr/harbor", Reversible: true,
		}},
		Unresolved: []string{"why the lock never released"},
	}
}

func newCurator(f providers.IssueProvider) *Curator {
	return &Curator{Issues: f, MinConfidencePR: 0.75, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestCurateConfidentOpensPR(t *testing.T) {
	f := &fakeIssues{}
	ref, err := newCurator(f).Curate(context.Background(), sampleInv(0.9))
	if err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.pr == nil || f.issue != nil {
		t.Fatalf("expected PR route, got issue=%v pr=%v", f.issue, f.pr)
	}
	if !strings.Contains(ref.URL, "/pull/") {
		t.Fatalf("unexpected ref: %s", ref.URL)
	}
	if f.pr.Title == "" || !strings.Contains(f.pr.Body, "chart 1.15") || f.pr.Type == "" {
		t.Fatalf("KBEntry not drafted well: %+v", f.pr)
	}
}

func TestCurateUncertainOpensIssue(t *testing.T) {
	f := &fakeIssues{}
	_, err := newCurator(f).Curate(context.Background(), sampleInv(0.4))
	if err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.issue == nil || f.pr != nil {
		t.Fatalf("expected issue route, got issue=%v pr=%v", f.issue, f.pr)
	}
}
