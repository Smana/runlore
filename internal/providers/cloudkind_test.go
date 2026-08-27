// SPDX-License-Identifier: Apache-2.0

package providers

import "testing"

// TestCloudKindQualifies is the contract internal/notify depends on: every cloud
// resource type arrives carrying a ':', which is the one character no Kubernetes kind can
// contain — and a type that is already colon-qualified is not prefixed twice.
func TestCloudKindQualifies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine Engine
		in     string
		want   string
	}{
		{"a GCP monitored-resource type gains the prefix it has no colon of its own",
			EngineGCP, "gke_nodepool", "gcp::gke_nodepool"},
		{"an AWS resource type is already colon-qualified and passes through unchanged",
			EngineAWS, "AWS::EC2::Instance", "AWS::EC2::Instance"},
		{"an AWS event source is dotted, not colon-qualified, so it gains the prefix",
			EngineAWS, "ec2.amazonaws.com", "aws::ec2.amazonaws.com"},
		{"an empty type stays empty rather than becoming a bare prefix, since a kind " +
			"nothing supplied is not a claim about anything", EngineGCP, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CloudKind(tc.engine, tc.in); got != tc.want {
				t.Errorf("CloudKind(%q, %q) = %q, want %q", tc.engine, tc.in, got, tc.want)
			}
		})
	}
}

// TestCallPathOmitsAnAbsentPrincipalRatherThanDangling pins the shared renderer both
// cloud audit lenses put in Change.Source.Path.
//
// The dangling case is not an edge case: it is every Google-initiated system_event and
// every AWS service-initiated call. "compute.instances.hostError by " reads as a caller
// whose identity was LOST, which is a different and far more alarming claim than "nobody
// called this".
func TestCallPathOmitsAnAbsentPrincipalRatherThanDangling(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		call, principal, code, message string
		want                           string
	}{
		{"a successful call by a known principal", "SetNodePoolSize", "alice@example.com", "", "",
			"SetNodePoolSize by alice@example.com"},
		{"no principal means no ' by ' clause at all", "compute.instances.hostError", "", "", "",
			"compute.instances.hostError"},
		{"a failure carries the code, which is the highest-value part of the line",
			"SetNodePoolSize", "alice@example.com", "RESOURCE_EXHAUSTED", "",
			"SetNodePoolSize by alice@example.com — FAILED: RESOURCE_EXHAUSTED"},
		{"a failure message is parenthesised after the code",
			"SetNodePoolSize", "", "RESOURCE_EXHAUSTED", "out of capacity",
			"SetNodePoolSize — FAILED: RESOURCE_EXHAUSTED (out of capacity)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CallPath(tc.call, tc.principal, tc.code, tc.message); got != tc.want {
				t.Errorf("CallPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestChangeTruncatedNoteIsOneSentenceForBothClouds: a partial view must read identically
// on either cloud. Both provider packages used to hold a byte-identical private copy —
// one typed int, one int64 — with the invariant asserted only in a doc comment.
func TestChangeTruncatedNoteIsOneSentenceForBothClouds(t *testing.T) {
	got := ChangeTruncatedNote(25)
	want := "results truncated at 25 — more events matched; narrow the window or resource"
	if got != want {
		t.Errorf("ChangeTruncatedNote(25) = %q, want %q", got, want)
	}
}

// TestEveryVocabularyFragmentIsRequired guards the failure the AWS golden structurally
// cannot catch: a new cloud that forgets a fragment ships a sentence with a hole in it.
func TestEveryVocabularyFragmentIsRequired(t *testing.T) {
	if err := (CloudVocabulary{}).Validate(); err == nil {
		t.Fatal("an empty vocabulary validated")
	}
	// HealthWindowArg is the fragment this branch added; a vocabulary missing it renders
	// a since_minutes schema description that is empty.
	v := AWSCloudVocabulary()
	v.HealthWindowArg = ""
	err := v.Validate()
	if err == nil {
		t.Fatal("a vocabulary with no HealthWindowArg validated, so a cloud can ship an " +
			"argument whose schema says nothing")
	}
}
