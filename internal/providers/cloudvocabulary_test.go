// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

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

// TestAWSCloudVocabularyReproducesTheLiveToolText compares AWSCloudVocabulary's
// rendered descriptions against CloudWhatChangedTool/CloudResourceHealthTool's
// ACTUAL Description() methods — a real cross-package import, not a pasted copy.
// internal/notify/slack_silence_blockid_guard_test.go:17-36 states the repo's
// standing position on why: a "must match" comment on both sides of a duplicated
// string is not a guard, because renaming one side still leaves both halves' own
// tests green. This compiles as an external test (package providers_test) importing
// internal/investigate, which itself imports internal/providers — legal because the
// production `providers` package carries no dependency on its own test files.
//
// NOTE: this comparison is correct only until Task 2 rewires CloudWhatChangedTool
// and CloudResourceHealthTool to render their descriptions FROM this vocabulary.
// Once that lands, both sides of every case below compute the exact same call, and
// the test becomes tautological — it would keep passing even if the rendered text
// were wrong, since there is nothing independent left to compare against. At that
// point this test should convert to a frozen golden string, captured once from
// pre-Task-2 behaviour and pinned independently of whatever AWSCloudVocabulary
// computes.
func TestAWSCloudVocabularyReproducesTheLiveToolText(t *testing.T) {
	v := providers.AWSCloudVocabulary()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "cloud_what_changed's description matches CloudWhatChangedTool.Description()",
			got:  v.ChangeDescription(),
			want: investigate.CloudWhatChangedTool{}.Description(),
		},
		{
			name: "cloud_resource_health's description matches CloudResourceHealthTool.Description()",
			got:  v.HealthDescription(),
			want: investigate.CloudResourceHealthTool{}.Description(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("rendered text drifted from the live tool description: %s", diffAt(tt.got, tt.want))
			}
		})
	}
}

// TestAWSCloudVocabularyInstanceArgMatchesTheLiveSchema pins InstanceArg the same
// way: CloudResourceHealthTool.Schema() embeds its instance_id argument description
// directly in a JSON string literal (and still will after Task 2, which rewires the
// two Description() methods but not Schema()), so a substring check against the
// live Schema() is the honest guard here — weaker than byte-identity, since a schema
// could wrap InstanceArg in more prose, but real: an exported constant with a typo
// fails it.
func TestAWSCloudVocabularyInstanceArgMatchesTheLiveSchema(t *testing.T) {
	v := providers.AWSCloudVocabulary()
	schema := investigate.CloudResourceHealthTool{}.Schema()
	if !strings.Contains(schema, v.InstanceArg) {
		t.Errorf("AWSCloudVocabulary().InstanceArg %q not found in CloudResourceHealthTool.Schema():\n%s", v.InstanceArg, schema)
	}
}

// TestAWSCloudVocabularyStillCarriesTheFailedOnlyGuidance is the substance check
// that byte-identity (above) cannot be, by itself: someone deliberately rewording
// ChangeDescription updates that comparison wholesale, and at that exact moment
// byte-identity stops being able to tell "deliberate reword" from "silently dropped
// FailureFilterNote's paragraph" — see that field's doc comment for why the
// paragraph exists at all. This test checks the paragraph's substance directly, so
// it still catches the drop even then.
func TestAWSCloudVocabularyStillCarriesTheFailedOnlyGuidance(t *testing.T) {
	desc := providers.AWSCloudVocabulary().ChangeDescription()
	tests := []struct {
		name string
		want string
	}{
		{"names the argument the model must set", "failed_only=true"},
		{"says when to use it: incident IS a failed operation, resource unknown", "the incident IS a failed AWS operation"},
		{"explains WHY: the result cap lands on routine churn, not the failure", "capped at the NEWEST events"},
		{"explains what setting it changes: spends the cap on rejected calls", "failed_only spends the cap on rejected calls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(desc, tt.want) {
				t.Errorf("ChangeDescription() missing failed_only guidance %q\ngot: %q", tt.want, desc)
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
