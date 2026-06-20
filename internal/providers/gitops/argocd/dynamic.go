package argocd

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// applicationGVR is the Argo CD Application resource.
var applicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

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
		select {
		case out <- ev:
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

// applicationFromUnstructured maps an unstructured Application to the minimal type.
func applicationFromUnstructured(u *unstructured.Unstructured) application {
	repoURL, _, _ := unstructured.NestedString(u.Object, "spec", "source", "repoURL")
	path, _, _ := unstructured.NestedString(u.Object, "spec", "source", "path")
	rev, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision")
	syncStatus, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	msg, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message")
	return application{
		Name:         u.GetName(),
		Namespace:    u.GetNamespace(),
		RepoURL:      repoURL,
		Path:         path,
		Revision:     rev,
		PrevRevision: prevRevision(u),
		HealthStatus: health,
		SyncStatus:   syncStatus,
		Message:      msg,
	}
}

// prevRevision returns the revision before the latest in status.history (the diff
// range start), or empty if there is no prior deployment.
func prevRevision(u *unstructured.Unstructured) string {
	hist, found, _ := unstructured.NestedSlice(u.Object, "status", "history")
	if !found || len(hist) < 2 {
		return ""
	}
	m, ok := hist[len(hist)-2].(map[string]any)
	if !ok {
		return ""
	}
	rev, _ := m["revision"].(string)
	return rev
}
