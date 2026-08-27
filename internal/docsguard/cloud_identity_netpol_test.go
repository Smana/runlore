// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const netpolPath = "../../deploy/helm/runlore/templates/networkpolicy.yaml"

// ciliumEgressRule is one entry of a CiliumNetworkPolicy's egress[].
type ciliumEgressRule struct {
	ToEntities []string `yaml:"toEntities"`
	ToCIDR     []string `yaml:"toCIDR"`
}

type ciliumPolicyDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Egress []ciliumEgressRule `yaml:"egress"`
	} `yaml:"spec"`
}

// ciliumPolicies parses every CiliumNetworkPolicy document out of the chart template,
// keyed by the literal suffix of its name.
//
// The template is read and parsed rather than rendered with `helm template`, matching
// gitops_rbac_test.go: the gate runs on a Go toolchain with no helm binary. Only the
// CiliumNetworkPolicy documents are parsed — the NetworkPolicy document above them is
// dense with range/toYaml actions whose blanked remains are not worth reasoning about,
// and nothing here asserts anything about it.
func ciliumPolicies(t *testing.T) map[string]ciliumPolicyDoc {
	t.Helper()
	b, err := os.ReadFile(netpolPath)
	if err != nil {
		t.Fatalf("read %s: %v", netpolPath, err)
	}
	out := map[string]ciliumPolicyDoc{}
	for _, raw := range strings.Split(string(b), "\n---\n") {
		if !strings.Contains(raw, "kind: CiliumNetworkPolicy") {
			continue
		}
		var doc ciliumPolicyDoc
		if err := yaml.Unmarshal([]byte(helmAction.ReplaceAllString(raw, "")), &doc); err != nil {
			t.Fatalf("parse CiliumNetworkPolicy document: %v\n%s", err, raw)
		}
		out[strings.TrimPrefix(doc.Metadata.Name, "-")] = doc
	}
	return out
}

// TestCloudIdentityPoliciesAreNotInterchangeable pins the fix for #562, and exists
// because the bug it pins was introduced by a refactor that looked obviously correct.
//
// Both clouds' credential endpoints are link-local addresses served on :80 by the node,
// so the two policies read as duplicates and were merged into one `toEntities: [host]`
// rule. They are not duplicates. Cilium classifies 169.254.170.23 (the EKS Pod Identity
// agent, which genuinely runs on the node host network) as the host entity, and does NOT
// classify GKE's 169.254.169.254 that way — so the merged rule matched nothing on GKE and
// every metadata call was dropped.
//
// That failure is invisible from the chart and nearly invisible from the logs: RunLore
// reports "project is required (set cloud.gcp.project)", which reads as a config problem,
// so the operator hardcodes the scope, the symptom disappears, and ADC still cannot mint
// a token. Nothing in the chain mentions egress. A unit test is the only cheap place this
// is ever going to be caught — the expensive place is a live GKE cluster, which is where
// it was caught the first time.
func TestCloudIdentityPoliciesAreNotInterchangeable(t *testing.T) {
	policies := ciliumPolicies(t)

	if len(policies) != 2 {
		t.Fatalf("expected two separate cloud-identity policies, got %d: %v\n"+
			"One policy for both clouds is exactly the #562 regression: the two endpoints "+
			"look interchangeable and are not.", len(policies), keysOf(policies))
	}

	gcp, ok := policies["gcp-workload-identity"]
	if !ok {
		t.Fatalf("no gcp-workload-identity policy; have %v", keysOf(policies))
	}
	aws, ok := policies["aws-pod-identity"]
	if !ok {
		t.Fatalf("no aws-pod-identity policy; have %v", keysOf(policies))
	}

	// GCP: the metadata server must be allowed by ADDRESS. toEntities:[host] does not
	// match it, which is the whole defect.
	var gcpCIDRs []string
	for _, r := range gcp.Spec.Egress {
		gcpCIDRs = append(gcpCIDRs, r.ToCIDR...)
		if len(r.ToEntities) > 0 {
			t.Errorf("the GKE policy allows egress by entity %v. Cilium does not classify "+
				"169.254.169.254 as any node entity, so this matches nothing and the token "+
				"fetch is silently dropped (#562)", r.ToEntities)
		}
	}
	if !contains(gcpCIDRs, "169.254.169.254/32") {
		t.Errorf("the GKE policy does not allow 169.254.169.254/32; ADC cannot reach the "+
			"metadata server and autodetection reports a misleading 'project is required'. "+
			"Allowed: %v", gcpCIDRs)
	}

	// AWS: toEntities:[host] is CORRECT here and must not be "fixed" to match GCP. The
	// Pod Identity agent really does run on the node's host network.
	var awsEntities []string
	for _, r := range aws.Spec.Egress {
		awsEntities = append(awsEntities, r.ToEntities...)
	}
	if !contains(awsEntities, "host") {
		t.Errorf("the EKS policy no longer allows the host entity (%v). 169.254.170.23 IS on "+
			"the node host network, which no ipBlock or toCIDR rule can match — this is the "+
			"one case where toEntities is the right answer", awsEntities)
	}
}

func keysOf(m map[string]ciliumPolicyDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
