// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

const (
	rdsShortName = "datagrok-aqemia-shared"
	rdsARN       = "arn:aws:rds:us-east-1:142655614335:db:" + rdsShortName
)

// rds builds the observability-scoped cloud workload the key cases share: no kind
// (a cloud resource is not a Kubernetes object), and the account as its own field —
// which is where ingestion puts it, having read it from the alert labels and from an
// ARN-spelled name alike (providers.ResolveWorkloadIdentity).
func rds(name, account string) providers.Workload {
	return providers.Workload{Namespace: "observability", Name: name, Account: account}
}

// TestIncidentKeyCollapsesTheTwoSpellingsOfOneResource is the recurrence half of the
// 2026-08-17/18 split. One RDS instance fired at 21:18Z identified by its
// DBInstanceIdentifier dimension and again at 03:53Z identified by its full ARN. The
// TriggerKey this builds is the recurrence ledger's grouping key and is compared by
// string equality, so the two firings landed in two buckets: one recurring fault
// read as two unrelated first sightings, and the suppression gate never armed.
func TestIncidentKeyCollapsesTheTwoSpellingsOfOneResource(t *testing.T) {
	short := IncidentKey("RDSHighCPU", rds(rdsShortName, ""), "aqemia-shared")
	arn := IncidentKey("RDSHighCPU", rds(rdsARN, ""), "aqemia-shared")
	if short == "" {
		t.Fatal("IncidentKey returned no key for a well-formed alert")
	}
	if short != arn {
		t.Fatalf("the two spellings of one RDS instance produced two trigger keys:\n short: %q\n arn:   %q", short, arn)
	}
	// The key must still separate genuinely different resources.
	if other := IncidentKey("RDSHighCPU", rds("compute-stages", ""), "aqemia-shared"); other == short {
		t.Fatal("two different DB instances collapsed to the same trigger key")
	}
	// …and everything else the key already qualified on stays qualifying.
	if other := IncidentKey("RDSHighCPU", rds(rdsARN, ""), "aqemia-dev"); other == short {
		t.Fatal("the cluster stopped qualifying the trigger key")
	}
}

// TestDupFingerprintCollapsesTheTwoSpellingsOfOneResource pins the same collapse on
// the curation side: without it the ARN firing opens a SECOND knowledge-base pull
// request for a resource the catalog already has an entry for.
func TestDupFingerprintCollapsesTheTwoSpellingsOfOneResource(t *testing.T) {
	inv := func(name string) providers.Investigation {
		return providers.Investigation{
			Title:      "RDSHighCPU",
			Resource:   providers.Workload{Namespace: "observability", Name: name},
			RootCauses: []providers.Hypothesis{{Summary: "connection pool exhausted by the nightly backfill"}},
		}
	}
	short, arn := DupFingerprint(inv(rdsShortName)), DupFingerprint(inv(rdsARN))
	if short == "" {
		t.Fatal("DupFingerprint returned no fingerprint for a well-formed finding")
	}
	if short != arn {
		t.Fatalf("the two spellings of one RDS instance produced two dup fingerprints:\n short: %q\n arn:   %q", short, arn)
	}
}

// TestIncidentKeySplitsTwoAWSAccounts is the collision this change closes. Two AWS
// accounts scraped by one exporter through one Prometheus agree on every other field
// the key is built from — the alertname is the rule's, the namespace is the
// EXPORTER's, the kind is empty for a cloud resource, and the `cluster` external
// label is stamped once per Prometheus across every account it watches — so one
// instance name in two accounts was ONE key. That key is the recurrence ledger's
// grouping key, so inside a cooldown the second account's genuine incident was
// suppressed on the first account's conclusion: no model call, no notification, no
// ledger entry.
func TestIncidentKeySplitsTwoAWSAccounts(t *testing.T) {
	a := IncidentKey("RDSHighCPU", rds("datagrok", "111111111111"), "aqemia-shared")
	b := IncidentKey("RDSHighCPU", rds("datagrok", "222222222222"), "aqemia-shared")
	if a == "" {
		t.Fatal("IncidentKey returned no key for a well-formed cloud alert")
	}
	if a == b {
		t.Fatalf("two AWS accounts hosting an instance named \"datagrok\" share one trigger key %q", a)
	}
	// The account qualifies; it does not replace. One account still fuses with itself
	// however the name was spelled, because the key reads the Account FIELD and not an
	// account embedded in the name — ingestion has already reconciled the two.
	arn := IncidentKey("RDSHighCPU", rds(rdsARN, "111111111111"), "aqemia-shared")
	short := IncidentKey("RDSHighCPU", rds(rdsShortName, "111111111111"), "aqemia-shared")
	if arn != short {
		t.Fatalf("the two spellings of one instance in one account produced two keys:\n arn:   %q\n short: %q", arn, short)
	}
}

// TestIncidentKeyForKubernetesIsByteIdentical is the regression this change is most
// likely to cause, so the expected bytes are written out literally: a Kubernetes
// object has no AWS account, must not grow a key segment, and must key exactly as it
// did before the account existed. Persisted recurrence counts for every Kubernetes
// alert depend on it.
func TestIncidentKeyForKubernetesIsByteIdentical(t *testing.T) {
	cases := []struct {
		w    providers.Workload
		want string
	}{
		{providers.Workload{Kind: "Pod", Namespace: "observability", Name: "node-exporter-prometheus-node-exporter-km6ld"},
			"kubepodnotready|observability|pod|node-exporter-prometheus-node-exporter|shared"},
		{providers.Workload{Kind: "Deployment", Namespace: "apps", Name: "payment-api"},
			"kubepodnotready|apps|deployment|payment-api|shared"},
		{providers.Workload{Namespace: "apps"},
			"kubepodnotready|apps|||shared"},
	}
	for _, c := range cases {
		if got := IncidentKey("KubePodNotReady", c.w, "shared"); got != c.want {
			t.Errorf("IncidentKey(%+v):\n got  %q\n want %q", c.w, got, c.want)
		}
	}
}

// TestDupFingerprintSplitsTwoAWSAccounts pins the same split on the curation side:
// without it, two accounts' incidents coalesce into ONE knowledge-base pull request
// describing whichever account was investigated first.
func TestDupFingerprintSplitsTwoAWSAccounts(t *testing.T) {
	inv := func(account string) providers.Investigation {
		return providers.Investigation{
			Title:      "RDSHighCPU",
			Resource:   rds("datagrok", account),
			RootCauses: []providers.Hypothesis{{Summary: "connection pool exhausted by the nightly backfill"}},
		}
	}
	a, b := DupFingerprint(inv("111111111111")), DupFingerprint(inv("222222222222"))
	if a == "" {
		t.Fatal("DupFingerprint returned no fingerprint for a well-formed finding")
	}
	if a == b {
		t.Fatal("two AWS accounts hosting the same instance name share one dup fingerprint")
	}
	// A workload with no account is fingerprinted exactly as it was.
	unqualified := DupFingerprint(inv(""))
	if unqualified == a || unqualified == b {
		t.Fatal("an unqualified finding collided with a qualified one")
	}
}
