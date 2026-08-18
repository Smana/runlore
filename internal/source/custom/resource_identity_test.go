// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"testing"
)

// TestDecodeCanonicalisesAnARNWorkloadName pins the same ingestion rule the
// Alertmanager adapter carries, on the mapper every vendor webhook (and
// sources.grafana, which delegates here) goes through: a mapped workload_name that
// is an AWS ARN is stored as the resource identifier, so one cloud resource has one
// spelling in notifications, curated entries and the recurrence key — whichever
// dimension the vendor's alert rule happened to template in. Anything that is not an
// ARN is stored byte-for-byte.
func TestDecodeCanonicalisesAnARNWorkloadName(t *testing.T) {
	insts, err := parseConfig(mustNode(t, `
instances:
  cw:
    items: alerts
    fields:
      title: labels.alertname
      namespace: labels.namespace
      workload_name: labels.resource
      fingerprint: fingerprint
    labels: labels
`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Source{instances: insts}
	// The account travels as a mapped label, which is what lets it reach the trigger
	// key under BOTH spellings — see TestVendorWebhookKeysPerAWSAccount.
	body := `{"alerts":[
  {"fingerprint":"fp1","labels":{"alertname":"RDSHighCPU","namespace":"observability","account_id":"142655614335","resource":"arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared"}},
  {"fingerprint":"fp2","labels":{"alertname":"RDSHighCPU","namespace":"observability","account_id":"142655614335","resource":"datagrok-aqemia-shared"}},
  {"fingerprint":"fp3","labels":{"alertname":"PodNotReady","namespace":"tooling","resource":"harbor-registry-59598dbd57-ltkzw"}}
]}`
	res, err := s.Decode([]byte(body), hdr("cw"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 3 {
		t.Fatalf("want 3 requests, got %d", len(res.Requests))
	}
	arn, short, pod := res.Requests[0], res.Requests[1], res.Requests[2]
	if arn.Workload.Name != "datagrok-aqemia-shared" {
		t.Errorf("Workload.Name = %q, want the resource identifier the ARN names", arn.Workload.Name)
	}
	if arn.TriggerKey != short.TriggerKey {
		t.Errorf("the two spellings of one DB instance produced two trigger keys:\n arn:   %q\n short: %q", arn.TriggerKey, short.TriggerKey)
	}
	if pod.Workload.Name != "harbor-registry-59598dbd57-ltkzw" {
		t.Errorf("a Kubernetes pod name must reach the request untouched, got %q", pod.Workload.Name)
	}
}

// TestVendorWebhookKeysPerAWSAccount pins that the cross-account split reaches the
// mapper every vendor webhook (and sources.grafana) goes through, not just the
// Alertmanager adapter: two accounts hosting an instance of the same name agree on
// the title, the namespace, the kind and the instance slot, so without the account
// they were one recurrence bucket — and inside a cooldown one account's incident is
// dropped on the other's conclusion.
func TestVendorWebhookKeysPerAWSAccount(t *testing.T) {
	insts, err := parseConfig(mustNode(t, `
instances:
  cw:
    items: alerts
    fields:
      title: labels.alertname
      namespace: labels.namespace
      workload_name: labels.resource
      fingerprint: fingerprint
    labels: labels
`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Source{instances: insts}
	body := `{"alerts":[
  {"fingerprint":"fp1","labels":{"alertname":"RDSHighCPU","namespace":"observability","account_id":"111111111111","resource":"datagrok"}},
  {"fingerprint":"fp2","labels":{"alertname":"RDSHighCPU","namespace":"observability","account_id":"222222222222","resource":"datagrok"}},
  {"fingerprint":"fp3","labels":{"alertname":"PodNotReady","namespace":"tooling","resource":"harbor-registry"}}
]}`
	res, err := s.Decode([]byte(body), hdr("cw"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 3 {
		t.Fatalf("want 3 requests, got %d", len(res.Requests))
	}
	a, b, k8s := res.Requests[0], res.Requests[1], res.Requests[2]
	if a.Workload.Account != "111111111111" || b.Workload.Account != "222222222222" {
		t.Errorf("mapped account labels did not reach the workload: %q / %q", a.Workload.Account, b.Workload.Account)
	}
	if a.TriggerKey == b.TriggerKey {
		t.Errorf("two AWS accounts hosting an instance named \"datagrok\" share one trigger key %q", a.TriggerKey)
	}
	// A workload with no cloud scope keeps the five-segment key it always had.
	if k8s.Workload.Account != "" {
		t.Errorf("an unqualified workload acquired an account %q", k8s.Workload.Account)
	}
	if want := "podnotready|tooling||harbor-registry|cw"; k8s.TriggerKey != want {
		t.Errorf("trigger key changed for an unqualified workload:\n got  %q\n want %q", k8s.TriggerKey, want)
	}
}
