// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// policyRule is the slice of a Kubernetes RBAC rule this guard checks.
type policyRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// readResourceSpecRules decodes rbac.resourceSpecRules out of the chart's values.yaml —
// the real parse target Helm itself reads, not a copy of it.
func readResourceSpecRules(t *testing.T) []policyRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var v struct {
		RBAC struct {
			ResourceSpecRules []policyRule `yaml:"resourceSpecRules"`
		} `yaml:"rbac"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("values.yaml is not valid YAML: %v", err)
	}
	if len(v.RBAC.ResourceSpecRules) == 0 {
		t.Fatal("rbac.resourceSpecRules is empty: resource_spec is non-functional on a stock " +
			"install — every kind it was built for comes back forbidden")
	}
	return v.RBAC.ResourceSpecRules
}

// TestResourceSpecRBACCoversTheMotivatingKinds: the tool resolves kinds through discovery,
// so it handles anything — but only what RBAC grants. The ClusterRole covered Flux, Argo CD,
// events and pods, which meant every example in the issue this tool closes (a Service
// selector matching nothing, a NetworkPolicy with no egress to kube-dns, an HPA on a metric
// nothing reports, a PVC bound to a vanished storage class, and the VMServiceScrape the
// issue is actually about) answered "forbidden" on a stock install.
//
// That is a security problem before it is a functional one: the obvious operator response
// to a wall of denials is `resources: ["*"]`, which includes secrets.
func TestResourceSpecRBACCoversTheMotivatingKinds(t *testing.T) {
	rules := readResourceSpecRules(t)
	granted := map[string]bool{}
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				granted[g+"/"+res] = true
			}
		}
	}
	for _, want := range []string{
		"/services",                                     // a selector that matches no pods
		"networking.k8s.io/networkpolicies",             // no egress rule to kube-dns
		"autoscaling/horizontalpodautoscalers",          // a metric that never reports
		"/persistentvolumeclaims",                       // a storage class that no longer exists
		"operator.victoriametrics.com/vmservicescrapes", // the CR the issue is about
		"apps/deployments",                              // the workload behind most incidents
	} {
		if !granted[want] {
			t.Errorf("resource_spec cannot read %q: it is not in rbac.resourceSpecRules", want)
		}
	}
}

// TestResourceSpecRBACNeverGrantsSecrets is the boundary itself.
//
// The tool refuses the Secret kind before AND after resolution, but that is an
// application-layer policy: RBAC is what actually stops the ServiceAccount's token from
// reading every Secret in the cluster. A wildcard here would quietly undo it, and the
// refusal — which today makes a stock Secret read an existence oracle rather than a dump —
// would be the only thing left standing.
func TestResourceSpecRBACNeverGrantsSecrets(t *testing.T) {
	for _, r := range readResourceSpecRules(t) {
		for _, res := range r.Resources {
			if res == "secrets" || res == "*" {
				t.Errorf("rbac.resourceSpecRules grants %q on apiGroups %v — a wildcard includes "+
					"secrets, and RBAC is the boundary the Secret refusal is NOT", res, r.APIGroups)
			}
		}
		// Read-only, cluster-wide: this list is bound to the ClusterRole, so a write verb
		// here is a cluster-wide write grant. Execution rights are namespace-scoped Roles
		// gated on rbac.allowActions, deliberately and separately.
		for _, v := range r.Verbs {
			if !slices.Contains([]string{"get", "list", "watch"}, v) {
				t.Errorf("rbac.resourceSpecRules grants the write verb %q on %v", v, r.Resources)
			}
		}
	}
}

// TestResourceSpecRBACIsWiredAndWarned: the list is inert unless the template renders it,
// and the wildcard warning is the only thing standing between an operator staring at a
// denial and `resources: ["*"]`. Both are one careless edit from disappearing.
func TestResourceSpecRBACIsWiredAndWarned(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join(chartDir, "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	if !strings.Contains(string(tpl), ".Values.rbac.resourceSpecRules") {
		t.Error("the ClusterRole does not render rbac.resourceSpecRules: the values key is inert")
	}
	values, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	for _, want := range []string{`resources: ["*"]`, "secrets"} {
		if !strings.Contains(string(values), want) {
			t.Errorf("values.yaml no longer warns about %q next to resourceSpecRules", want)
		}
	}
}
