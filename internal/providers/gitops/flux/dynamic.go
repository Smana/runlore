package flux

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// Flux CRD resources (v1).
var (
	kustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
)

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

// WatchKustomizations watches all Kustomizations and forwards each add/modify as
// a KustomizationEvent. The channel closes when the underlying watch stops or ctx
// is done.
func (r *dynamicReader) WatchKustomizations(ctx context.Context) (<-chan KustomizationEvent, error) {
	w, err := r.client.Resource(kustomizationGVR).Namespace(metav1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("watch kustomizations: %w", err)
	}
	out := make(chan KustomizationEvent)
	go func() {
		defer close(out)
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.ResultChan():
				if !ok {
					return
				}
				if e.Type != watch.Added && e.Type != watch.Modified {
					continue
				}
				u, ok := e.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				select {
				case out <- KustomizationEvent{Kustomization: kustomizationFromUnstructured(u)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// kustomizationFromUnstructured maps an unstructured Kustomization object to the
// minimal kustomization type.
func kustomizationFromUnstructured(u *unstructured.Unstructured) kustomization {
	path, _, _ := unstructured.NestedString(u.Object, "spec", "path")
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
		SourceName:      srcName,
		SourceNamespace: srcNamespace,
		Revision:        rev,
		ReadyStatus:     readyStatus,
		ReadyReason:     readyReason,
		ReadyMessage:    readyMessage,
	}
}
