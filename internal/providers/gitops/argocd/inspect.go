package argocd

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/runlore/internal/providers"
)

// ResourceStatus reports an Argo CD Application's health + sync status, key
// source/destination refs, error conditions, and recent Events — the Argo analogue of
// the Flux inspector's "why is it failing" lens. A missing Application is reported via
// NotFound (often the cascade root), not an error.
func (p *Provider) ResourceStatus(ctx context.Context, w providers.Workload) (providers.ResourceStatus, error) {
	rs := providers.ResourceStatus{Workload: w, Refs: map[string]string{}}
	u, err := p.reader.GetApplication(ctx, w.Namespace, w.Name)
	if apierrors.IsNotFound(err) {
		rs.NotFound = true
		return rs, nil
	}
	if err != nil {
		return rs, err
	}
	rs.Ready, rs.Reason, rs.Message = appReady(u)
	// sourceRepoPath handles both single-source (spec.source) and multi-source
	// (spec.sources[0]) schemas, mirroring the Changes() path in dynamic.go.
	if repo, path := sourceRepoPath(u); repo != "" {
		rs.Refs["repoURL"] = repo
		if path != "" {
			rs.Refs["path"] = path
		}
		tr, _, _ := unstructured.NestedString(u.Object, "spec", "source", "targetRevision")
		if tr == "" {
			if first, ok := firstSourceMap(u, "spec", "sources"); ok {
				tr, _ = first["targetRevision"].(string)
			}
		}
		if tr != "" {
			rs.Refs["targetRevision"] = tr
		}
	}
	if dns, _, _ := unstructured.NestedString(u.Object, "spec", "destination", "namespace"); dns != "" {
		rs.Refs["destinationNamespace"] = dns
	}
	if sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status"); sync != "" {
		rs.Refs["sync"] = sync
	}
	// Use the resolved object's namespace/name (GetApplication may have found it in
	// another namespace than the workload selector's).
	rs.Events, _ = p.reader.ListEvents(ctx, u.GetNamespace(), u.GetName(), "Application") // best-effort
	return rs, nil
}

// DependencyTree returns the Application with its FAILING managed resources
// (status.resources whose health is not Healthy/Progressing) as children, recursing
// into child Applications (app-of-apps) — so the root failure behind a degraded app is
// visible. Best-effort: child read errors don't abort the walk.
func (p *Provider) DependencyTree(ctx context.Context, w providers.Workload) (providers.DepNode, error) {
	return p.appNode(ctx, w, map[string]bool{}), nil
}

func (p *Provider) appNode(ctx context.Context, w providers.Workload, seen map[string]bool) providers.DepNode {
	node := providers.DepNode{Workload: w}
	key := w.Namespace + "/" + w.Name
	if seen[key] {
		return node // cycle guard (app-of-apps loops)
	}
	seen[key] = true
	u, err := p.reader.GetApplication(ctx, w.Namespace, w.Name)
	if apierrors.IsNotFound(err) {
		node.NotFound = true
		return node
	}
	if err != nil {
		return node
	}
	node.Ready, node.Reason, _ = appReady(u)
	for _, r := range managedResources(u) {
		if r.Kind == "Application" && r.Name != "" { // app-of-apps: recurse, surface if not healthy
			child := p.appNode(ctx, providers.Workload{Kind: "Application", Name: r.Name, Namespace: r.Namespace}, seen)
			if child.Ready != "True" || child.NotFound || len(child.Children) > 0 {
				node.Children = append(node.Children, child)
			}
			continue
		}
		// Surface only failing managed resources to point at the root failure; Healthy
		// and Progressing (transient) ones are noise. An unassessed resource (no
		// health) is left out.
		if r.Health != "" && r.Health != "Healthy" && r.Health != "Progressing" {
			node.Children = append(node.Children, providers.DepNode{
				Workload: providers.Workload{Kind: r.Kind, Name: r.Name, Namespace: r.Namespace},
				Ready:    healthToReady(r.Health),
				Reason:   r.Health,
			})
		}
	}
	return node
}

// appReady maps Argo health + sync into the (Ready, Reason, Message) shape the
// inspector contract expects. Ready is "True"/"False"/"Unknown"/"" — Healthy→True,
// Degraded/Missing→False, a failed sync forces False, others Unknown.
func appReady(u *unstructured.Unstructured) (ready, reason, message string) {
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	phase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	opMsg, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message")
	ready = healthToReady(health)
	reason = health
	if phase == "Failed" || phase == "Error" {
		reason = strings.TrimSpace(reason + " Sync" + phase)
		if ready != "False" {
			ready = "False" // a failed sync is not-ready even if health hasn't flipped yet
		}
	}
	msgs := conditionMessages(u)
	if opMsg != "" {
		msgs = append([]string{opMsg}, msgs...)
	}
	return ready, reason, strings.Join(msgs, "; ")
}

func healthToReady(h string) string {
	switch h {
	case "Healthy":
		return "True"
	case "Degraded", "Missing":
		return "False"
	case "":
		return ""
	default:
		return "Unknown" // Progressing, Suspended, Unknown
	}
}

// conditionMessages surfaces Argo error/warning conditions (ComparisonError,
// SyncError, InvalidSpecError, *Warning) as "Type: message".
func conditionMessages(u *unstructured.Unstructured) []string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var out []string
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		msg, _ := m["message"].(string)
		if msg == "" {
			continue
		}
		lt := strings.ToLower(typ)
		if strings.Contains(lt, "error") || strings.Contains(lt, "warning") {
			out = append(out, typ+": "+msg)
		}
	}
	return out
}

type managedResource struct{ Kind, Name, Namespace, Health string }

// managedResources parses status.resources[] — the Application's managed objects with
// their per-resource health.
func managedResources(u *unstructured.Unstructured) []managedResource {
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "resources")
	out := make([]managedResource, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		ns, _ := m["namespace"].(string)
		health := ""
		if h, ok := m["health"].(map[string]any); ok {
			health, _ = h["status"].(string)
		}
		out = append(out, managedResource{Kind: kind, Name: name, Namespace: ns, Health: health})
	}
	return out
}
