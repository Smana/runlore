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

// TestDraftResourceShape is the draft-time gate on the one frontmatter field
// recall matches structurally. Two live drafts (#518) shipped a `resource` that
// could not work, in the two opposite ways:
//
//   - "observability/ip-10-11-189-250.ec2.internal (cluster=shared, instance
//     i-0fd8c3c351590a3a0)" — whitespace, so the catalog's CI gate rejected it
//     loudly and the PR sat unmergeable for four days.
//   - "argocd/essentials,monitoring,argocd-app-of-apps" — no whitespace, so it
//     PASSED the gate and merged, yet recall compares `resource` by string
//     equality: no incident's workload is ever that string, so the entry was
//     dead on arrival with nothing to surface it.
//
// Both must be settled here, at draft time, before the pull request exists. The
// table pins what gets WRITTEN and whether the draft path has anything to say
// about it — repaired silently when the leading namespace/name token is
// unambiguous, warned about when it is not.
func TestDraftResourceShape(t *testing.T) {
	tests := []struct {
		name         string
		ref          string
		wantResource string
		wantReason   string // substring; "" means the value is usable as-is
	}{{
		name:         "a valid namespace/name is written untouched and needs no warning",
		ref:          "tooling/harbor-registry",
		wantResource: "tooling/harbor-registry",
	}, {
		name:         "the live whitespace+parenthetical draft keeps only the identifier",
		ref:          "observability/ip-10-11-189-250.ec2.internal (cluster=shared, instance i-0fd8c3c351590a3a0)",
		wantResource: "observability/ip-10-11-189-250.ec2.internal",
	}, {
		name:         "the live comma-joined list keeps its first object, which is a real one",
		ref:          "argocd/essentials,monitoring,argocd-app-of-apps",
		wantResource: "argocd/essentials",
	}, {
		name:         "a bare name with no namespace cannot be repaired, so it is warned about",
		ref:          "harbor-registry",
		wantResource: "harbor-registry",
		wantReason:   "bare namespace",
	}, {
		name:         "empty is legitimately scopeless — no value, no warning",
		ref:          "",
		wantResource: "",
	}, {
		// recall's resourceAgrees normalizes the pod-hash suffix off BOTH sides before
		// comparing, so a hash-bearing ref still matches. It must not be warned about
		// and must not be mangled: it is a well-formed namespace/name.
		name:         "a pod-template-hash suffix stays acceptable",
		ref:          "tooling/harbor-registry-59598dbd57-ltkzw",
		wantResource: "tooling/harbor-registry-59598dbd57-ltkzw",
	}, {
		name:         "a ref with more than one slash is not a namespace/name",
		ref:          "tooling/harbor/registry",
		wantResource: "tooling/harbor/registry",
		wantReason:   "namespace/name",
	}, {
		name:         "an empty namespace half is not a namespace/name either",
		ref:          "/harbor-registry",
		wantResource: "/harbor-registry",
		wantReason:   "namespace/name",
	}, {
		// The class the shape check missed while it only counted slashes: each of the
		// four below clears the merge gate (no whitespace) and carries no character
		// EntryResourceRef cuts at, yet none can ever equal a Workload.Ref(). Before
		// the charset rule they shipped with no warning at all — #518's own worse half.
		name:         "an uppercase name can never name a Kubernetes object",
		ref:          "tooling/Harbor",
		wantResource: "tooling/Harbor",
		wantReason:   "namespace/name",
	}, {
		name:         "an underscore is not legal in a Kubernetes name",
		ref:          "tooling/harbor_registry",
		wantResource: "tooling/harbor_registry",
		wantReason:   "namespace/name",
	}, {
		name:         "a separator the cut does not know about is still reported",
		ref:          "argocd/essentials|monitoring",
		wantResource: "argocd/essentials|monitoring",
		wantReason:   "namespace/name",
	}, {
		name:         "a trailing dot is not a legal name either",
		ref:          "tooling/harbor.",
		wantResource: "tooling/harbor.",
		wantReason:   "namespace/name",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := draftResource(tt.ref)
			if got != tt.wantResource {
				t.Errorf("draftResource(%q) resource = %q, want %q", tt.ref, got, tt.wantResource)
			}
			switch {
			case tt.wantReason == "" && reason != "":
				t.Errorf("draftResource(%q) warned %q, want no warning", tt.ref, reason)
			case tt.wantReason != "" && !strings.Contains(reason, tt.wantReason):
				t.Errorf("draftResource(%q) warned %q, want it to mention %q", tt.ref, reason, tt.wantReason)
			}
			// The reason is derived from the value that ships, so re-deriving it from
			// that value must give the same answer — that is what lets the curator warn
			// from the finished entry rather than from the raw ref.
			if again, againReason := draftResource(got); again != got || againReason != reason {
				t.Errorf("draftResource is not idempotent: %q → (%q, %q) → (%q, %q)", tt.ref, got, reason, again, againReason)
			}
		})
	}
}

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
