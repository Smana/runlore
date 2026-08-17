// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeSpecReader struct {
	out providers.ResourceSpec
	err error
	got providers.ResourceSpecQuery
}

func (f *fakeSpecReader) ResourceSpec(_ context.Context, q providers.ResourceSpecQuery) (providers.ResourceSpec, error) {
	f.got = q
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

// TestResourceSpecRedactsSpecAndStatus pins the LAST pass over spec/status text on its way
// to the provider: a credential in a free-form field, a connection string, a token quoted
// in a status message.
//
// It is deliberately NOT the guard for a container's env. That shape — the sensitive word
// in the VALUE of `name:`, the credential under the literal key `value:` — is invisible to
// a key-name-oriented string ruleset, and this test used to "cover" it only because the
// fixture's value happened to carry a ghp_ prefix that a token rule matched on its own.
// The env shape is masked STRUCTURALLY, before anything is marshalled, and is pinned by
// cluster.TestResourceSpecMasksContainerEnvValues.
func TestResourceSpecRedactsSpecAndStatus(t *testing.T) {
	const tok = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	r := &fakeSpecReader{out: providers.ResourceSpec{
		Outcome:    providers.ResourceFound,
		APIVersion: "v1",
		Spec:       "database:\n  connection_string: postgres://app:hunter2supersecret@db:5432/x\n  token: " + tok,
		Status:     "message: auth failed for " + tok,
	}}
	out := callSpec(t, r, `{"kind":"Pod","name":"p","namespace":"n"}`)
	for _, gone := range []string{tok, "hunter2supersecret"} {
		if strings.Contains(out, gone) {
			t.Fatalf("a credential in the spec/status reached the model verbatim:\n%s", out)
		}
	}
	// A denial's detail is server-provided text and gets the same treatment.
	r2 := &fakeSpecReader{out: providers.ResourceSpec{Outcome: providers.ResourceForbidden, Detail: "denied using " + tok}}
	if out := callSpec(t, r2, `{"kind":"Pod","name":"p","namespace":"n"}`); strings.Contains(out, tok) {
		t.Fatalf("a credential in a denial message reached the model verbatim:\n%s", out)
	}
}

// TestResourceSpecPassesTheQueryThrough guards the plumbing: a tool that silently drops the
// namespace would read the wrong object and report confidently about it. The optional group
// argument travels the same path — without it, an ambiguous kind stays a dead end.
//
// It also covers the fallback identity: a reader that echoes no resolved query (as this
// fake does) must still produce the object's id from the request rather than a blank.
func TestResourceSpecPassesTheQueryThrough(t *testing.T) {
	r := &fakeSpecReader{out: providers.ResourceSpec{Outcome: providers.ResourceFound, APIVersion: "v1", Spec: "k: v"}}
	out := callSpec(t, r, `{"kind":"NetworkPolicy","name":"deny-all","namespace":"apps","group":"networking.k8s.io"}`)
	want := providers.ResourceSpecQuery{Kind: "NetworkPolicy", Name: "deny-all", Namespace: "apps", Group: "networking.k8s.io"}
	if r.got != want {
		t.Fatalf("reader received %+v, want %+v", r.got, want)
	}
	if !strings.Contains(out, "NetworkPolicy apps/deny-all") {
		t.Fatalf("the object is not identified in the output:\n%s", out)
	}
}

// TestResourceSpecIdentifiesWhatWasREAD, not what was asked for. A cluster-scoped kind
// called with a namespace was read anyway and the invented namespace rendered back as
// fact — "StorageClass totally-made-up-ns/fast" — which is a fabricated detail in exactly
// the register the model treats as evidence.
func TestResourceSpecIdentifiesWhatWasRead(t *testing.T) {
	r := &fakeSpecReader{out: providers.ResourceSpec{
		Outcome:    providers.ResourceFound,
		APIVersion: "storage.k8s.io/v1",
		Query:      providers.ResourceSpecQuery{Kind: "StorageClass", Name: "fast", Group: "storage.k8s.io"},
		Spec:       "provisioner: ebs.csi.aws.com",
	}}
	out := callSpec(t, r, `{"kind":"storageclass","name":"fast","namespace":"totally-made-up-ns"}`)
	if strings.Contains(out, "totally-made-up-ns") {
		t.Fatalf("a namespace the object does not have was rendered as fact:\n%s", out)
	}
	if !strings.Contains(out, "StorageClass fast") {
		t.Fatalf("the resolved identity is not rendered:\n%s", out)
	}
}

// TestResourceSpecRefusalIsNotADenial: the Secret refusal used to render as FORBIDDEN,
// which reads to a human as an RBAC gap. The obvious fix for an RBAC gap is to widen the
// ClusterRole — and the widest fix, resources: ["*"], grants `secrets`. A policy refusal
// that pushes an operator toward granting the very thing it refuses is a bad ending, so it
// says what it is.
func TestResourceSpecRefusalIsNotADenial(t *testing.T) {
	r := &fakeSpecReader{out: providers.ResourceSpec{
		Outcome: providers.ResourceRefused,
		Detail:  "Secret objects are never readable through this tool",
	}}
	out := callSpec(t, r, `{"kind":"Secret","name":"db","namespace":"apps"}`)
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("a refusal must be distinguishable from a denial:\n%s", out)
	}
	if strings.Contains(out, "FORBIDDEN") {
		t.Fatalf("a policy refusal still renders as an RBAC denial:\n%s", out)
	}
	if !strings.Contains(out, "NOT an RBAC denial") {
		t.Fatalf("the refusal does not steer the reader away from widening RBAC:\n%s", out)
	}
	if strings.Contains(out, "does not exist") {
		t.Fatalf("a refusal claims the object does not exist:\n%s", out)
	}
}

// TestResourceSpecAmbiguityIsActionable: an ambiguous kind is its own ending, and the
// output must carry the way out of it — the group argument — or the model's only options
// are to give up or to re-ask the identical question.
func TestResourceSpecAmbiguityIsActionable(t *testing.T) {
	r := &fakeSpecReader{out: providers.ResourceSpec{
		Outcome: providers.ResourceKindAmbiguous,
		Detail: `kind "NetworkPolicy" is served by more than one API group ` +
			`(networking.k8s.io/v1, crd.projectcalico.org/v1); nothing was read. ` +
			`Call again with the group argument set to the one you mean`,
	}}
	out := callSpec(t, r, `{"kind":"NetworkPolicy","name":"deny-all","namespace":"apps"}`)
	if !strings.Contains(out, "AMBIGUOUS KIND") {
		t.Fatalf("ambiguity is not distinguishable from an unknown kind:\n%s", out)
	}
	for _, want := range []string{"group", "crd.projectcalico.org/v1", "NOTHING about whether"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "does not exist") {
		t.Fatalf("an ambiguous kind claims the object does not exist:\n%s", out)
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

// TestResourceSpecSchemaDeclaresGroup: the group argument is the only escape from an
// ambiguous kind, so the model has to be able to see it — and the schema is hand-written
// JSON with an embedded quoted example, which is exactly the kind of string that breaks
// silently and leaves the tool uncallable.
func TestResourceSpecSchemaDeclaresGroup(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal([]byte(ResourceSpecTool{}.Schema()), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	for _, want := range []string{"kind", "name", "namespace", "group"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("schema declares no %q argument", want)
		}
	}
	// group must stay OPTIONAL: it is only needed for the ambiguous minority, and
	// requiring it would force the model to guess a group for every ordinary read.
	if slices.Contains(schema.Required, "group") {
		t.Error("group must be optional")
	}
}
