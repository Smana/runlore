// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeSpecReader struct {
	out providers.ResourceSpec
	err error
	got providers.Workload
}

func (f *fakeSpecReader) ResourceSpec(_ context.Context, w providers.Workload) (providers.ResourceSpec, error) {
	f.got = w
	return f.out, f.err
}

func callSpec(t *testing.T, r providers.ResourceSpecReader, args string) string {
	t.Helper()
	out, err := ResourceSpecTool{Reader: r}.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return out
}

// TestResourceSpecOutcomesAreNotConflated is the whole point of the tool's rendering, and
// the reason it exists at all. "You may not read this" and "this cluster has no such kind"
// are NOT "the object is absent" — collapsing them is the defect that made
// gitops_resource_status hand back a fabricated fact and a wrong root cause.
//
// Only the ABSENT case may claim the object does not exist.
func TestResourceSpecOutcomesAreNotConflated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outcome     providers.ResourceSpecOutcome
		detail      string
		wantMarker  string
		mustDisclam bool
	}{
		{"absent", providers.ResourceAbsent, "the API server reports no such object", "ABSENT", false},
		{"forbidden", providers.ResourceForbidden, `vmservicescrapes.operator.victoriametrics.com is forbidden`, "FORBIDDEN", true},
		{"unknown kind", providers.ResourceKindUnknown, `this cluster serves no kind "Widget"`, "UNKNOWN KIND", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeSpecReader{out: providers.ResourceSpec{Outcome: tc.outcome, Detail: tc.detail}}
			out := callSpec(t, r, `{"kind":"VMServiceScrape","name":"x","namespace":"observability"}`)
			if !strings.Contains(out, tc.wantMarker) {
				t.Fatalf("outcome %s not distinguishable in output:\n%s", tc.outcome, out)
			}
			if tc.mustDisclam {
				if !strings.Contains(out, "NOTHING about whether") {
					t.Errorf("%s does not disclaim evidence of absence:\n%s", tc.outcome, out)
				}
				// The failure mode being guarded: a non-absence outcome that nonetheless
				// tells the model the object is not there.
				if strings.Contains(out, "does not exist") {
					t.Errorf("%s claims the object does not exist:\n%s", tc.outcome, out)
				}
			}
		})
	}
	// And the converse: a genuine absence MUST still say so plainly, or the tool is
	// useless for the case it is most often needed in.
	r := &fakeSpecReader{out: providers.ResourceSpec{Outcome: providers.ResourceAbsent, Detail: "gone"}}
	if out := callSpec(t, r, `{"kind":"Service","name":"x","namespace":"n"}`); !strings.Contains(out, "does not exist") {
		t.Errorf("a real absence must be stated as evidence:\n%s", out)
	}
}

// TestResourceSpecRedactsSpecAndStatus pins the only line of defence for non-Secret kinds.
// A spec can carry an inline credential — an env value, a connection string, a token in a
// free-form field — and this output crosses the provider boundary.
func TestResourceSpecRedactsSpecAndStatus(t *testing.T) {
	const tok = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	r := &fakeSpecReader{out: providers.ResourceSpec{
		Outcome:    providers.ResourceFound,
		APIVersion: "v1",
		Spec:       "containers:\n- env:\n  - name: TOKEN\n    value: " + tok,
		Status:     "message: auth failed for " + tok,
	}}
	out := callSpec(t, r, `{"kind":"Pod","name":"p","namespace":"n"}`)
	if strings.Contains(out, tok) {
		t.Fatalf("a credential in the spec/status reached the model verbatim:\n%s", out)
	}
	// A denial's detail is server-provided text and gets the same treatment.
	r2 := &fakeSpecReader{out: providers.ResourceSpec{Outcome: providers.ResourceForbidden, Detail: "denied using " + tok}}
	if out := callSpec(t, r2, `{"kind":"Pod","name":"p","namespace":"n"}`); strings.Contains(out, tok) {
		t.Fatalf("a credential in a denial message reached the model verbatim:\n%s", out)
	}
}

// TestResourceSpecPassesTheWorkloadThrough guards the plumbing: a tool that silently drops
// the namespace would read the wrong object and report confidently about it.
func TestResourceSpecPassesTheWorkloadThrough(t *testing.T) {
	r := &fakeSpecReader{out: providers.ResourceSpec{Outcome: providers.ResourceFound, APIVersion: "v1", Spec: "k: v"}}
	callSpec(t, r, `{"kind":"VMServiceScrape","name":"datagrok-rabbitmq","namespace":"observability"}`)
	want := providers.Workload{Kind: "VMServiceScrape", Name: "datagrok-rabbitmq", Namespace: "observability"}
	if r.got != want {
		t.Fatalf("reader received %+v, want %+v", r.got, want)
	}
}

// TestResourceSpecUnconfiguredIsHonest: with no cluster access the tool must say so rather
// than return an empty result that reads like "the object has no spec".
func TestResourceSpecUnconfiguredIsHonest(t *testing.T) {
	out, err := ResourceSpecTool{}.Call(context.Background(), `{"kind":"Pod","name":"p","namespace":"n"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not configured") {
		t.Fatalf("an unwired tool must say so:\n%s", out)
	}
}

// TestResourceSpecDescriptionSetsExpectations: the description is the only place the model
// learns that Secret is refused and that a denial is not absence, so those claims must be
// present. A model that believes it can read a Secret will waste a call and, worse, may
// read a refusal as absence.
func TestResourceSpecDescriptionSetsExpectations(t *testing.T) {
	d := ResourceSpecTool{}.Description()
	for _, want := range []string{"Secret is refused", "NOT evidence", "spec"} {
		if !strings.Contains(d, want) {
			t.Errorf("description missing %q:\n%s", want, d)
		}
	}
}
