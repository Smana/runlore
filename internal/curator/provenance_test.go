// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestDraftKBEntryProvenanceLinks(t *testing.T) {
	inv := providers.Investigation{
		Title:      "Harbor crashloop after chart bump",
		Confidence: 0.9,
		Verified:   true,
		RootCauses: []providers.Hypothesis{{
			Summary:    "chart 1.15 enabled migrations",
			Confidence: 0.9,
			ChangeRef:  "flux-system/apps: aaa1111..bbb2222",
			ChangeURL:  "https://github.com/org/infra/compare/aaa1111...bbb2222",
			Provenance: []providers.ProvenanceLink{
				{Label: "apps/harbor/values.yaml#L42", URL: "https://github.com/org/infra/blob/bbb2222/apps/harbor/values.yaml#L42", Kind: "file"},
				{Label: "bad", URL: "http://evil.example/x", Kind: "file"},
			},
			Evidence:        []string{"migration lock timeout"},
			SuggestedAction: "roll back chart to 1.14.2",
			Reversible:      true,
		}},
	}

	entry := draftKBEntry(inv)
	for _, want := range []string{
		"[flux-system/apps: aaa1111..bbb2222](https://github.com/org/infra/compare/aaa1111...bbb2222)",
		"### Provenance",
		"- [apps/harbor/values.yaml#L42](https://github.com/org/infra/blob/bbb2222/apps/harbor/values.yaml#L42)",
	} {
		if !strings.Contains(entry.Body, want) {
			t.Errorf("draftKBEntry Body missing %q:\n%s", want, entry.Body)
		}
	}
	if strings.Contains(entry.Body, "http://evil.example") {
		t.Errorf("draftKBEntry emitted non-HTTPS provenance link:\n%s", entry.Body)
	}
}

func TestDraftKBEntryWithoutChangeURLUnchanged(t *testing.T) {
	inv := providers.Investigation{
		Title:      "Harbor crashloop",
		Confidence: 0.9,
		Verified:   true,
		RootCauses: []providers.Hypothesis{{
			Summary:         "chart bump",
			Confidence:      0.9,
			ChangeRef:       "aaa1111..bbb2222",
			Evidence:        []string{"crash"},
			SuggestedAction: "rollback",
		}},
	}
	entry := draftKBEntry(inv)
	if strings.Contains(entry.Body, "### Provenance") {
		t.Errorf("unexpected ### Provenance section when no provenance links exist:\n%s", entry.Body)
	}
	if !strings.Contains(entry.Body, "— change: aaa1111..bbb2222\n") {
		t.Errorf("expected plain ChangeRef when ChangeURL is empty:\n%s", entry.Body)
	}
}
