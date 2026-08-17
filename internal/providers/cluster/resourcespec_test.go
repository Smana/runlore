// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Smana/runlore/internal/providers"
)

// fakeDiscovery serves a fixed resource list, so resolution is testable without an API
// server. err is returned alongside the lists to model a PARTIAL discovery failure (one
// broken aggregated APIService), which must not make the whole read fail.
type fakeDiscovery struct {
	lists []*metav1.APIResourceList
	err   error
}

func (f fakeDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return f.lists, f.err
}

func discoServing(gv string, res ...metav1.APIResource) fakeDiscovery {
	return fakeDiscovery{lists: []*metav1.APIResourceList{{GroupVersion: gv, APIResources: res}}}
}

// TestResourceSpecRefusesSecretByKind is the load-bearing security property, and it is
// checked BEFORE any client or mapper is touched — hence the nil client here, which also
// proves no API call is made.
//
// Refusal is by KIND rather than by redaction on purpose: a Secret is entirely sensitive,
// so a redactor missing one pattern leaks the whole object. Refusing fails closed;
// redacting fails open.
func TestResourceSpecRefusesSecretByKind(t *testing.T) {
	r := NewSpecReader(nil, nil)
	for _, kind := range []string{"Secret", "secret", "SECRET"} {
		got, err := r.ResourceSpec(context.Background(), providers.Workload{
			Kind: kind, Name: "runlore-secrets", Namespace: "runlore",
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", kind, err)
		}
		if got.Outcome != providers.ResourceForbidden {
			t.Errorf("%s: outcome = %q, want forbidden", kind, got.Outcome)
		}
		if got.Spec != "" || got.Status != "" {
			t.Errorf("%s: refused read still returned content", kind)
		}
		if !strings.Contains(got.Detail, "never readable") {
			t.Errorf("%s: refusal does not explain itself: %q", kind, got.Detail)
		}
	}
}

// TestResourceSpecUnknownKindIsNotAbsence pins the taxonomy at the reader boundary: a kind
// this cluster does not serve must NOT be reported as an absent object. This is the exact
// conflation that made gitops_resource_status produce a fabricated fact.
func TestResourceSpecUnknownKindIsNotAbsence(t *testing.T) {
	// Discovery serves nothing matching, so the kind cannot resolve.
	r := NewSpecReader(nil, discoServing("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true}))
	got, err := r.ResourceSpec(context.Background(), providers.Workload{Kind: "Widget", Name: "w", Namespace: "n"})
	if err != nil {
		t.Fatalf("an unknown kind must be an OUTCOME, not an error: %v", err)
	}
	if got.Outcome != providers.ResourceKindUnknown {
		t.Fatalf("outcome = %q, want kind_unknown", got.Outcome)
	}
	if got.Outcome == providers.ResourceAbsent {
		t.Fatal("an unserved kind was reported as an absent object")
	}
}

// TestResourceSpecReadsSpecAndStatus covers the happy path against a fake dynamic client,
// including that the version actually used is reported — a kind can be served at several
// versions, so "which one answered" is part of reading honestly.
func TestResourceSpecReadsSpecAndStatus(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "operator.victoriametrics.com", Version: "v1beta1", Resource: "vmservicescrapes"}
	gvk := gvr.GroupVersion().WithKind("VMServiceScrape")
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       "VMServiceScrape",
		"metadata":   map[string]any{"name": "datagrok-rabbitmq", "namespace": "observability"},
		"spec": map[string]any{
			"namespaceSelector": map[string]any{"matchNames": []any{"datagrok"}},
		},
		"status": map[string]any{"conditions": []any{}},
	}}
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "VMServiceScrapeList"}, obj)
	disco := discoServing(gvr.GroupVersion().String(),
		metav1.APIResource{Name: gvr.Resource, Kind: gvk.Kind, Namespaced: true})

	r := NewSpecReader(client, disco)
	got, err := r.ResourceSpec(context.Background(), providers.Workload{
		Kind: "VMServiceScrape", Name: "datagrok-rabbitmq", Namespace: "observability",
	})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome != providers.ResourceFound {
		t.Fatalf("outcome = %q (detail %q), want found", got.Outcome, got.Detail)
	}
	// The field the live investigation needed and could not read: it inferred the object
	// had been deleted when its namespaceSelector simply pointed at an absent namespace.
	if !strings.Contains(got.Spec, "datagrok") {
		t.Errorf("spec does not carry namespaceSelector.matchNames:\n%s", got.Spec)
	}
	if got.APIVersion != gvr.GroupVersion().String() {
		t.Errorf("APIVersion = %q, want %q", got.APIVersion, gvr.GroupVersion().String())
	}
}

// TestResourceSpecMissingObjectIsAbsent: for a SERVED kind, a genuinely missing object is
// the one outcome that IS evidence of absence.
func TestResourceSpecMissingObjectIsAbsent(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	gvk := gvr.GroupVersion().WithKind("Service")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "ServiceList"})
	disco := discoServing("v1", metav1.APIResource{Name: "services", Kind: gvk.Kind, Namespaced: true})

	got, err := NewSpecReader(client, disco).ResourceSpec(context.Background(),
		providers.Workload{Kind: "Service", Name: "nope", Namespace: "default"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome != providers.ResourceAbsent {
		t.Fatalf("outcome = %q, want absent", got.Outcome)
	}
}

// TestRenderSectionTruncatesOnALineBoundary: an unstructured object has no natural size
// ceiling, and one object must not consume the whole tool-output budget and evict the rest
// of the evidence. Cutting on a line boundary keeps the result reading as truncated YAML
// rather than as corruption.
func TestRenderSectionTruncatesOnALineBoundary(t *testing.T) {
	big := make([]any, 4000)
	for i := range big {
		big[i] = "a-fairly-long-entry-to-blow-past-the-section-cap"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"items": big}}}
	out := renderSection(obj, "spec")
	if len(out) > maxSpecBytes+64 {
		t.Fatalf("section not bounded: %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "… (truncated)") {
		t.Fatalf("a cut section must say it was cut:\n%s", out[max(0, len(out)-120):])
	}
	if strings.Contains(strings.TrimSuffix(out, "\n… (truncated)"), "\n\n") {
		t.Error("truncation left a dangling blank line")
	}
	// Absent and nil sections are "" rather than an error: many kinds have no spec.
	if got := renderSection(&unstructured.Unstructured{Object: map[string]any{}}, "spec"); got != "" {
		t.Errorf("missing section = %q, want empty", got)
	}
}

// TestResourceSpecAmbiguousKindReadsNothing pins the choice to report an ambiguity rather
// than resolve it. "Ingress" was served by both networking.k8s.io and extensions, and
// picking one silently would read a DIFFERENT object than the caller meant and then
// describe it confidently — the exact failure this tool exists to remove.
func TestResourceSpecAmbiguousKindReadsNothing(t *testing.T) {
	disco := fakeDiscovery{lists: []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Namespaced: true}}},
		{GroupVersion: "extensions/v1beta1", APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Namespaced: true}}},
	}}
	// nil client: an ambiguous kind must be refused before any read is attempted.
	got, err := NewSpecReader(nil, disco).ResourceSpec(context.Background(),
		providers.Workload{Kind: "Ingress", Name: "web", Namespace: "apps"})
	if err != nil {
		t.Fatalf("ambiguity must be an outcome, not an error: %v", err)
	}
	if got.Outcome == providers.ResourceAbsent || got.Outcome == providers.ResourceFound {
		t.Fatalf("outcome = %q: an ambiguous kind must not read or claim absence", got.Outcome)
	}
	for _, want := range []string{"more than one API group", "networking.k8s.io/v1", "extensions/v1beta1"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail does not name the ambiguity (%q): %q", want, got.Detail)
		}
	}
}

// TestResolveKindSkipsSubresourcesAndSurvivesPartialDiscovery: "pods/log" is not an object
// with a spec, and a single broken aggregated APIService must not make every read fail —
// discovery returns usable lists alongside its error in that case.
func TestResolveKindSkipsSubresourcesAndSurvivesPartialDiscovery(t *testing.T) {
	disco := fakeDiscovery{
		lists: []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods/log", Kind: "Pod", Namespaced: true},
			{Name: "pods", Kind: "Pod", Namespaced: true},
		}}},
		err: errors.New("unable to retrieve the complete list of server APIs: metrics.k8s.io/v1beta1"),
	}
	got, err := NewSpecReader(nil, disco).resolveKind("Pod")
	if err != nil {
		t.Fatalf("a partial discovery failure must not fail the read: %v", err)
	}
	if len(got) != 1 || got[0].gvr.Resource != "pods" {
		t.Fatalf("resolveKind = %+v, want exactly the pods resource (subresource skipped)", got)
	}
	// But a TOTAL discovery failure — nothing usable came back — must surface.
	if _, err := NewSpecReader(nil, fakeDiscovery{err: errors.New("connection refused")}).resolveKind("Pod"); err == nil {
		t.Error("a total discovery failure must be an error, not silently zero matches")
	}
}
