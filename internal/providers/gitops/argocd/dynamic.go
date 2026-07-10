// SPDX-License-Identifier: Apache-2.0

package argocd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// applicationGVR is the Argo CD Application resource.
var applicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// eventsGVR is the core v1 Events resource (for ListEvents).
var eventsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}

// sendTimeout bounds how long the informer handler blocks waiting for the
// consumer to drain a watch event before giving up. It is large enough to ride
// out a transient consumer stall (so we don't silently drop a failure event on a
// burst) yet finite, so a wedged consumer can never block the shared informer
// indefinitely.
const sendTimeout = 5 * time.Second

// dynamicReader reads Argo CD Applications as unstructured objects.
type dynamicReader struct {
	client dynamic.Interface
}

// NewDynamicReader builds a Reader backed by a client-go dynamic client.
func NewDynamicReader(client dynamic.Interface) Reader {
	return &dynamicReader{client: client}
}

// ListApplications lists all Argo CD Applications across all namespaces.
func (r *dynamicReader) ListApplications(ctx context.Context) ([]application, error) {
	list, err := r.client.Resource(applicationGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	out := make([]application, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, applicationFromUnstructured(&list.Items[i]))
	}
	return out, nil
}

// GetApplication fetches one Application as unstructured. Argo Applications usually
// live in the argocd namespace, not the workload's; if the caller passes the
// workload's namespace and the app isn't there, fall back to a name search across all
// namespaces before trusting the NotFound.
func (r *dynamicReader) GetApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	u, err := r.client.Resource(applicationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return u, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if list, lerr := r.client.Resource(applicationGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); lerr == nil {
		for i := range list.Items {
			if list.Items[i].GetName() == name {
				return &list.Items[i], nil
			}
		}
	}
	return nil, err // genuinely absent in every namespace: return the original NotFound
}

// ListEvents returns recent Event lines for an involved object, filtered by name
// (server-side) + kind (client-side), rendered as "Type Reason Message".
func (r *dynamicReader) ListEvents(ctx context.Context, namespace, name, kind string) ([]string, error) {
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
		if n, _, _ := unstructured.NestedString(o, "involvedObject", "name"); n != name {
			continue
		}
		if kind != "" {
			if k, _, _ := unstructured.NestedString(o, "involvedObject", "kind"); k != "" && k != kind {
				continue
			}
		}
		typ, _, _ := unstructured.NestedString(o, "type")
		reason, _, _ := unstructured.NestedString(o, "reason")
		msg, _, _ := unstructured.NestedString(o, "message")
		out = append(out, fmt.Sprintf("%s %s %s", typ, reason, msg))
	}
	return out, nil
}

// WatchApplications watches all Applications via a dynamic informer (list-watch
// with reconnection + periodic resync) and forwards each add/update. The channel
// closes when ctx is done.
func (r *dynamicReader) WatchApplications(ctx context.Context) (<-chan ApplicationEvent, error) {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(r.client, 10*time.Minute)
	informer := factory.ForResource(applicationGVR).Informer()

	out := make(chan ApplicationEvent, 128)
	send := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		ev := ApplicationEvent{Application: applicationFromUnstructured(u)}
		sendEvent(ctx, out, ev)
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

// sendEvent forwards a watch event to out with a BOUNDED block. A full channel no
// longer means an instant silent drop (the old `default:` branch): we wait up to
// sendTimeout for the consumer to drain, so a transient burst doesn't lose a
// failure event. If the consumer is wedged past the timeout we give up and log —
// the shared informer must never be blocked indefinitely. ctx cancellation returns
// immediately.
func sendEvent(ctx context.Context, out chan<- ApplicationEvent, ev ApplicationEvent) {
	timer := time.NewTimer(sendTimeout)
	defer timer.Stop()
	select {
	case out <- ev:
	case <-ctx.Done():
	case <-timer.C:
		slog.Warn("argocd: dropped watch event (consumer backpressure)",
			"application", ev.Application.Namespace+"/"+ev.Application.Name,
			"timeout", sendTimeout)
	}
}

// applicationFromUnstructured maps an unstructured Application to the minimal type.
// It supports both the single-source schema (spec.source / status.sync.revision)
// and the multi-source schema (spec.sources[] / status.sync.revisions[]); for a
// multi-source app the FIRST source + first revision are used (the Git source that
// backs the manifests), so the app contributes to the change spine instead of
// being silently dropped.
func applicationFromUnstructured(u *unstructured.Unstructured) application {
	repoURL, path := sourceRepoPath(u)
	rev := syncRevision(u)
	syncStatus, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	phase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	msg, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message")
	return application{
		Name:           u.GetName(),
		Namespace:      u.GetNamespace(),
		RepoURL:        repoURL,
		Path:           path,
		Revision:       rev,
		PrevRevision:   prevRevision(u),
		HealthStatus:   health,
		SyncStatus:     syncStatus,
		OperationPhase: phase,
		Message:        msg,
	}
}

// sourceRepoPath returns the repoURL + path of the Application's source. It reads
// the singular spec.source first, then falls back to the FIRST entry of the
// multi-source spec.sources[].
func sourceRepoPath(u *unstructured.Unstructured) (repoURL, path string) {
	repoURL, _, _ = unstructured.NestedString(u.Object, "spec", "source", "repoURL")
	if repoURL != "" {
		path, _, _ = unstructured.NestedString(u.Object, "spec", "source", "path")
		return repoURL, path
	}
	if first, ok := firstSourceMap(u, "spec", "sources"); ok {
		repoURL, _ = first["repoURL"].(string)
		path, _ = first["path"].(string)
	}
	return repoURL, path
}

// syncRevision returns the synced revision: status.sync.revision (single-source)
// or the first of status.sync.revisions[] (multi-source).
func syncRevision(u *unstructured.Unstructured) string {
	if rev, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision"); rev != "" {
		return rev
	}
	return firstRevision(u, "status", "sync", "revisions")
}

// prevRevision returns the revision before the latest in status.history (the diff
// range start), or empty if there is no prior deployment. Handles both the
// singular .revision and the multi-source plural .revisions[] (first element).
func prevRevision(u *unstructured.Unstructured) string {
	hist, found, _ := unstructured.NestedSlice(u.Object, "status", "history")
	if !found || len(hist) < 2 {
		return ""
	}
	m, ok := hist[len(hist)-2].(map[string]any)
	if !ok {
		return ""
	}
	if rev, ok := m["revision"].(string); ok && rev != "" {
		return rev
	}
	return firstString(m["revisions"])
}

// firstSourceMap returns the first element of an object-array field (e.g.
// spec.sources) as a map, or ok=false when absent/empty.
func firstSourceMap(u *unstructured.Unstructured, fields ...string) (map[string]any, bool) {
	s, found, _ := unstructured.NestedSlice(u.Object, fields...)
	if !found || len(s) == 0 {
		return nil, false
	}
	m, ok := s[0].(map[string]any)
	return m, ok
}

// firstRevision returns the first element of a string-array field (e.g.
// status.sync.revisions), or "".
func firstRevision(u *unstructured.Unstructured, fields ...string) string {
	s, found, _ := unstructured.NestedSlice(u.Object, fields...)
	if !found {
		return ""
	}
	return firstString(s)
}

// firstString returns the first string element of a []any, or "".
func firstString(v any) string {
	s, ok := v.([]any)
	if !ok || len(s) == 0 {
		return ""
	}
	str, _ := s[0].(string)
	return str
}
