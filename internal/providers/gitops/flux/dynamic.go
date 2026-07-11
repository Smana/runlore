// SPDX-License-Identifier: Apache-2.0

package flux

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// fluxSystemNamespace is where Flux objects conventionally live, regardless of the
// namespace a workload they manage runs in.
const fluxSystemNamespace = "flux-system"

// Flux CRD resources (v1).
var (
	kustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	helmReleaseGVR   = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	gitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	eventsGVR        = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
)

// kindToGVR maps the Flux Kinds the inspector understands to their GVR.
var kindToGVR = map[string]schema.GroupVersionResource{
	"Kustomization":    kustomizationGVR,
	"GitRepository":    gitRepositoryGVR,
	"HelmRelease":      helmReleaseGVR,
	"OCIRepository":    {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"},
	"HelmRepository":   {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmrepositories"},
	"HelmChart":        {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmcharts"},
	"Bucket":           {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "buckets"},
	"ExternalArtifact": {Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "externalartifacts"},
}

// dynamicReader reads Flux CRDs as unstructured objects via the dynamic client.
type dynamicReader struct {
	client dynamic.Interface
}

// NewDynamicReader builds a Reader backed by a client-go dynamic client.
func NewDynamicReader(client dynamic.Interface) Reader {
	return &dynamicReader{client: client}
}

// ListKustomizations lists all Flux Kustomizations across all namespaces.
func (r *dynamicReader) ListKustomizations(ctx context.Context) ([]kustomization, error) {
	list, err := r.client.Resource(kustomizationGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list kustomizations: %w", err)
	}
	out := make([]kustomization, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, kustomizationFromUnstructured(&list.Items[i]))
	}
	return out, nil
}

// GetGitRepository retrieves a specific Flux GitRepository by namespace and name.
func (r *dynamicReader) GetGitRepository(ctx context.Context, namespace, name string) (gitRepository, error) {
	u, err := r.client.Resource(gitRepositoryGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return gitRepository{}, fmt.Errorf("get gitrepository %s/%s: %w", namespace, name, err)
	}
	url, _, _ := unstructured.NestedString(u.Object, "spec", "url")
	return gitRepository{Name: name, Namespace: namespace, URL: url}, nil
}

// SourceRevision returns a Flux source's current synced revision from
// status.artifact.revision. The kind is the Kustomization's spec.sourceRef.kind
// (GitRepository/OCIRepository/Bucket/ExternalArtifact); an empty kind defaults to
// GitRepository, matching Flux's own default. This is the source HEAD a failing
// Kustomization's pinned lastAppliedRevision may lag behind.
func (r *dynamicReader) SourceRevision(ctx context.Context, kind, namespace, name string) (string, error) {
	if kind == "" {
		kind = "GitRepository"
	}
	gvr, ok := kindToGVR[kind]
	if !ok {
		return "", fmt.Errorf("unsupported source kind %q", kind)
	}
	u, err := r.client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}
	rev, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "revision")
	return rev, nil
}

// GetResource fetches one object by kind/namespace/name. The kind must be one the
// inspector knows (see kindToGVR). A NotFound error is returned verbatim so callers
// can distinguish "missing" from other failures.
func (r *dynamicReader) GetResource(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error) {
	gvr, ok := kindToGVR[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
	u, err := r.client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return u, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	// Namespace fallback. Flux objects usually live in flux-system, not the
	// namespace of the workload they manage — so a caller passing the workload's
	// namespace (e.g. an alert's "apps") would otherwise get a misleading NotFound
	// that reads as "the resource doesn't exist". Retry in flux-system, then search
	// all namespaces by name, before trusting the NotFound.
	if namespace != fluxSystemNamespace {
		if u2, err2 := r.client.Resource(gvr).Namespace(fluxSystemNamespace).Get(ctx, name, metav1.GetOptions{}); err2 == nil {
			return u2, nil
		}
	}
	if list, lerr := r.client.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); lerr == nil {
		for i := range list.Items {
			if list.Items[i].GetName() == name {
				return &list.Items[i], nil
			}
		}
	}
	return nil, err // genuinely absent in every namespace: return the original NotFound
}

// ListEvents returns recent Event lines for an object, filtered client-side by the
// involved object's name (and kind, when given). Each line is rendered as
// "<lastTimestamp> Type Reason(xN) Message" — mirroring the kube_events tool so a
// GitOps event carries the same WHEN + repeat-count context (RunLore G2): the
// timestamp lets the model correlate the event to a change/deploy time and (xN)
// shows a flapping reconcile. Timestamp/count are omitted when the API doesn't set them.
func (r *dynamicReader) ListEvents(ctx context.Context, namespace, name, kind string) ([]string, error) {
	// Filter server-side by the involved object and cap the result — a busy
	// namespace can hold thousands of events.
	opts := metav1.ListOptions{Limit: 100}
	if name != "" {
		opts.FieldSelector = "involvedObject.name=" + name
	}
	list, err := r.client.Resource(eventsGVR).Namespace(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	var out []string
	for i := range list.Items {
		o := list.Items[i].Object
		ioName, _, _ := unstructured.NestedString(o, "involvedObject", "name")
		if ioName != name {
			continue
		}
		if kind != "" {
			ioKind, _, _ := unstructured.NestedString(o, "involvedObject", "kind")
			if ioKind != "" && ioKind != kind {
				continue
			}
		}
		typ, _, _ := unstructured.NestedString(o, "type")
		reason, _, _ := unstructured.NestedString(o, "reason")
		msg, _, _ := unstructured.NestedString(o, "message")
		out = append(out, renderEventLine(o, typ, reason, msg))
	}
	return out, nil
}

// renderEventLine formats one Kubernetes Event as "<lastTimestamp> Type Reason(xN) Message",
// mirroring the kube_events tool (G2). The leading timestamp (RFC3339, UTC) comes from
// lastTimestamp, falling back to eventTime (the newer Events API field); it is omitted
// when neither is set. "(xN)" is appended only when count>1. This keeps GitOps inspector
// events consistent with kube_events, which already leads with WHEN and shows repeat counts.
func renderEventLine(o map[string]any, typ, reason, msg string) string {
	when := ""
	if ts := eventLastTime(o); !ts.IsZero() {
		when = ts.UTC().Format(time.RFC3339) + " "
	}
	count := ""
	if c := eventCount(o); c > 1 {
		count = fmt.Sprintf("(x%d)", c)
	}
	return fmt.Sprintf("%s%s %s%s %s", when, typ, reason, count, msg)
}

// eventLastTime returns an Event's most-recent occurrence time: lastTimestamp
// (core/v1 aggregated events), falling back to eventTime (events.k8s.io/v1). Zero
// when neither is present or parseable.
func eventLastTime(o map[string]any) time.Time {
	for _, field := range []string{"lastTimestamp", "eventTime"} {
		if s, _, _ := unstructured.NestedString(o, field); s != "" {
			if ts, err := time.Parse(time.RFC3339, s); err == nil {
				return ts
			}
		}
	}
	return time.Time{}
}

// eventCount returns an Event's occurrence count from the "count" field (unstructured
// decodes JSON numbers as int64 or float64). Zero when absent.
func eventCount(o map[string]any) int64 {
	switch c := o["count"].(type) {
	case int64:
		return c
	case float64:
		return int64(c)
	}
	return 0
}

// readyCondition returns the (status, reason, message) of the Ready condition.
func readyCondition(u *unstructured.Unstructured) (status, reason, message string) {
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return "", "", ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "Ready" {
			status, _ = m["status"].(string)
			reason, _ = m["reason"].(string)
			message, _ = m["message"].(string)
			return status, reason, message
		}
	}
	return "", "", ""
}

// readyTransitionTime returns the Ready condition's lastTransitionTime — the
// reconcile time, used as the Change.When fallback (RunLore B1). Zero if absent
// or unparseable.
func readyTransitionTime(u *unstructured.Unstructured) time.Time {
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return time.Time{}
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "Ready" {
			ltt, _ := m["lastTransitionTime"].(string)
			if ts, err := time.Parse(time.RFC3339, ltt); err == nil {
				return ts
			}
			return time.Time{}
		}
	}
	return time.Time{}
}

// watchResource watches a Flux GVR via a dynamic informer (list-watch with
// reconnection + periodic resync) and forwards each add/update, mapped by conv,
// on the returned buffered channel. Sends never block the informer (dropped under
// backpressure); the channel closes when ctx is done. Shared by the Kustomization
// and HelmRelease failure watchers so both get identical watch semantics.
func watchResource[T any](ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, conv func(*unstructured.Unstructured) T) (<-chan T, error) {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, 10*time.Minute)
	informer := factory.ForResource(gvr).Informer()

	out := make(chan T, 128)
	send := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		select {
		case out <- conv(u):
		case <-ctx.Done():
		default: // never block the informer; drop under backpressure
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { send(obj) },
		UpdateFunc: func(_, obj any) { send(obj) },
	}); err != nil {
		return nil, fmt.Errorf("add event handler: %w", err)
	}

	go func() {
		defer close(out)
		factory.Start(ctx.Done())
		<-ctx.Done()
	}()
	return out, nil
}

// WatchKustomizations watches all Kustomizations and forwards each add/update as a
// KustomizationEvent. The channel closes when ctx is done.
func (r *dynamicReader) WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error) {
	return watchResource(ctx, r.client, kustomizationGVR, func(u *unstructured.Unstructured) KustomizationEvent {
		return KustomizationEvent{Kustomization: kustomizationFromUnstructured(u)}
	})
}

// WatchHelmReleases watches all HelmReleases and forwards each add/update as a
// HelmReleaseEvent. The channel closes when ctx is done. A HelmRelease that never
// installs or upgrades (image pull failures, failed hooks, exhausted retries) goes
// Ready=False without any Kustomization flipping, so it is invisible to the
// Kustomization watch — this closes that gap (runlore#306).
func (r *dynamicReader) WatchHelmReleases(ctx context.Context) (<-chan HelmReleaseEvent, error) {
	return watchResource(ctx, r.client, helmReleaseGVR, func(u *unstructured.Unstructured) HelmReleaseEvent {
		return HelmReleaseEvent{HelmRelease: helmReleaseFromUnstructured(u)}
	})
}

// kustomizationFromUnstructured maps an unstructured Kustomization object to the
// minimal kustomization type.
func kustomizationFromUnstructured(u *unstructured.Unstructured) kustomization {
	path, _, _ := unstructured.NestedString(u.Object, "spec", "path")
	targetNS, _, _ := unstructured.NestedString(u.Object, "spec", "targetNamespace")
	srcKind, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "kind")
	srcName, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "name")
	srcNamespace, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "namespace")
	rev, _, _ := unstructured.NestedString(u.Object, "status", "lastAppliedRevision")
	namespace := u.GetNamespace()
	if srcNamespace == "" {
		srcNamespace = namespace // sourceRef.namespace defaults to the Kustomization namespace
	}
	readyStatus, readyReason, readyMessage := readyCondition(u)
	return kustomization{
		Name:            u.GetName(),
		Namespace:       namespace,
		Path:            path,
		TargetNamespace: targetNS,
		SourceKind:      srcKind,
		SourceName:      srcName,
		SourceNamespace: srcNamespace,
		Revision:        rev,
		ReadyStatus:     readyStatus,
		ReadyReason:     readyReason,
		ReadyMessage:    readyMessage,
		ReadyTime:       readyTransitionTime(u),
	}
}

// helmReleaseFromUnstructured maps an unstructured HelmRelease to the minimal
// helmRelease type the failure watcher needs (identity + Ready condition).
func helmReleaseFromUnstructured(u *unstructured.Unstructured) helmRelease {
	status, reason, message := readyCondition(u)
	return helmRelease{
		Name:         u.GetName(),
		Namespace:    u.GetNamespace(),
		ReadyStatus:  status,
		ReadyReason:  reason,
		ReadyMessage: message,
	}
}
