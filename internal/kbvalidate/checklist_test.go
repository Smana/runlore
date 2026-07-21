// SPDX-License-Identifier: Apache-2.0

package kbvalidate

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// checklistPath is the kb-steward skill doc this test pins against. It can't
// live in internal/catalog (where the analogous skillcontract_test.go lives)
// because kbvalidate imports catalog — the reverse import would cycle — so
// the check lives here instead, next to the validator it's pinning.
const checklistPath = "../../plugins/kb-steward/skills/kb-steward/references/entry-quality-checklist.md"

// TestChecklistMatchesValidator pins entry-quality-checklist.md's restated
// gate numbers to kbvalidate's actual validator, so the checklist can't drift
// from ValidateStructural without a test noticing.
func TestChecklistMatchesValidator(t *testing.T) {
	raw, err := os.ReadFile(checklistPath)
	if err != nil {
		t.Fatalf("read entry-quality-checklist.md: %v", err)
	}
	doc := string(raw)

	wantLen := strconv.Itoa(maxTitleLen)
	if !strings.Contains(doc, wantLen) {
		t.Errorf("checklist does not mention the max title length %s (kbvalidate.maxTitleLen)", wantLen)
	}

	// Reuse the validator's own required-sections list rather than
	// duplicating the heading names here.
	for _, s := range requiredIncidentSections {
		head := fmt.Sprintf("## %s", s.head)
		if !strings.Contains(doc, head) {
			t.Errorf("checklist does not mention required Incident section %q", head)
		}
	}
}
