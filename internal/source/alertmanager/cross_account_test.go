// SPDX-License-Identifier: Apache-2.0

package alertmanager

import (
	"encoding/json"
	"testing"
)

// decodeOne decodes a single firing alert with the given labels.
func decodeOne(t *testing.T, labels map[string]string) (name, account, region, triggerKey string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"alerts": []map[string]any{{
		"status": "firing", "labels": labels,
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
	r := res.Requests[0]
	return r.Workload.Name, r.Workload.Account, r.Workload.Region, r.TriggerKey
}

// cwLabels builds the labels a yace/CloudWatch-derived alert rule carries for one
// RDS instance in one account, with the workload spelled as `workload`.
func cwLabels(workload, account string) map[string]string {
	return map[string]string{
		"alertname": "RDSHighCPU", "namespace": "observability",
		"workload": workload, "cluster": "aqemia-shared", "severity": "warning",
		"account_id": account, "region": "us-east-1",
	}
}

// TestTwoAWSAccountsDoNotShareATriggerKey is the collision this change exists to
// close. Two AWS accounts scraped by ONE yace/CloudWatch exporter through ONE
// Prometheus produce alerts that agree on every field the trigger key was built
// from: the alertname is the rule's, the `namespace` is the EXPORTER's (a literal
// in a CloudWatch-derived rule, not the resource's), the kind is empty for a cloud
// resource, and the `cluster` external label is stamped once per Prometheus across
// every account it watches. So `datagrok` in account 111111111111 and `datagrok` in
// account 222222222222 keyed identically.
//
// That key is the recurrence ledger's grouping key: inside a configured
// RecurrenceGate cooldown, the second account's firing is suppressed on the FIRST
// account's conclusion and investigate's loop returns with no model call, no
// notification and no ledger entry. A genuine production incident in one account is
// silently dropped because an unrelated account had the same instance name.
func TestTwoAWSAccountsDoNotShareATriggerKey(t *testing.T) {
	const instance = "datagrok"
	_, _, _, keyA := decodeOne(t, cwLabels(instance, "111111111111"))
	_, _, _, keyB := decodeOne(t, cwLabels(instance, "222222222222"))
	if keyA == "" {
		t.Fatal("no trigger key for a well-formed cloud alert")
	}
	if keyA == keyB {
		t.Fatalf("two AWS accounts hosting an instance named %q share one trigger key %q — "+
			"inside a recurrence cooldown one account's incident is silently dropped on the other's conclusion", instance, keyA)
	}
}

// TestARNAndShortSpellingsStillFuse is the property the branch exists for and must
// not regress: one instance reached RunLore by its DBInstanceIdentifier dimension on
// one firing and by its full ARN on the next. Both spellings carry the SAME alert
// labels, so the account reaches the key either way and the two firings stay one
// incident.
func TestARNAndShortSpellingsStillFuse(t *testing.T) {
	const (
		short   = "datagrok"
		account = "111111111111"
		arn     = "arn:aws:rds:us-east-1:" + account + ":db:" + short
	)
	arnName, arnAcct, arnRegion, arnKey := decodeOne(t, cwLabels(arn, account))
	shortName, shortAcct, shortRegion, shortKey := decodeOne(t, cwLabels(short, account))

	if arnName != short || shortName != short {
		t.Errorf("both spellings must store the bare identifier %q: arn gave %q, short gave %q", short, arnName, shortName)
	}
	if arnAcct != account || shortAcct != account {
		t.Errorf("both spellings must carry the account %q: arn gave %q, short gave %q", account, arnAcct, shortAcct)
	}
	if arnRegion != "us-east-1" || shortRegion != "us-east-1" {
		t.Errorf("both spellings must carry the region: arn gave %q, short gave %q", arnRegion, shortRegion)
	}
	if arnKey != shortKey {
		t.Errorf("the two spellings of one instance produced two trigger keys:\n arn:   %q\n short: %q", arnKey, shortKey)
	}
}

// TestARNWithoutAnAccountLabelStillCarriesItsAccount pins the reconciliation: the
// account reaches the Workload from whichever side has it. An ARN names its account
// even when the rule drops the label, and a short name has only the label.
func TestARNWithoutAnAccountLabelStillCarriesItsAccount(t *testing.T) {
	_, account, region, _ := decodeOne(t, map[string]string{
		"alertname": "RDSHighCPU", "namespace": "observability",
		"workload": "arn:aws:rds:eu-west-1:333333333333:db:compute-stages",
	})
	if account != "333333333333" || region != "eu-west-1" {
		t.Fatalf("qualifiers lost when only the ARN carries them: account=%q region=%q", account, region)
	}
}

// TestKubernetesWorkloadsAreUnqualifiedAndKeyByteIdentically is the regression this
// change is most likely to cause. A Kubernetes object's identity is namespace/name
// and nothing else — it has no AWS account — so a cluster whose Prometheus happens
// to stamp `account_id`/`region` as external labels must not acquire a qualifier,
// must not grow a key segment, and must produce the exact key bytes it produced
// before. The expected value is written out literally rather than compared against a
// second call, so a change to the key SHAPE cannot pass by moving both sides.
func TestKubernetesWorkloadsAreUnqualifiedAndKeyByteIdentically(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"deployment with AWS external labels", map[string]string{
			"alertname": "KubeDeploymentReplicasMismatch", "namespace": "apps",
			"deployment": "payment-api", "cluster": "aqemia-shared",
			"account_id": "111111111111", "region": "us-east-1",
		}, "kubedeploymentreplicasmismatch|apps|deployment|payment-api|aqemia-shared"},
		{"pod-scoped alert keeps its hash at ingestion and normalizes in the key", map[string]string{
			"alertname": "KubePodNotReady", "namespace": "observability",
			"pod": "node-exporter-prometheus-node-exporter-km6ld", "cluster": "shared",
			"account_id": "111111111111",
		}, "kubepodnotready|observability|pod|node-exporter-prometheus-node-exporter|shared"},
		{"workload label with a workload_type is a Kubernetes object", map[string]string{
			"alertname": "TooManyLogs", "namespace": "observability",
			"workload": "vmagent", "workload_type": "Deployment", "cluster": "shared",
			"account_id": "111111111111", "region": "us-east-1",
		}, "toomanylogs|observability|deployment|vmagent|shared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, account, region, key := decodeOne(t, c.labels)
			if account != "" || region != "" {
				t.Errorf("a Kubernetes workload must carry no cloud qualifier, got account=%q region=%q", account, region)
			}
			if key != c.want {
				t.Errorf("trigger key changed for a Kubernetes workload:\n got  %q\n want %q", key, c.want)
			}
		})
	}
}

// TestAnARNOnlyAccountDoesNotFuseWithAnUnqualifiedFiring pins the one case the two
// requirements cannot both be met in, so it is a decision on the record rather than
// a surprise. A stack that omits the account LABEL leaves the account visible on the
// ARN spelling and invisible on the short one, and no single key can then both fuse
// the two spellings and split two accounts — a map key is compared by equality, so
// fusing them means key(ARN in account A) == key(short) == key(ARN in account B),
// which forces the two accounts equal again.
//
// The account is kept, and the two spellings therefore key apart. That direction is
// chosen on severity: the cost of splitting is a duplicated recurrence bucket — one
// extra investigation and one extra KB pull request — while the cost of collapsing
// is a genuine production incident in one account being silently dropped inside a
// cooldown armed by another account's conclusion. The reproduced defect this change
// exists to close is exactly that shape, with BOTH accounts spelled as ARNs, and
// dropping an ARN-only account would leave it open.
//
// Where the label IS present — every yace/CloudWatch deployment, which stamps
// account_id on every series — the account reaches the key under both spellings and
// TestARNAndShortSpellingsStillFuse holds.
func TestAnARNOnlyAccountDoesNotFuseWithAnUnqualifiedFiring(t *testing.T) {
	base := func(workload string) map[string]string {
		return map[string]string{
			"alertname": "RDSHighCPU", "namespace": "observability",
			"workload": workload, "cluster": "aqemia-shared",
		}
	}
	_, _, _, arnKey := decodeOne(t, base("arn:aws:rds:us-east-1:111111111111:db:datagrok"))
	_, _, _, shortKey := decodeOne(t, base("datagrok"))
	if arnKey == shortKey {
		t.Fatalf("an ARN naming account 111111111111 keyed identically to an unqualified firing (%q) — "+
			"the account it names was dropped, which is what re-opens the cross-account collision", arnKey)
	}
	// Two ARN-spelled firings from two accounts — the reproduced defect — stay apart
	// even with no label to help.
	_, _, _, otherKey := decodeOne(t, base("arn:aws:rds:us-east-1:222222222222:db:datagrok"))
	if arnKey == otherKey {
		t.Fatal("two accounts' ARNs still share one trigger key")
	}
	// And two unqualified firings still fuse: with no account anywhere there is
	// nothing to split on, and the spelling collapse is unchanged.
	_, _, _, shortKey2 := decodeOne(t, base("datagrok"))
	if shortKey != shortKey2 {
		t.Fatal("two unqualified firings of one instance stopped fusing")
	}
}
