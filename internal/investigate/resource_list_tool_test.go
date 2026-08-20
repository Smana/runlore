// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeLister struct {
	out providers.ResourceList
	err error
	got providers.ResourceListQuery
}

func (f *fakeLister) ResourceList(_ context.Context, q providers.ResourceListQuery) (providers.ResourceList, error) {
	f.got = q
	return f.out, f.err
}

func callList(t *testing.T, r providers.ResourceLister, args string) string {
	t.Helper()
	out, err := ResourceListTool{Lister: r}.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return out
}

// TestEmptyListingIsEvidenceButDenialIsNot is the reason this tool exists alongside
// resource_spec, and the distinction it must never blur.
//
// A successful listing that returns nothing IS evidence that no such object is there —
// that is precisely the answer resource_spec cannot give, because it can only ask about
// names somebody already guessed. A FORBIDDEN or UNKNOWN KIND listing is NOT evidence of
// anything about the objects, and must say so, exactly as resource_spec does.
func TestEmptyListingIsEvidenceButDenialIsNot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		outcome    providers.ResourceSpecOutcome
		detail     string
		wantMarker string
		// mustDisclaim: the output must state that it says nothing about existence.
		mustDisclaim bool
		// mustAssert: the output must state that this IS evidence of absence.
		mustAssert bool
	}{
		{"found but empty", providers.ResourceFound, "", "NONE", false, true},
		{"forbidden", providers.ResourceForbidden, `ciliumnetworkpolicies.cilium.io is forbidden`, "FORBIDDEN", true, false},
		{"unknown kind", providers.ResourceKindUnknown, `this cluster serves no kind "Widget"`, "UNKNOWN KIND", true, false},
		{"ambiguous kind", providers.ResourceKindAmbiguous, "served by 2 groups", "AMBIGUOUS KIND", true, false},
		{"refused", providers.ResourceRefused, "Secret objects are never listable", "REFUSED", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &fakeLister{out: providers.ResourceList{Outcome: tc.outcome, Detail: tc.detail}}
			out := callList(t, l, `{"kind":"CiliumNetworkPolicy","namespace":"demo"}`)
			if !strings.Contains(out, tc.wantMarker) {
				t.Fatalf("outcome %s not distinguishable in output:\n%s", tc.outcome, out)
			}
			if tc.mustDisclaim && !strings.Contains(out, "NOTHING about whether") {
				t.Fatalf("outcome %s must disclaim evidence of absence, got:\n%s", tc.outcome, out)
			}
			if tc.mustAssert && !strings.Contains(out, "IS evidence") {
				t.Fatalf("an empty successful listing must be stated as evidence, got:\n%s", out)
			}
			// A denial must never be renderable as an empty namespace.
			if !tc.mustAssert && strings.Contains(out, "IS evidence") {
				t.Fatalf("outcome %s must not claim evidence of absence:\n%s", tc.outcome, out)
			}
		})
	}
}

// TestListingNamesTheObjects covers the case the tool was built for: the model has a Kind
// and a namespace and needs the NAMES, which is what it could not obtain by guessing.
func TestListingNamesTheObjects(t *testing.T) {
	l := &fakeLister{out: providers.ResourceList{
		Outcome:    providers.ResourceFound,
		APIVersion: "cilium.io/v2",
		Query:      providers.ResourceListQuery{Kind: "CiliumNetworkPolicy", Namespace: "demo"},
		Items: []providers.ResourceListItem{
			{Name: "orders-api-allow-payments-only", Namespace: "demo"},
			{Name: "default-deny", Namespace: "demo"},
		},
	}}
	out := callList(t, l, `{"kind":"CiliumNetworkPolicy","namespace":"demo"}`)
	for _, want := range []string{"orders-api-allow-payments-only", "default-deny", "cilium.io/v2", "2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing must contain %q, got:\n%s", want, out)
		}
	}
}

// TestTruncationIsDisclosed matters because a truncated listing silently reintroduces the
// bug this tool removes: a name missing from a capped page would otherwise read as proof
// the object does not exist.
func TestTruncationIsDisclosed(t *testing.T) {
	l := &fakeLister{out: providers.ResourceList{
		Outcome:   providers.ResourceFound,
		Items:     []providers.ResourceListItem{{Name: "a", Namespace: "demo"}},
		Truncated: true,
	}}
	out := callList(t, l, `{"kind":"ConfigMap","namespace":"demo"}`)
	if !strings.Contains(out, "TRUNCATED") {
		t.Fatalf("a truncated listing must say so, got:\n%s", out)
	}
	if strings.Contains(out, "IS evidence") {
		t.Fatalf("a truncated listing must not be rendered as evidence of absence:\n%s", out)
	}
}

// TestArgsReachTheLister keeps the schema and the query in step — a namespace or selector
// dropped on the floor would silently widen or narrow what the model believes it asked.
func TestArgsReachTheLister(t *testing.T) {
	l := &fakeLister{out: providers.ResourceList{Outcome: providers.ResourceFound}}
	callList(t, l, `{"kind":"NetworkPolicy","namespace":"demo","group":"networking.k8s.io","labelSelector":"app=x"}`)
	want := providers.ResourceListQuery{Kind: "NetworkPolicy", Namespace: "demo", Group: "networking.k8s.io", LabelSelector: "app=x"}
	if l.got != want {
		t.Fatalf("query not forwarded intact:\ngot  %+v\nwant %+v", l.got, want)
	}
}

// TestUnconfiguredListerSaysSo mirrors resource_spec: no cluster access is a statement
// about the agent, not about the cluster.
func TestUnconfiguredListerSaysSo(t *testing.T) {
	out, err := ResourceListTool{}.Call(context.Background(), `{"kind":"Pod","namespace":"demo"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "not configured") {
		t.Fatalf("unconfigured lister must say so, got: %s", out)
	}
}

// TestSchemaIsValidJSONAndRequiresKind guards the contract the model is handed.
func TestSchemaIsValidJSONAndRequiresKind(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal([]byte(ResourceListTool{}.Schema()), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "kind" {
		t.Fatalf("kind must be the only required arg, got %v", schema.Required)
	}
	for _, p := range []string{"kind", "namespace", "group", "labelSelector"} {
		if _, ok := schema.Properties[p]; !ok {
			t.Fatalf("schema is missing property %q", p)
		}
	}
}
