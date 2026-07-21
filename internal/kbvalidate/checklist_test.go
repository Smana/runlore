// SPDX-License-Identifier: Apache-2.0

package kbvalidate

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pinnedDocs are the kb-steward skill docs that restate this validator's gate
// values. They can't be pinned from internal/catalog (where the analogous
// skillcontract_test.go lives) because kbvalidate imports catalog — the reverse
// import would cycle — so the check lives here, next to what it pins.
var pinnedDocs = []string{
	"../../plugins/kb-steward/skills/kb-steward/references/entry-quality-checklist.md",
	"../../plugins/kb-steward/skills/kb-steward/references/okf-format.md",
}

// titleLimitRE captures the number the docs state as the title limit. Matching
// the surrounding phrase — not a bare strings.Contains of the number — is what
// makes this a real pin: the checklist also contains "~40 chars" and a "base64"
// mention, so a bare substring search for maxTitleLen would pass on an
// unrelated digit string while the stated limit silently drifted.
var titleLimitRE = regexp.MustCompile(`≤\s*(\d+)\s*bytes`)

// TestChecklistMatchesValidator pins the skill docs' restated gate values to
// this package's actual validator.
func TestChecklistMatchesValidator(t *testing.T) {
	for _, path := range pinnedDocs {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo doc paths
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc := string(raw)

		m := titleLimitRE.FindStringSubmatch(doc)
		if m == nil {
			t.Errorf("%s: no title limit of the form \"≤N bytes\" found; it must state the gate's limit", path)
		} else if got, err := strconv.Atoi(m[1]); err != nil || got != maxTitleLen {
			t.Errorf("%s: documents a title limit of %s, want %d (kbvalidate.maxTitleLen)", path, m[1], maxTitleLen)
		}

		// Reuse the validator's own required-sections list rather than
		// duplicating the heading names here.
		for _, s := range requiredIncidentSections {
			if head := "## " + s.head; !strings.Contains(doc, head) {
				t.Errorf("%s: does not mention required Incident section %q", path, head)
			}
		}
	}
}
