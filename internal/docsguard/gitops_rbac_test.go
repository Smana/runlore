// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
)

const gitopsRBACPath = "../../deploy/helm/runlore/templates/rbac.yaml"

// helmAction matches a Helm template action so the chart can be YAML-parsed. Every
// action in rbac.yaml sits in metadata (names, labels, subjects) or wraps a whole
// document; none of them produce a rule, so blanking them leaves the rules intact.
var helmAction = regexp.MustCompile(`{{-?.*?-?}}`)

// clusterRoleRule is one entry of a ClusterRole's rules[].
type clusterRoleRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

type clusterRoleDoc struct {
	Kind  string            `yaml:"kind"`
	Rules []clusterRoleRule `yaml:"rules"`
}

// TestGitOpsInspectionKindsAreRBACGranted pins the chart's cluster-wide read grant to
// the kinds the GitOps providers actually resolve.
//
// gitops_resource_status and gitops_tree advertise exactly flux.GitOpsKinds() /
// argocd.GitOpsKinds() (see investigate.GitOpsKinds), so a kind added to a provider
// without the matching RBAC rule is advertised to the model, called, and answered with
// a Forbidden — and a denial the tools cannot distinguish from an absence is precisely
// the #503 failure mode. The reverse drift is cheaper but still worth naming: a grant
// for a resource no provider resolves is unused privilege in a ClusterRole.
//
// This reflects over the providers' own maps, so it fails the moment kindToGVR and the
// chart disagree in either direction.
func TestGitOpsInspectionKindsAreRBACGranted(t *testing.T) {
	raw, err := os.ReadFile(gitopsRBACPath)
	if err != nil {
		t.Fatalf("read %s: %v", gitopsRBACPath, err)
	}
	granted := clusterWideReads(t, string(raw))

	want := append(flux.GitOpsResources(), argocd.GitOpsResources()...)
	for _, res := range want {
		if !granted[res] {
			t.Errorf("%s grants no cluster-wide get/list/watch on %q, yet a GitOps provider "+
				"resolves a kind backed by it — the tool would advertise that kind and be denied",
				gitopsRBACPath, res)
		}
	}
	// The reverse: nothing in the GitOps CRD groups may be granted that no provider
	// resolves. Core resources (pods, events) and the Argo CD extras the chart documents
	// as investigation context are excluded by name, so this stays a GitOps-kind guard
	// rather than a whole-ClusterRole freeze.
	notAKind := []string{"applicationsets", "appprojects"}
	for res := range granted {
		if slices.Contains(want, res) || slices.Contains(notAKind, res) || !gitopsCRDResource(res) {
			continue
		}
		t.Errorf("%s grants %q in a GitOps CRD group, but no provider resolves a kind backed "+
			"by it — either wire the kind or drop the grant", gitopsRBACPath, res)
	}
}

// gitopsCRDResource reports whether a granted resource belongs to the Flux/Argo CD CRD
// surface this guard owns, as opposed to the core Kubernetes reads (pods, events) the
// same ClusterRole carries for pod_status/kube_events.
func gitopsCRDResource(res string) bool {
	return !slices.Contains([]string{"pods", "events"}, res)
}

// clusterWideReads returns the resources the chart's ClusterRole grants get+list+watch
// on. Namespaced Roles in the same file are ignored: the GitOps inspector reads any
// namespace, so only a ClusterRole rule actually authorises it.
func clusterWideReads(t *testing.T, chart string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, doc := range strings.Split(helmAction.ReplaceAllString(chart, ""), "\n---\n") {
		var cr clusterRoleDoc
		if err := yaml.Unmarshal([]byte(doc), &cr); err != nil {
			t.Fatalf("parse a document of %s as YAML: %v", gitopsRBACPath, err)
		}
		if cr.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range cr.Rules {
			if !slices.Contains(rule.Verbs, "get") || !slices.Contains(rule.Verbs, "list") {
				continue
			}
			for _, res := range rule.Resources {
				out[res] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no ClusterRole read rules found in %s — the guard is not reading the chart", gitopsRBACPath)
	}
	return out
}
