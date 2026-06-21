package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeForge struct {
	issues     []providers.CuratedIssue
	comments   map[int]string
	relabelled map[int][2]string // number -> {remove, add}
}

func (f *fakeForge) ListIssuesByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.issues, nil
}
func (f *fakeForge) Comment(_ context.Context, n int, body string) error {
	if f.comments == nil {
		f.comments = map[int]string{}
	}
	f.comments[n] = body
	return nil
}
func (f *fakeForge) ReplaceLabel(_ context.Context, n int, remove, add string) error {
	if f.relabelled == nil {
		f.relabelled = map[int][2]string{}
	}
	f.relabelled[n] = [2]string{remove, add}
	return nil
}

func TestReinvestigatorPollOnce(t *testing.T) {
	forge := &fakeForge{issues: []providers.CuratedIssue{{Number: 7, Title: "HarborInstallFailed"}}}
	var ranTitle string
	r := &Reinvestigator{
		Forge: forge,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Run: func(_ context.Context, req Request) (providers.Investigation, error) {
			ranTitle = req.Title
			return providers.Investigation{
				Confidence: 0.7,
				RootCauses: []providers.Hypothesis{{Summary: "stuck HelmRelease", Confidence: 0.7, Evidence: []string{"exceeded retries"}}},
			}, nil
		},
	}
	r.pollOnce(context.Background())

	if ranTitle != "HarborInstallFailed" {
		t.Fatalf("re-run used title %q, want the issue title", ranTitle)
	}
	c, ok := forge.comments[7]
	if !ok || !strings.Contains(c, "re-investigation") || !strings.Contains(c, "stuck HelmRelease") {
		t.Fatalf("expected a findings comment on issue 7, got %q", c)
	}
	if forge.relabelled[7] != [2]string{ReinvestigateLabel, "investigating"} {
		t.Fatalf("expected label %s→investigating, got %v", ReinvestigateLabel, forge.relabelled[7])
	}
}
