// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// TestResourceAgreesAcrossARNAndShortName is the second half of the 2026-08-17/18
// split. The same RDS instance reached RunLore as "observability/datagrok-aqemia-shared"
// on one firing and as "observability/arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared"
// on the next, and the structural recall gate compares those by string equality — so
// a catalog entry filed under either spelling is invisible to the other. That is not
// hypothetical: a live entry is indexed as
// "observability/arn:aws:rds:us-east-1:195275669196:db:compute-stages" while its
// neighbours use short names.
//
// Canonicalising at ingestion cannot fix it on its own — entries already written in
// the ARN form stay written that way — so the gate itself has to be identity-aware.
func TestResourceAgreesAcrossARNAndShortName(t *testing.T) {
	w := func(ns, name string) providers.Workload { return providers.Workload{Namespace: ns, Name: name} }
	const (
		rdsARN   = "arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared"
		otherAcc = "arn:aws:rds:us-east-1:999999999999:db:datagrok-aqemia-shared"
	)
	cases := []struct {
		name      string
		reqW      providers.Workload
		entry     string
		requireWL bool
		want      matchStrength
	}{
		{"arn-spelled alert vs short-name entry",
			w("observability", rdsARN), "observability/datagrok-aqemia-shared", false, matchExact},
		{"short-name alert vs arn-spelled entry (the live catalog entry)",
			w("observability", "datagrok-aqemia-shared"), "observability/" + rdsARN, false, matchExact},
		{"both spelled as the same arn",
			w("observability", rdsARN), "observability/" + rdsARN, false, matchExact},
		{"strict mode still agrees — the two spellings ARE an exact match",
			w("observability", rdsARN), "observability/datagrok-aqemia-shared", true, matchExact},
		{"slash-style arn vs its identifier",
			w("observability", "arn:aws:ec2:us-east-1:142655614335:instance/i-0abc123def4567890"),
			"observability/i-0abc123def4567890", false, matchExact},
		// Identity, not string-collapse: the account qualifies the resource.
		{"the same instance name in two different accounts stays two resources",
			w("observability", rdsARN), "observability/" + otherAcc, false, matchNone},
		{"an arn in another namespace is still another resource",
			w("observability", rdsARN), "other/datagrok-aqemia-shared", false, matchNone},
		// Nothing about Kubernetes matching may move.
		{"plain kubernetes namespace/name is untouched",
			w("apps", "payment-api"), "apps/payment-api", false, matchExact},
		{"two distinct kubernetes workloads stay distinct",
			w("apps", "payment-api"), "apps/web", false, matchNone},
		{"pod-template-hash forgiveness still holds",
			w("tooling", "harbor-registry-59598dbd57-ltkzw"), "tooling/harbor-registry", false, matchExact},
		{"a name merely containing arn is not an arn",
			w("apps", "arnica-exporter"), "apps/exporter", false, matchNone},
		{"bare-namespace tiers are unchanged",
			w("observability", rdsARN), "observability", false, matchNamespace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resourceAgrees(c.reqW, c.entry, c.requireWL); got != c.want {
				t.Errorf("resourceAgrees(%+v, %q, %v) = %v, want %v", c.reqW, c.entry, c.requireWL, got, c.want)
			}
		})
	}
}

// TestEntryAgreesMatchesARNOnEitherStoredResource pins that identity-awareness
// reaches BOTH resources an entry carries — the fault locus and the resource the
// originating alert fired on — because entryAgrees is the function the recall
// pre-filter actually calls, and an entry whose AlertResource is the ARN form is
// exactly the shape the live catalog holds.
func TestEntryAgreesMatchesARNOnEitherStoredResource(t *testing.T) {
	reqW := providers.Workload{Namespace: "observability", Name: "datagrok-aqemia-shared"}
	e := catalog.Entry{
		Resource:      "observability/some-other-thing",
		AlertResource: "observability/arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared",
	}
	if got := entryAgrees(reqW, e, false); got != matchExact {
		t.Fatalf("entryAgrees = %v, want matchExact — the entry's AlertResource is the same DB instance spelled as an ARN", got)
	}
}
