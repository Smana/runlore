// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

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

// invalidatableDiscovery models the MEMOISED discovery client the reader gets in
// production (client-go's memcache): it serves nothing until Invalidate() is called,
// after which `after` is served. memcache caches failures as permanently as successes,
// so a CRD installed after startup — or one aggregated APIService that blipped — stays
// invisible for the process's whole life unless the cache is dropped and retried.
type invalidatableDiscovery struct {
	after       []*metav1.APIResourceList
	invalidated int
}

func (d *invalidatableDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	if d.invalidated == 0 {
		return nil, errors.New("unable to retrieve the complete list of server APIs")
	}
	return d.after, nil
}

func (d *invalidatableDiscovery) Invalidate() { d.invalidated++ }

// TestResourceSpecRefusesSecretByKind is the load-bearing security property, and it is
// checked BEFORE any client or mapper is touched — hence the nil client here, which also
// proves no API call is made.
//
// Refusal is by KIND rather than by redaction on purpose: a Secret is entirely sensitive,
// so a redactor missing one pattern leaks the whole object. Refusing fails closed;
// redacting fails open.
func TestResourceSpecRefusesSecretByKind(t *testing.T) {
	r := NewSpecReader(nil, nil)
	for _, kind := range []string{"Secret", "secret", "SECRET", "ſecret"} {
		got, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{
			Kind: kind, Name: "runlore-secrets", Namespace: "runlore",
		})
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", kind, err)
		}
		if got.Outcome != providers.ResourceRefused {
			t.Errorf("%q: outcome = %q, want refused", kind, got.Outcome)
		}
		if got.Spec != "" || got.Status != "" {
			t.Errorf("%q: refused read still returned content", kind)
		}
		if !strings.Contains(got.Detail, "never readable") {
			t.Errorf("%q: refusal does not explain itself: %q", kind, got.Detail)
		}
	}
}

// TestResourceSpecRefusalSurvivesCaseFolding is the S2 regression, and the same defect
// class as the IDN homoglyph bypass in #498: a case-folding MISMATCH between the check
// and the use.
//
// The refusal used strings.ToLower while resolution used strings.EqualFold, and the two
// disagree on Unicode simple folding. U+017F LATIN SMALL LETTER LONG S lowercases to
// itself, so "ſecret" was not in the refused map — yet EqualFold("Secret", "ſecret") is
// true, so it resolved to v1/secrets and was read. It is reachable by prompt injection
// through an alert annotation.
//
// This is the end-to-end case: served kind, live object, and the bypass spelling. The two
// halves it depends on are pinned separately — TestRefusalAndResolutionAgreeOnFolding for
// the pre-check's folding, TestResourceSpecRefusesASecretsResourceUnderAnyKind for the
// post-resolution refusal.
func TestResourceSpecRefusalSurvivesCaseFolding(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "runlore-secrets", "namespace": "runlore"},
		// A stock Secret has no spec, which is why the live bypass is an existence
		// ORACLE rather than a dump. A Secret-shaped CRD does have one, and that is
		// what turns the oracle into a leak — so the fixture carries one, and the
		// test fails loudly instead of passing on an empty payload.
		"spec": map[string]any{"password": "pr0d-Pa55w0rd-x9"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "SecretList"}, obj)
	disco := discoServing("v1", metav1.APIResource{Name: "secrets", Kind: "Secret", Namespaced: true})

	got, err := NewSpecReader(client, disco).ResourceSpec(context.Background(),
		providers.ResourceSpecQuery{Kind: "ſecret", Name: "runlore-secrets", Namespace: "runlore"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome != providers.ResourceRefused {
		t.Fatalf("outcome = %q, want refused: a case-folding variant defeated the refusal", got.Outcome)
	}
	if got.Spec != "" || got.Status != "" {
		t.Fatalf("a refused kind still rendered content:\nspec=%q\nstatus=%q", got.Spec, got.Status)
	}
}

// TestResourceSpecRefusesASecretsResourceUnderAnyKind: the pre-check is by KIND and never
// sees the resolved GVR, so a CRD or an aggregated API server exposing a `secrets`
// resource under some other Kind slipped through it entirely. Refusing the RESOURCE name
// after resolution, in any group, closes that.
func TestResourceSpecRefusesASecretsResourceUnderAnyKind(t *testing.T) {
	disco := discoServing("vault.example.com/v1",
		metav1.APIResource{Name: "secrets", Kind: "VaultCredential", Namespaced: true})
	// nil client: the refusal must land before any read is attempted.
	got, err := NewSpecReader(nil, disco).ResourceSpec(context.Background(),
		providers.ResourceSpecQuery{Kind: "VaultCredential", Name: "db", Namespace: "apps"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome != providers.ResourceRefused {
		t.Fatalf("outcome = %q, want refused for a resource named secrets", got.Outcome)
	}
}

// TestResourceSpecUnknownKindIsNotAbsence pins the taxonomy at the reader boundary: a kind
// this cluster does not serve must NOT be reported as an absent object. This is the exact
// conflation that made gitops_resource_status produce a fabricated fact.
func TestResourceSpecUnknownKindIsNotAbsence(t *testing.T) {
	// Discovery serves nothing matching, so the kind cannot resolve.
	r := NewSpecReader(nil, discoServing("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true}))
	got, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{Kind: "Widget", Name: "w", Namespace: "n"})
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
	got, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{
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
		providers.ResourceSpecQuery{Kind: "Service", Name: "nope", Namespace: "default"})
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

// TestRenderSectionRedactsBeforeTruncating is the S3 regression. The documented
// invariant (website/content/docs/security/security-architecture.md) is:
//
//	"Redaction runs before truncation, so a secret that straddles the size cap is
//	 masked, not sliced."
//
// renderSection inverted it — it cut first and left redaction to the caller — so an
// anchored rule lost its anchor. A PEM block whose -----END----- falls past the cap has
// no terminator left to match, and ~8 KB of key body shipped verbatim.
func TestRenderSectionRedactsBeforeTruncating(t *testing.T) {
	const line = "MIIEowIBAAKCAQEAx7Rm9d3sLpQ2vN8kZ1fT"
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat(line+"\n", 400) + "-----END RSA PRIVATE KEY-----"
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"tls.key": pem}}}
	out := renderSection(obj, "spec")
	if strings.Contains(out, line) {
		t.Fatalf("a private key straddling the cap shipped verbatim:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "[REDACTED PRIVATE KEY]") {
		t.Fatalf("the key was neither masked nor recognisable as masked:\n%s", out[:min(len(out), 300)])
	}
}

// TestRenderSectionDropsAPartialLine covers the branch the old test was NAMED after but
// never reached: when the first maxSpecBytes contain no newline at all, the cut fell back
// to the byte offset and sliced mid-token. A ghp_/JWT/AKIA value straddling that offset
// survived as an unrecognisable — and therefore unmasked — fragment.
//
// Redaction now runs first, so the token is already masked; the partial line is dropped
// as well, because a fragment of an UNRECOGNISED secret is still a fragment of a secret.
func TestRenderSectionDropsAPartialLine(t *testing.T) {
	tok := "ghp_" + strings.Repeat("A", 36)
	// One scalar, no spaces: the YAML emitter has nowhere to fold it, so the rendered
	// section is a single line longer than the cap.
	// The token straddles the cap; the trailing padding is non-alphanumeric so the
	// token rule cannot swallow it and shorten the line back under the cap.
	one := strings.Repeat("x", maxSpecBytes-20) + tok + strings.Repeat("-z", 200)
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"k": one}}}
	out := renderSection(obj, "spec")
	if strings.Contains(out, "ghp_") {
		t.Fatalf("a token sliced by the cap survived: %q", out[max(0, len(out)-120):])
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("a dropped section must say it was cut: %q", out)
	}
	// The partial line is DROPPED, not sliced. Nothing of that line may survive: a
	// fragment of a token the ruleset did not recognise is still a fragment of a secret,
	// and nothing downstream can recognise it either.
	if strings.Contains(out, "xxxx") {
		t.Fatalf("the over-long line was sliced rather than dropped: %q", out[:min(len(out), 120)])
	}
	if len(out) > maxSpecBytes+64 {
		t.Fatalf("section not bounded: %d bytes", len(out))
	}
}

// TestResourceSpecMasksContainerEnvValues is the S1 regression, at the wiring level.
//
// redact.Secrets is KEY-NAME oriented — it masks `password: <value>`. Kubernetes' env
// shape puts the sensitive word in the VALUE of `name:` and the credential under the
// literal key `value:`, which is in no keyword vocabulary, so the whole env block reached
// the model in plaintext. The chart grants `pods` get/list CLUSTER-WIDE and no tool
// exposed a pod's .spec before this one, so it was a new egress category.
//
// The values here deliberately carry NO recognisable token prefix: a ghp_-shaped value
// would be masked by the string rules and prove nothing.
func TestResourceSpecMasksContainerEnvValues(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "api-0", "namespace": "apps"},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name":  "api",
			"image": "registry.k8s.io/pause:3.9",
			"env": []any{
				map[string]any{"name": "POSTGRES_PASSWORD", "value": "pr0d-Pa55w0rd-x9"},
				map[string]any{"name": "REDIS_AUTH", "value": "hunter2hunter2"},
				map[string]any{"name": "DB_PASSWORD", "value": "hunter2supersecret"},
				map[string]any{"name": "LOG_LEVEL", "value": "debug"},
			},
		}}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "PodList"}, obj)
	disco := discoServing("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true})

	got, err := NewSpecReader(client, disco).ResourceSpec(context.Background(),
		providers.ResourceSpecQuery{Kind: "Pod", Name: "api-0", Namespace: "apps"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	for _, cred := range []string{"pr0d-Pa55w0rd-x9", "hunter2hunter2", "hunter2supersecret"} {
		if strings.Contains(got.Spec, cred) {
			t.Errorf("env credential %q reached the model in plaintext:\n%s", cred, got.Spec)
		}
	}
	// Structure survives: the model still needs to see WHICH variables are set, and
	// benign configuration must not be destroyed on the way.
	for _, keep := range []string{"POSTGRES_PASSWORD", "LOG_LEVEL", "debug", "registry.k8s.io/pause:3.9"} {
		if !strings.Contains(got.Spec, keep) {
			t.Errorf("%q should survive redaction:\n%s", keep, got.Spec)
		}
	}
}

// TestResourceSpecClassifiesADenial pins CLASSIFICATION, not rendering: the
// IsForbidden/IsUnauthorized branch had no coverage at all, because the taxonomy tests
// drive a fake whose outcome is pre-set — they never exercise the code that decides which
// outcome an API error is. A denial silently classified as absence is the #503 defect.
func TestResourceSpecClassifiesADenial(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "operator.victoriametrics.com", Version: "v1beta1", Resource: "vmservicescrapes"}
	disco := discoServing(gvr.GroupVersion().String(),
		metav1.APIResource{Name: gvr.Resource, Kind: "VMServiceScrape", Namespaced: true})
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", apierrors.NewForbidden(gvr.GroupResource(), "datagrok-rabbitmq",
			errors.New(`User "system:serviceaccount:runlore:runlore" cannot get resource "vmservicescrapes"`))},
		{"unauthorized", apierrors.NewUnauthorized("token expired")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{gvr: "VMServiceScrapeList"})
			client.PrependReactor("get", gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			got, err := NewSpecReader(client, disco).ResourceSpec(context.Background(),
				providers.ResourceSpecQuery{Kind: "VMServiceScrape", Name: "datagrok-rabbitmq", Namespace: "observability"})
			if err != nil {
				t.Fatalf("a denial must be an OUTCOME, not an error: %v", err)
			}
			if got.Outcome != providers.ResourceForbidden {
				t.Fatalf("outcome = %q, want forbidden", got.Outcome)
			}
			if got.Outcome == providers.ResourceAbsent {
				t.Fatal("a denial was reported as absence")
			}
			if got.Detail == "" {
				t.Error("a denial must carry the server's own message")
			}
		})
	}
}

// TestResourceSpecResourceLevel404IsNotAbsence: apierrors.IsNotFound is true for BOTH
// "no such object" and "no such resource at this path" (a CRD deleted since discovery
// ran, an aggregated APIService that stopped serving). Reporting the second as absence
// tells the model "this IS evidence the object does not exist" about an object the
// server was never asked for — the #503 conflation this batch exists to remove.
func TestResourceSpecResourceLevel404IsNotAbsence(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "operator.victoriametrics.com", Version: "v1beta1", Resource: "vmservicescrapes"}
	disco := discoServing(gvr.GroupVersion().String(),
		metav1.APIResource{Name: gvr.Resource, Kind: "VMServiceScrape", Namespaced: true})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "VMServiceScrapeList"})
	// What a real API server returns for an unserved path: reason NotFound, code 404,
	// and NO details naming an object.
	client.PrependReactor("get", gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Reason:  metav1.StatusReasonNotFound,
			Code:    404,
			Message: "the server could not find the requested resource",
		}}
	})
	got, err := NewSpecReader(client, disco).ResourceSpec(context.Background(),
		providers.ResourceSpecQuery{Kind: "VMServiceScrape", Name: "datagrok-rabbitmq", Namespace: "observability"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome == providers.ResourceAbsent {
		t.Fatal("a resource-level 404 was reported as evidence the OBJECT does not exist")
	}
	if got.Outcome != providers.ResourceKindUnknown {
		t.Fatalf("outcome = %q, want kind_unknown", got.Outcome)
	}
}

// TestResourceSpecClusterScopedIsNotNamespaced: a cluster-scoped kind used to be read
// anyway when a namespace was supplied, and the bogus namespace was then rendered back as
// fact ("StorageClass totally-made-up-ns/fast"). The identity must come from the object
// that was actually read.
func TestResourceSpecClusterScopedIsNotNamespaced(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata": map[string]any{"name": "fast"},
		"spec":     map[string]any{"provisioner": "ebs.csi.aws.com"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "StorageClassList"}, obj)
	disco := discoServing(gvr.GroupVersion().String(),
		metav1.APIResource{Name: gvr.Resource, Kind: "StorageClass", Namespaced: false})
	r := NewSpecReader(client, disco)

	// With a namespace the caller invented.
	got, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{
		Kind: "StorageClass", Name: "fast", Namespace: "totally-made-up-ns"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome != providers.ResourceFound {
		t.Fatalf("outcome = %q (detail %q), want found", got.Outcome, got.Detail)
	}
	if got.Query.Namespace != "" {
		t.Errorf("a cluster-scoped object came back namespaced as %q", got.Query.Namespace)
	}
	// And without one, which is the normal call: a cluster-scoped kind must not demand
	// a namespace.
	bare, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{Kind: "StorageClass", Name: "fast"})
	if err != nil {
		t.Fatalf("a cluster-scoped kind must not require a namespace: %v", err)
	}
	if bare.Outcome != providers.ResourceFound {
		t.Fatalf("outcome = %q (detail %q), want found", bare.Outcome, bare.Detail)
	}
	// The non-found endings need the same treatment: there is no object to take the
	// identity from, so an invented namespace would otherwise be rendered alongside a
	// statement the model treats as evidence ("StorageClass made-up/gone: ABSENT").
	gone, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{
		Kind: "StorageClass", Name: "gone", Namespace: "totally-made-up-ns"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if gone.Outcome != providers.ResourceAbsent {
		t.Fatalf("outcome = %q, want absent", gone.Outcome)
	}
	if gone.Query.Namespace != "" {
		t.Errorf("an absent cluster-scoped object came back namespaced as %q", gone.Query.Namespace)
	}
}

// TestResourceSpecCanonicalisesTheKind: the resolved kind is echoed in the casing the
// SERVER serves it under, so "vmservicescrape" off an alert comes back as
// "VMServiceScrape" rather than being repeated as the caller mistyped it.
func TestResourceSpecCanonicalisesTheKind(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "operator.victoriametrics.com", Version: "v1beta1", Resource: "vmservicescrapes"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(), "kind": "VMServiceScrape",
		"metadata": map[string]any{"name": "rabbit", "namespace": "observability"},
		"spec":     map[string]any{"jobLabel": "app"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "VMServiceScrapeList"}, obj)
	disco := discoServing(gvr.GroupVersion().String(),
		metav1.APIResource{Name: gvr.Resource, Kind: "VMServiceScrape", Namespaced: true})
	got, err := NewSpecReader(client, disco).ResourceSpec(context.Background(),
		providers.ResourceSpecQuery{Kind: "vmservicescrape", Name: "rabbit", Namespace: "observability"})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Query.Kind != "VMServiceScrape" || got.Query.Group != gvr.Group {
		t.Errorf("query echo = %+v, want the resolved kind and group", got.Query)
	}
}

// TestResolveKindInvalidatesStaleDiscovery: discovery is memoised by a memcache client,
// and memcache caches FAILURES as well as successes. Without an invalidation, a CRD
// installed after RunLore started is kind_unknown for the pod's whole lifetime, and one
// aggregated-APIService blip poisons a group forever — both of which read to the model as
// "this cluster serves no such kind".
func TestResolveKindInvalidatesStaleDiscovery(t *testing.T) {
	d := &invalidatableDiscovery{
		after: []*metav1.APIResourceList{{GroupVersion: "operator.victoriametrics.com/v1beta1",
			APIResources: []metav1.APIResource{{Name: "vmservicescrapes", Kind: "VMServiceScrape", Namespaced: true}}}},
	}
	got, err := NewSpecReader(nil, d).resolveKind("VMServiceScrape")
	if err != nil {
		t.Fatalf("resolveKind: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a kind served only after invalidation was not found: %+v (invalidated=%d)", got, d.invalidated)
	}
	if d.invalidated != 1 {
		t.Errorf("Invalidate() called %d times, want exactly 1", d.invalidated)
	}
	// A kind that is genuinely unserved must not invalidate on EVERY call beyond the
	// first retry — one refresh per miss, not a loop.
	d2 := &invalidatableDiscovery{}
	if _, err := NewSpecReader(nil, d2).resolveKind("Widget"); err != nil {
		t.Fatalf("resolveKind: %v", err)
	}
	if d2.invalidated != 1 {
		t.Errorf("Invalidate() called %d times for an unserved kind, want exactly 1", d2.invalidated)
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
		providers.ResourceSpecQuery{Kind: "Ingress", Name: "web", Namespace: "apps"})
	if err != nil {
		t.Fatalf("ambiguity must be an outcome, not an error: %v", err)
	}
	// The exact outcome, not merely "neither absent nor found": ambiguity is its OWN
	// ending, distinct from kind_unknown, because the caller's next move differs. An
	// unknown kind means re-check the spelling; an ambiguous one means name the group.
	if got.Outcome != providers.ResourceKindAmbiguous {
		t.Fatalf("outcome = %q, want kind_ambiguous", got.Outcome)
	}
	for _, want := range []string{"more than one API group", "networking.k8s.io", "extensions"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail does not name the ambiguity (%q): %q", want, got.Detail)
		}
	}
	// And it must be a DEAD END no longer: the detail has to tell the caller how to
	// resolve it, with the argument that actually exists.
	if !strings.Contains(got.Detail, "group") {
		t.Errorf("an ambiguity the caller cannot resolve is a dead end: %q", got.Detail)
	}
}

// TestResourceSpecGroupDisambiguates: the way OUT of an ambiguity. NetworkPolicy is
// served by both networking.k8s.io and crd.projectcalico.org on any Calico cluster, and
// it is one of the five examples in the issue this tool closes — so "ambiguous" had to
// stop being a terminal answer.
func TestResourceSpecGroupDisambiguates(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": "deny-all", "namespace": "apps"},
		"spec":     map[string]any{"podSelector": map[string]any{}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "NetworkPolicyList"}, obj)
	disco := fakeDiscovery{lists: []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}}},
		{GroupVersion: "crd.projectcalico.org/v1", APIResources: []metav1.APIResource{
			{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}}},
	}}
	r := NewSpecReader(client, disco)
	got, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{
		Kind: "NetworkPolicy", Name: "deny-all", Namespace: "apps", Group: "networking.k8s.io",
	})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if got.Outcome != providers.ResourceFound {
		t.Fatalf("outcome = %q (detail %q), want found", got.Outcome, got.Detail)
	}
	if got.APIVersion != "networking.k8s.io/v1" {
		t.Errorf("APIVersion = %q, want the group the caller asked for", got.APIVersion)
	}
	// A group nothing serves must not silently fall back to the other candidate —
	// reading a DIFFERENT object than the caller named is the failure this tool exists
	// to remove.
	other, err := r.ResourceSpec(context.Background(), providers.ResourceSpecQuery{
		Kind: "NetworkPolicy", Name: "deny-all", Namespace: "apps", Group: "nope.example.com",
	})
	if err != nil {
		t.Fatalf("ResourceSpec: %v", err)
	}
	if other.Outcome == providers.ResourceFound {
		t.Fatal("an unserved group fell back to another group's object")
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

// TestRefusalAndResolutionAgreeOnFolding pins the equivalence the refusal RELIES on.
//
// The bypass in S2 was exactly this equivalence breaking: the refusal folded with
// strings.ToLower while resolution folded with strings.EqualFold, and "ſecret" (U+017F)
// sits in the gap — ToLower leaves it alone, EqualFold matches it against "Secret". Any
// spelling that RESOLVES to a refused kind must also be REFUSED, or a bypass exists; any
// spelling that does not resolve must not be refused, or ordinary kinds become unreadable.
//
// The post-resolution check covers a Secret reached under another Kind; this covers the
// pre-check, which is what keeps a refused kind from ever reaching the client at all.
func TestRefusalAndResolutionAgreeOnFolding(t *testing.T) {
	for _, s := range []string{"Secret", "secret", "SECRET", "SeCrEt", "ſecret", "Secrets", "sealedsecret", "Sekret", ""} {
		_, refused := refusalFor(s)
		// strings.EqualFold is the matcher resolveKind uses against a served ar.Kind.
		if want := strings.EqualFold("Secret", s); refused != want {
			t.Errorf("refusalFor(%q) = %v, but resolution would match it = %v: "+
				"the refusal and resolution fold differently, which is a bypass", s, refused, want)
		}
	}
}
