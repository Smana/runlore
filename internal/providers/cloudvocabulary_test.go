// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// The frozen AWS tool text: every model-facing string CloudVocabulary renders, as it
// shipped before the cloud tools started rendering from a vocabulary at all. Captured
// once from the tools' own literals at commit 431f167 (the last commit before
// CloudDescriber existed) and never recomputed since.
//
// A golden, rather than the cross-package comparison this file used to hold. That
// comparison — AWSCloudVocabulary's render against CloudWhatChangedTool's
// Description() — was a real guard exactly as long as the two were independently
// written. The moment Description() became `return vocabularyFor(t.Cloud).
// ChangeDescription()`, both sides of it computed the same call, and it would have
// gone on passing while the AWS wording changed underneath it. Freezing the bytes
// restores the second, independent witness that the comparison used to be.
//
// Changing one of these constants is therefore always a deliberate act: it means
// "yes, I am changing what an AWS deployment tells the model", never "the test broke".
// The `want` side is the live tool method wherever one exists, so the golden covers
// the tool-layer splicing (the schema templates, incident_timeline's sentence) and not
// just the vocabulary renderers underneath it.
const (
	goldenWhatChangedDescription = "List recent MUTATING AWS control-plane events (CloudTrail) — " +
		"ASG/EC2/EKS/RDS/SG changes, manual actions, and other infra changes invisible to GitOps. " +
		"Use when no Git change explains the incident. Optional resource is an EXACT CloudTrail " +
		"ResourceName — a full ARN, instance-id, ASG name, or a resource's full path (e.g. a Secrets " +
		"Manager secret's \"apps/team/name\") — never a service name or substring; OMIT it to see " +
		"every mutating event, which is the right move when you do not know the exact identifier. " +
		"Set failed_only=true when the incident IS a failed AWS operation and you do not know which " +
		"resource it happened to (a failed backup/snapshot job, a rejected API call): results are " +
		"capped at the NEWEST events, which on a busy cluster are routine instance and tag churn, so " +
		"the rejected call you are looking for is usually just past the cap. failed_only spends the " +
		"cap on rejected calls instead and reports each one's error code. since_minutes default 90 " +
		"(CloudTrail lags ~15m)."

	goldenWhatChangedSchema = "{\"type\":\"object\",\"properties\":{\"resource\":{\"type\":\"string\"},\"since_minutes\":{\"type\":\"integer\"},\"failed_only\":{\"type\":\"boolean\",\"description\":\"keep " +
		"only MUTATING control-plane calls that were REJECTED, reporting each error code; use when " +
		"the incident is itself a failed AWS write operation. Read-only calls are never listed by " +
		"this tool, so a denied Describe/Get will NOT appear here\"}},\"required\":[]}"

	goldenResourceHealthDescription = "Describe AWS-side health for the cluster's nodes/capacity: " +
		"EKS nodegroup status + health issues, ASG scaling activities (launch/capacity failures), " +
		"and — when given an EC2 instance-id (i-…) — its instance/system status checks. Use to " +
		"confirm a node/infra/capacity cause. Optional since_minutes scopes the scaling-activity " +
		"lookback to the incident window (default: recent activities)."

	goldenResourceHealthSchema = "{\"type\":\"object\",\"properties\":{\"instance_id\":{\"type\":\"string\",\"description\":\"optional " +
		"EC2 instance id (i-…)\"},\"since_minutes\":{\"type\":\"integer\",\"description\":\"scope " +
		"scaling-activity lookback to the last N minutes\"}},\"required\":[]}"

	goldenTimelineDescription = "Build ONE time-sorted incident timeline for a namespace by fusing " +
		"GitOps changes (deploys/reconciles + what the diff touched), cloud control-plane changes " +
		"(CloudTrail: ASG/EC2/EKS/manual actions), and Kubernetes Warning Events — merged and " +
		"ordered by timestamp so you see the incident chronology at a glance instead of stitching " +
		"timestamps across separate tools. USE THIS EARLY to establish the sequence (\"deploy at " +
		"14:02 → first crash at 14:33\"), then drill into a specific row with what_changed / " +
		"kube_events / cloud_what_changed. Each row is tagged with its datasource: [git] [flux] " +
		"[argocd] [cloud] [event]. since_minutes bounds the window (default 120)."

	goldenWidenedBanner = "resource \"guessed\" matched no CloudTrail events — ResourceName is an " +
		"exact match on the full AWS resource name or ARN (e.g. a secret's full path " +
		"\"apps/team/name\"), not a service or substring. Showing ALL mutating events in the window " +
		"instead:\n"

	goldenNoChanges = "no mutating AWS events in the window"

	goldenNoFailedChanges = "no FAILED AWS control-plane calls in the window (successful events were " +
		"not listed — re-run without failed_only to see them)"

	goldenNoHealth = "no AWS resource health returned"
)

// widenedBannerSentinel is the resource name the frozen goldenWidenedBanner was
// rendered with. Any value works — it only has to match what the golden captured.
const widenedBannerSentinel = "guessed"

// diffAt reports the first byte at which a and b diverge, with a ±40-char window of
// context, instead of dumping two ~700-character %q strings that bury a single
// swapped word or missing space in noise a human has to diff by eye.
func diffAt(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	if i == len(a) && i == len(b) {
		return "(equal)"
	}
	const window = 40
	ctx := func(s string) string {
		start := i - window
		if start < 0 {
			start = 0
		}
		end := i + window
		if end > len(s) {
			end = len(s)
		}
		return fmt.Sprintf("%q", s[start:end])
	}
	return fmt.Sprintf("first differ at byte %d\n got:  …%s…\n want: …%s…", i, ctx(a), ctx(b))
}

// TestAWSCloudVocabularyStillRendersTheShippedAWSText is the whole compatibility
// promise of CloudDescriber as one executable claim: an AWS deployment reads exactly
// the tool text it read before any of this existed.
//
// It covers every surface, not just the two Description() methods, because the two
// that are easiest to forget are the ones no Description() test would touch. The JSON
// Schemas each carry a cloud-specific argument description spliced in at render time,
// and they reach the model verbatim — internal/model/anthropic/anthropic.go passes
// Schema() through as a json.RawMessage. The empty-result strings never appear in a
// prompt at all; they appear in a RESULT, which is worse, because that is the sentence
// a model quotes into a finding a human then reads as evidence.
//
// incident_timeline is in the table for a reason that is easy to miss: it is
// registered whenever ANY of its three datasources is wired, cloud included, so its
// description ships next to cloud_what_changed's on every cloud-enabled install.
func TestAWSCloudVocabularyStillRendersTheShippedAWSText(t *testing.T) {
	v := providers.AWSCloudVocabulary()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "cloud_what_changed's description is the text it shipped with",
			got:  investigate.CloudWhatChangedTool{}.Description(),
			want: goldenWhatChangedDescription,
		},
		{
			name: "cloud_what_changed's schema, including failed_only's description, is unchanged",
			got:  investigate.CloudWhatChangedTool{}.Schema(),
			want: goldenWhatChangedSchema,
		},
		{
			name: "cloud_resource_health's description is the text it shipped with",
			got:  investigate.CloudResourceHealthTool{}.Description(),
			want: goldenResourceHealthDescription,
		},
		{
			name: "cloud_resource_health's schema, including instance_id's description, is unchanged",
			got:  investigate.CloudResourceHealthTool{}.Schema(),
			want: goldenResourceHealthSchema,
		},
		{
			name: "incident_timeline still names CloudTrail on an AWS deployment",
			got:  investigate.IncidentTimelineTool{}.Description(),
			want: goldenTimelineDescription,
		},
		{
			name: "the dropped-scope banner still explains CloudTrail's exact-match rule",
			got:  v.WidenedBanner(widenedBannerSentinel),
			want: goldenWidenedBanner,
		},
		{
			name: "a quiet window still reports no mutating AWS events",
			got:  v.EmptyChangesMessage(),
			want: goldenNoChanges,
		},
		{
			name: "an empty failed_only lookup still names the filter that emptied it",
			got:  v.EmptyFailedChangesMessage(),
			want: goldenNoFailedChanges,
		},
		{
			name: "an empty health lookup still reports no AWS resource health",
			got:  v.EmptyHealthMessage(),
			want: goldenNoHealth,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("AWS model-facing text changed: %s", diffAt(tt.got, tt.want))
			}
		})
	}
}

// TestAWSCloudVocabularyStillCarriesTheFailedOnlyGuidance is the substance check the
// golden above cannot be, by itself: someone deliberately rewording cloud_what_changed
// updates the golden wholesale, and at that exact moment byte-identity stops being
// able to tell "deliberate reword" from "silently dropped FailureFilterNote's
// paragraph" — see that field's doc comment for why the paragraph exists at all. This
// checks the substance directly, so it still catches the drop even then.
//
// Both failed_only surfaces are covered. The description explains WHEN to reach for
// the flag; the schema's argument description states what the tool can and cannot
// show. Losing either leaves the flag reachable but unusable, and PR #551 added them
// as a pair.
func TestAWSCloudVocabularyStillCarriesTheFailedOnlyGuidance(t *testing.T) {
	v := providers.AWSCloudVocabulary()
	desc := v.ChangeDescription()
	schema := investigate.CloudWhatChangedTool{}.Schema()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"the description names the argument the model must set", desc, "failed_only=true"},
		{"the description says when: the incident IS a failed operation, resource unknown", desc, "the incident IS a failed AWS operation"},
		{"the description explains WHY: the result cap lands on routine churn, not the failure", desc, "capped at the NEWEST events"},
		{"the description says what setting it changes: the cap is spent on rejected calls", desc, "failed_only spends the cap on rejected calls"},
		{"the schema still declares the argument at all", schema, `"failed_only":{"type":"boolean"`},
		{"the schema says what it keeps: rejected mutating calls, with error codes", schema, "were REJECTED, reporting each error code"},
		{"the schema warns that denied reads are NOT visible here", schema, "a denied Describe/Get will NOT appear here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tt.in, tt.want) {
				t.Errorf("failed_only guidance %q is gone\ngot: %q", tt.want, tt.in)
			}
		})
	}
}

// TestChangeDescriptionToleratesAnEmptyFailureFilterNote locks in the renderer
// contract documented on CloudVocabulary: FailureFilterNote == "" (a cloud with no
// failed-call filter to explain) must not leave a double space or an empty sentence
// in the rendered text — the field is genuinely optional, not merely nullable.
func TestChangeDescriptionToleratesAnEmptyFailureFilterNote(t *testing.T) {
	v := providers.AWSCloudVocabulary()
	v.FailureFilterNote = ""
	desc := v.ChangeDescription()
	if strings.Contains(desc, "  ") {
		t.Errorf("ChangeDescription() with FailureFilterNote==\"\" contains a double space:\n%q", desc)
	}
	if !strings.Contains(desc, "identifier. since_minutes") {
		t.Errorf("ChangeDescription() with FailureFilterNote==\"\" should join the scope-guidance sentence "+
			"directly to since_minutes with one space; got:\n%q", desc)
	}
}

// TestEngineConstantsAreAllPairwiseDistinct guards the one way a new engine
// constant can go wrong silently: colliding with an existing value would fuse two
// engines' changes into one bucket everywhere Engine is used as a map key. Table
// form (rather than one bespoke EngineGCP-vs-EngineAWS check) means a future
// EngineGKE = "gcp" typo is caught too, not just the one pair someone thought to
// check.
func TestEngineConstantsAreAllPairwiseDistinct(t *testing.T) {
	engines := []struct {
		name string
		val  providers.Engine
	}{
		{"flux", providers.EngineFlux},
		{"argocd", providers.EngineArgoCD},
		{"aws", providers.EngineAWS},
		{"gcp", providers.EngineGCP},
	}
	for i := range engines {
		for j := i + 1; j < len(engines); j++ {
			if engines[i].val == engines[j].val {
				t.Errorf("%s and %s share the Engine value %q — every consumer keying by Engine would fuse them",
					engines[i].name, engines[j].name, engines[i].val)
			}
		}
	}
	if providers.EngineGCP != "gcp" {
		t.Errorf("EngineGCP = %q, want %q", providers.EngineGCP, "gcp")
	}
}
