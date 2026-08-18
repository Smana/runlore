// SPDX-License-Identifier: Apache-2.0

package alertmanager

import (
	"encoding/json"
	"testing"
)

// TestDecodeCanonicalisesAnARNWorkloadLabel is the ingestion half of the
// 2026-08-17/18 split. A CloudWatch-derived alert rule templates the affected
// resource into the `workload` label, and which dimension it reaches for decides the
// spelling: the RDS instance datagrok-aqemia-shared arrived as its bare
// DBInstanceIdentifier on the 21:18Z firing and as its full ARN on the 03:53Z one.
//
// Ingestion is where the split has to stop for NEW data: Workload.Name is what a
// notification renders, what a curated entry's `resource:` frontmatter is indexed
// under, and what the recurrence key is built from. Storing the identifier means a
// resource only ever has one spelling downstream. (Entries already written in the
// ARN form are the comparison side's job — see investigate.refsAgree.)
func TestDecodeCanonicalisesAnARNWorkloadLabel(t *testing.T) {
	const (
		short = "datagrok-aqemia-shared"
		arn   = "arn:aws:rds:us-east-1:142655614335:db:" + short
	)
	decode := func(t *testing.T, workload string) (name, triggerKey string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"alerts": []map[string]any{{
			"status": "firing",
			"labels": map[string]string{
				"alertname": "RDSHighCPU", "namespace": "observability",
				"workload": workload, "cluster": "aqemia-shared", "severity": "warning",
			},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		res, err := (&Source{}).Decode(body, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Requests) != 1 {
			t.Fatalf("want 1 request, got %d", len(res.Requests))
		}
		return res.Requests[0].Workload.Name, res.Requests[0].TriggerKey
	}

	arnName, arnKey := decode(t, arn)
	shortName, shortKey := decode(t, short)

	if arnName != short {
		t.Errorf("Workload.Name = %q, want the resource identifier %q — the ARN scaffolding names nothing extra", arnName, short)
	}
	if shortName != short {
		t.Errorf("a short workload label must pass through untouched: got %q, want %q", shortName, short)
	}
	if arnKey != shortKey {
		t.Errorf("the two firings of one DB instance produced two trigger keys:\n arn:   %q\n short: %q", arnKey, shortKey)
	}
	// The full ARN is not lost — it still travels verbatim on the labels, so the
	// account and region reach the seed prompt.
	body, err := json.Marshal(map[string]any{"alerts": []map[string]any{{
		"status": "firing",
		"labels": map[string]string{"alertname": "RDSHighCPU", "namespace": "observability", "workload": arn},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Requests[0].Labels["workload"]; got != arn {
		t.Errorf("the raw ARN must survive on Labels for the seed prompt: got %q, want %q", got, arn)
	}
}

// TestWorkloadFromLabelsLeavesKubernetesNamesAlone guards the conservative half of
// the rule: only an ARN is rewritten. A pod name keeps its template hash (it is
// normalized at comparison time, never at ingestion), and a name that merely
// contains "arn" is not an ARN.
func TestWorkloadFromLabelsLeavesKubernetesNamesAlone(t *testing.T) {
	cases := []struct {
		labels     map[string]string
		kind, name string
	}{
		{map[string]string{"pod": "harbor-registry-59598dbd57-ltkzw"}, "Pod", "harbor-registry-59598dbd57-ltkzw"},
		{map[string]string{"deployment": "arnica-exporter"}, "Deployment", "arnica-exporter"},
		{map[string]string{"statefulset": "arn:aws:rds"}, "StatefulSet", "arn:aws:rds"}, // truncated: not an ARN
		{map[string]string{"alertname": "X"}, "", ""},
	}
	for _, c := range cases {
		kind, name := workloadFromLabels(c.labels)
		if kind != c.kind || name != c.name {
			t.Errorf("workloadFromLabels(%v) = (%q, %q), want (%q, %q)", c.labels, kind, name, c.kind, c.name)
		}
	}
}
