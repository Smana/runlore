package catalog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Section quotes an entry's Cause/Resolution into chat + PR bodies: first
// paragraph only, one line, bold markers stripped, hard length cap.
func TestEntrySection(t *testing.T) {
	body := `## Decision

- **why keep:** x

## Cause

1. **ConfigMap truncated after kustomize bump** (85%) — change: flux/apps
2. **DNS flake** (10%)

## Resolution

- revert the patch and pin kustomize 5.3.2 (reversible=true)

## Citations

[1] flux/apps
`
	e := Entry{Body: body}
	cases := []struct{ name, want string }{
		{"Cause", "1. ConfigMap truncated after kustomize bump (85%) — change: flux/apps 2. DNS flake (10%)"},
		{"Resolution", "- revert the patch and pin kustomize 5.3.2 (reversible=true)"},
		{"resolution", "- revert the patch and pin kustomize 5.3.2 (reversible=true)"}, // case-insensitive
		{"Symptom", ""}, // absent section
	}
	for _, c := range cases {
		if got := e.Section(c.name); got != c.want {
			t.Errorf("Section(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// The first BLANK line after content ends the excerpt: a second paragraph in
// the same section is not quoted.
func TestEntrySectionFirstParagraphOnly(t *testing.T) {
	e := Entry{Body: "## Cause\n\nfirst para line one\nline two\n\nsecond para\n\n## Next\n\nx\n"}
	if got := e.Section("Cause"); got != "first para line one line two" {
		t.Errorf("Section(Cause) = %q, want first paragraph only", got)
	}
}

func TestEntrySectionTruncates(t *testing.T) {
	e := Entry{Body: "## Cause\n\n" + strings.Repeat("word ", 200) + "\n"}
	got := e.Section("Cause")
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long section not truncated: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 301 {
		t.Fatalf("excerpt is %d runes, want ≤ 301 (300 + ellipsis)", n)
	}
}

func TestEntrySectionMalformed(t *testing.T) {
	cases := []struct{ label, body string }{
		{"empty body", ""},
		{"heading only", "## Cause\n"},
		{"heading then next heading", "## Cause\n\n## Resolution\n\n- x\n"},
	}
	for _, c := range cases {
		if got := (Entry{Body: c.body}).Section("Cause"); got != "" {
			t.Errorf("%s: Section(Cause) = %q, want \"\"", c.label, got)
		}
	}
}

// A human-edited Resolution often carries a fenced command block whose "#"
// comment lines would otherwise parse as headings and truncate the excerpt —
// fences are opaque: skipped entirely, never section boundaries.
func TestEntrySectionSkipsFencedCode(t *testing.T) {
	body := "## Resolution\n\n```bash\n# revert the patch\nkubectl rollout undo deploy/web\n```\n\nRevert the patch and pin 5.3.2.\n\n## Next\n\nx\n"
	if got := (Entry{Body: body}).Section("Resolution"); got != "Revert the patch and pin 5.3.2." {
		t.Errorf("Section(Resolution) = %q, want the prose after the fence", got)
	}
	// A section that is ONLY a fence has nothing quotable.
	only := "## Resolution\n\n```\nkubectl get pods\n```\n\n## Next\n\nx\n"
	if got := (Entry{Body: only}).Section("Resolution"); got != "" {
		t.Errorf("fence-only section = %q, want \"\"", got)
	}
	// A "## heading" line inside a fence in an EARLIER section must not
	// derail which section is matched.
	earlier := "## Cause\n\n```\n## Resolution\n```\n\ncause text\n\n## Resolution\n\nreal resolution\n"
	if got := (Entry{Body: earlier}).Section("Resolution"); got != "real resolution" {
		t.Errorf("Section(Resolution) with fenced fake heading = %q, want %q", got, "real resolution")
	}
}
