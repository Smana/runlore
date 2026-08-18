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

// TestIncidentKeyCollapsesTheTwoSpellingsOfOneResource is the recurrence half of the
// 2026-08-17/18 split. One RDS instance fired at 21:18Z identified by its
// DBInstanceIdentifier dimension and again at 03:53Z identified by its full ARN. The
// TriggerKey this builds is the recurrence ledger's grouping key and is compared by
// string equality, so the two firings landed in two buckets: one recurring fault
// read as two unrelated first sightings, and the suppression gate never armed.
func TestIncidentKeyCollapsesTheTwoSpellingsOfOneResource(t *testing.T) {
	short := IncidentKey("RDSHighCPU", "observability", "", rdsShortName, "aqemia-shared")
	arn := IncidentKey("RDSHighCPU", "observability", "", rdsARN, "aqemia-shared")
	if short == "" {
		t.Fatal("IncidentKey returned no key for a well-formed alert")
	}
	if short != arn {
		t.Fatalf("the two spellings of one RDS instance produced two trigger keys:\n short: %q\n arn:   %q", short, arn)
	}
	// The key must still separate genuinely different resources.
	if other := IncidentKey("RDSHighCPU", "observability", "", "compute-stages", "aqemia-shared"); other == short {
		t.Fatal("two different DB instances collapsed to the same trigger key")
	}
	// …and everything else the key already qualified on stays qualifying.
	if other := IncidentKey("RDSHighCPU", "observability", "", rdsARN, "aqemia-dev"); other == short {
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
