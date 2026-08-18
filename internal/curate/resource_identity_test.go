// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"sort"
	"testing"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

const (
	rdsShortRef = "observability/datagrok-aqemia-shared"
	rdsARNRefA  = "observability/arn:aws:rds:us-east-1:111111111111:db:datagrok-aqemia-shared"
	rdsARNRefB  = "observability/arn:aws:rds:us-east-1:222222222222:db:datagrok-aqemia-shared"
)

// gapTitles runs the pass over eps and returns the knowledge-gap issue titles it
// opened, sorted so a map-ordered pass compares deterministically.
func gapTitles(t *testing.T, eps []outcome.Episode) []string {
	t.Helper()
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := append([]string(nil), gf.openedTitles...)
	sort.Strings(got)
	return got
}

// TestRecurrenceGroupsPodsOfOneDeployment is the CORE-681 half of the finding. Three
// pods of ONE Deployment recur; a pod-scoped alert (KubePodNotReady carries only a
// `pod` label) arrives with the full pod name, so grouping on the raw ref read three
// unrelated resources, each one short of the threshold, and the knowledge gap was
// never reported at all. resourceAgrees and curator.IncidentKey already forgive this
// suffix; this pass did not.
func TestRecurrenceGroupsPodsOfOneDeployment(t *testing.T) {
	eps := []outcome.Episode{
		{Resource: "tooling/harbor-registry-59598dbd57-ltkzw", Resolved: false},
		{Resource: "tooling/harbor-registry-59598dbd57-9q2xd", Resolved: false},
		{Resource: "tooling/harbor-registry-7f4b9c8d6-mnp4q", Resolved: false},
	}
	if got := gapTitles(t, eps); len(got) != 1 || got[0] != "knowledge-gap: tooling/harbor-registry" {
		t.Fatalf("three pods of one Deployment must be one recurrence, got %v", got)
	}
}

// TestRecurrenceKubernetesRefUnchanged is the regression this change is most likely
// to cause, so the expected bytes are written out literally. A plain namespace/name
// ref carries no ARN scaffolding and no pod hash, so its pattern — which is also the
// issue TITLE, and therefore the idempotency key against already-open issues — must
// be byte-identical to what it was.
func TestRecurrenceKubernetesRefUnchanged(t *testing.T) {
	for _, ref := range []string{"apps/web", "apps/redis-cache", "apps"} {
		if got := gapTitles(t, unresolved(ref, 3)); len(got) != 1 || got[0] != "knowledge-gap: "+ref {
			t.Fatalf("Kubernetes ref %q must group unchanged, got %v", ref, got)
		}
	}
}

// TestRecurrenceGroupsTheTwoSpellingsOfOneResource is the cloud half of the finding.
// One S3 bucket reaches the ledger under its ARN on one investigation and under its
// bare name on the next (inv.Resource is model-written, so whichever spelling the
// model echoed from the seed prompt is what was persisted). Grouped as raw strings
// those were two unrelated resources: one recurring fault, two buckets of two, and
// no gap issue.
func TestRecurrenceGroupsTheTwoSpellingsOfOneResource(t *testing.T) {
	eps := append(unresolved("observability/arn:aws:s3:::prod-logs", 2), unresolved("observability/prod-logs", 1)...)
	if got := gapTitles(t, eps); len(got) != 1 || got[0] != "knowledge-gap: observability/prod-logs" {
		t.Fatalf("the ARN and short spellings of one bucket must be one recurrence, got %v", got)
	}
}

// TestRecurrenceSplitsTwoAWSAccounts pins the split the base change established. Two
// AWS accounts can host an instance under the same name and those are genuinely
// different databases, so collapsing an ARN to its bare identifier must not fuse
// them — the curation backlog would then groom two production incidents as one.
func TestRecurrenceSplitsTwoAWSAccounts(t *testing.T) {
	eps := append(unresolved(rdsARNRefA, 3), unresolved(rdsARNRefB, 3)...)
	want := []string{
		"knowledge-gap: " + rdsShortRef + "@111111111111",
		"knowledge-gap: " + rdsShortRef + "@222222222222",
	}
	got := gapTitles(t, eps)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("two accounts must stay two recurrences:\n got:  %v\n want: %v", got, want)
	}
}

// TestRecurrenceAccountResidualKeepsTheSpellingsApart pins what this layer CANNOT
// recover, so nobody reads the two tests above as a full fix. outcome.Episode carries
// the resource as a rendered ref string and Workload.Ref() drops Account, so the
// account is present only when the persisted spelling was an ARN. An ARN-spelled
// firing therefore knows its account and a short-spelled one does not, and no single
// grouping key can fuse the two spellings without re-fusing the two accounts. The
// account is kept — splitting costs a duplicate gap issue, collapsing costs grooming
// one account's incident under another's.
func TestRecurrenceAccountResidualKeepsTheSpellingsApart(t *testing.T) {
	eps := append(unresolved(rdsARNRefA, 3), unresolved(rdsShortRef, 3)...)
	want := []string{
		"knowledge-gap: " + rdsShortRef,
		"knowledge-gap: " + rdsShortRef + "@111111111111",
	}
	got := gapTitles(t, eps)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the qualified/unqualified residual is not what was pinned:\n got:  %v\n want: %v", got, want)
	}
}

// TestRecurrenceSuppressedEscalationGroupsPodFamily checks the suppressed
// (closed-unmerged) branch reads the same identity: it keys its COUNT on the
// fingerprint but renders and idempotency-guards on the pattern, so a pod-scoped
// escalation must title itself with the controller family too.
func TestRecurrenceSuppressedEscalationGroupsPodFamily(t *testing.T) {
	eps := []outcome.Episode{
		{Resource: "tooling/harbor-registry-59598dbd57-ltkzw", DupFingerprint: "fp", Resolved: false},
		{Resource: "tooling/harbor-registry-59598dbd57-9q2xd", DupFingerprint: "fp", Resolved: false},
		{Resource: "tooling/harbor-registry-7f4b9c8d6-mnp4q", DupFingerprint: "fp", Resolved: false},
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{
		Forge:      gf,
		Ledger:     fakeLedger{eps: eps},
		Threshold:  3,
		Suppressed: fakeSuppression{set: map[string]SuppressedEntry{"fp": {Fingerprint: "fp", PRNumber: 7}}},
		Log:        discardLog(),
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: tooling/harbor-registry" {
		t.Fatalf("the escalation must name the controller family, got %v", gf.openedTitles)
	}
}

// TestRecurrencePatternReusesProvidersIdentity asserts the grouping key is the shared
// helper's output and not a private restatement of it: a fourth copy of resource
// normalisation is exactly the drift this programme has already found twice.
func TestRecurrencePatternReusesProvidersIdentity(t *testing.T) {
	for _, name := range []string{
		"harbor-registry-59598dbd57-ltkzw",
		"arn:aws:s3:::prod-logs",
		"arn:aws:ec2:eu-west-1:111111111111:instance/i-0abc",
		"redis-cache",
	} {
		got := recurrencePattern(outcome.Episode{Resource: "ns/" + name})
		id := providers.ParseResourceID(name)
		want := "ns/" + id.Name
		if id.Account != "" {
			want += "@" + id.Account
		}
		if got != want {
			t.Fatalf("recurrencePattern(%q) = %q, want %q (providers.ParseResourceID)", name, got, want)
		}
	}
}
