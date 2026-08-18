// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestDraftKBEntryWritesTheNarrowedResource pins that the draft path actually
// uses the narrowing — the frontmatter value, not just the helper, is what the
// pull request ships.
func TestDraftKBEntryWritesTheNarrowedResource(t *testing.T) {
	inv := goodFinding()
	inv.Resource = providers.Workload{
		Kind: "Node", Namespace: "observability",
		Name: "ip-10-11-189-250.ec2.internal (cluster=shared, instance i-0fd8c3c351590a3a0)",
	}
	e := draftKBEntry(inv)
	if want := "observability/ip-10-11-189-250.ec2.internal"; e.Resource != want {
		t.Errorf("drafted resource = %q, want %q", e.Resource, want)
	}
	// Nothing is lost: the qualifying detail the model wrote still reaches the body.
	if !strings.Contains(e.Body, "cluster=shared") {
		t.Errorf("the dropped qualifier must survive in the entry body, got:\n%s", e.Body)
	}
}

// TestCurateWarnsWhenTheDraftedResourceCannotBeIndexed is the visibility half of
// the fix: a resource the draft path cannot repair must not ship silently. It is
// a WARNING, never a hard failure — an unrecallable entry still beats a lost
// investigation, so the PR is opened either way.
func TestCurateWarnsWhenTheDraftedResourceCannotBeIndexed(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeForge{}
	c := newCurator(f, fakeScored{})
	c.Log = slog.New(slog.NewTextHandler(&logs, nil))

	inv := goodFinding()
	// A model-written name with no namespace: Ref() renders the namespace alone, so
	// what ships is a bare token recall can only read as a namespace.
	inv.Resource = providers.Workload{Kind: "Pod", Namespace: "harbor-registry"}

	ref, err := c.Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil || ref.URL == "" {
		t.Fatalf("a shape warning must not lose the investigation: pr=%+v ref=%q", f.openedPR, ref.URL)
	}
	out := logs.String()
	if !strings.Contains(out, "resource") || !strings.Contains(out, "harbor-registry") {
		t.Errorf("expected a visible warning naming the resource, got logs:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("the shape check must warn, not error, got logs:\n%s", out)
	}
}

// TestCurateWarnsWhenTheDraftFailsItsOwnMergeGate wires the draft path to the
// validator the catalog's CI runs (`lore validate-kb` → kbvalidate), so a draft
// that cannot merge says so BEFORE the pull request is opened. kbvalidate's own
// doc claims a drafted entry passes "by construction"; nothing checked it, and an
// Incident with no resource is the counterexample the gate rejects.
func TestCurateWarnsWhenTheDraftFailsItsOwnMergeGate(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeForge{}
	c := newCurator(f, fakeScored{})
	c.Log = slog.New(slog.NewTextHandler(&logs, nil))

	// goodFinding carries no resource at all, and its ChangeRef keeps it an
	// Incident — for which kbvalidate requires a resource.
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil {
		t.Fatal("a merge-gate warning must not suppress the PR")
	}
	out := logs.String()
	if !strings.Contains(out, "merge gate") {
		t.Errorf("expected the draft's own merge-gate failure to be logged, got:\n%s", out)
	}
}

// TestCurateDoesNotWarnOnACleanDraft keeps the warning honest: a well-shaped
// entry must produce no draft-time complaint at all, or the signal is worthless.
func TestCurateDoesNotWarnOnACleanDraft(t *testing.T) {
	var logs bytes.Buffer
	c := newCurator(&fakeForge{}, fakeScored{})
	c.Log = slog.New(slog.NewTextHandler(&logs, nil))

	inv := goodFinding()
	inv.Resource = providers.Workload{Kind: "Deployment", Namespace: "tooling", Name: "harbor-registry"}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("a clean draft must warn about nothing, got:\n%s", out)
	}
}
