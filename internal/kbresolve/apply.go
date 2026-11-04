// SPDX-License-Identifier: Apache-2.0

package kbresolve

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

const (
	maxEvidenceBytes = 2048
	markerPrefix     = "<!-- runlore-resolution:"
	suggestedHeading = "### Suggested by the investigation"
)

var (
	fixPRRe    = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/pull/[0-9]+$`)
	mergeSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	spaceRe    = regexp.MustCompile(`\s+`)
	headingRe  = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
)

// Validate refuses a resolution that could not be written safely: the fix must
// be a pull-request URL on one line, the merge SHA hex (or empty), and the
// verdict one RunLore itself produces.
func Validate(r providers.AppliedResolution) error {
	switch {
	case !fixPRRe.MatchString(r.FixPR):
		return fmt.Errorf("fix %q is not a pull request URL", r.FixPR)
	case r.MergeSHA != "" && !mergeSHARe.MatchString(r.MergeSHA):
		return fmt.Errorf("merge sha %q is not 7 to 40 hex characters", r.MergeSHA)
	case r.Verdict != "resolved" && r.Verdict != "persisting":
		return fmt.Errorf("verdict %q is neither resolved nor persisting", r.Verdict)
	}
	return nil
}

// FlattenEvidence turns untrusted, multi-line text into one line that cannot
// open a section or carry a marker: whitespace runs collapse to one space, every
// "<!--" and "-->" is stripped until none remain (stripping one can join the
// pieces of another), whitespace collapses again, the result is trimmed, and it
// is cut to 2 KiB on a rune boundary. The cut comes last so the 2 KiB is of what
// is kept.
func FlattenEvidence(s string) string { return cut(strip(s)) }

// strip is FlattenEvidence without the cut.
func strip(s string) string {
	s = spaceRe.ReplaceAllString(s, " ")
	for strings.Contains(s, "<!--") || strings.Contains(s, "-->") {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "<!--", ""), "-->", "")
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func cut(s string) string {
	if len(s) <= maxEvidenceBytes {
		return s
	}
	n := maxEvidenceBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n])
}

// oneLine is the full treatment untrusted text gets before it enters an entry,
// in this order: redact (on the whole text, so a secret is never cut before it
// is recognised), strip comments and flatten to one line, redact again (removing
// a comment can join the halves of a token), defuse images and HTML, and cut to
// 2 KiB last so the bound holds on what is written.
func oneLine(s string) string {
	s = redact.Secrets(strip(redact.Secrets(s)))
	return cut(strings.TrimSpace(spaceRe.ReplaceAllString(okf.DefuseMarkup(s), " ")))
}

func marker(r providers.AppliedResolution) string { return markerFor(r.FixPR, r.Verdict) }

func markerFor(fixPR, verdict string) string {
	sum := sha256.Sum256([]byte(fixPR + "|" + verdict))
	return markerPrefix + hex.EncodeToString(sum[:])[:8] + " -->"
}

func prLink(fixPR string) string {
	return "[#" + fixPR[strings.LastIndex(fixPR, "/")+1:] + "](" + fixPR + ")"
}

// section locates the first "name" heading outside a fence: start is the
// heading's line, end the line of the next heading of the same or a higher
// level (or len(lines)).
func section(lines []string, name string) (start, end int, ok bool) {
	fenced := catalog.FencedLines(lines)
	level := 0
	for i, ln := range lines {
		if fenced[i] {
			continue
		}
		m := headingRe.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		if level == 0 {
			if strings.EqualFold(m[2], name) {
				start, level = i, len(m[1])
			}
		} else if len(m[1]) <= level {
			return start, i, true
		}
	}
	return start, len(lines), level != 0
}

// splice replaces the section's content (what follows its heading) with content.
func splice(body, name, content string) (string, bool) {
	lines := strings.Split(body, "\n")
	start, end, ok := section(lines, name)
	if !ok {
		return "", false
	}
	head := strings.Join(lines[:start+1], "\n")
	out := head + "\n"
	if content != "" {
		out = head + "\n\n" + content + "\n"
	}
	if rest := strings.TrimSpace(strings.Join(lines[end:], "\n")); rest != "" {
		out += "\n" + strings.Join(lines[end:], "\n")
	}
	return out, true
}

func sectionContent(body, name string) (string, bool) {
	lines := strings.Split(body, "\n")
	start, end, ok := section(lines, name)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(strings.Join(lines[start+1:end], "\n")), true
}

// withdrawApplied removes fixPR's Applied record from "## Resolution": its three
// lines and marker, and the "Suggested" heading above the old suggestion, which
// is then the section's content again.
func withdrawApplied(body, fixPR string) string {
	lines := strings.Split(body, "\n")
	start, end, ok := section(lines, "Resolution")
	if !ok {
		return body
	}
	m := markerFor(fixPR, "resolved")
	idx := -1
	for i := start + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) == m {
			idx = i
		}
	}
	if idx < 0 {
		return body
	}
	a, applied := idx, "- **Applied:** "+prLink(fixPR)
	for a > start && !strings.HasPrefix(lines[a], applied) {
		a--
	}
	if a == start {
		a = idx
	}
	b := idx + 1
	for b < end && strings.TrimSpace(lines[b]) == "" {
		b++
	}
	if b < end && strings.TrimSpace(lines[b]) == suggestedHeading {
		for b++; b < end && strings.TrimSpace(lines[b]) == ""; b++ {
		}
	}
	rest := lines[b:]
	if b >= end && end < len(lines) {
		rest = append([]string{""}, rest...)
	}
	return strings.Join(append(lines[:a:a], rest...), "\n")
}

// withdrawUnresolved removes fixPR's "did not hold" bullet and marker.
func withdrawUnresolved(body, fixPR string) string {
	m := markerFor(fixPR, "persisting")
	lines := strings.Split(body, "\n")
	idx := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == m {
			idx = i
		}
	}
	if idx < 0 {
		return body
	}
	a, bullet := idx, "- The fix "+prLink(fixPR)
	for a > 0 && !strings.HasPrefix(lines[a], bullet) {
		a--
	}
	if !strings.HasPrefix(lines[a], bullet) {
		a = idx
	}
	b := idx + 1
	for b < len(lines) && strings.TrimSpace(lines[b]) == "" && a > 0 && strings.TrimSpace(lines[a-1]) == "" {
		b++
	}
	return strings.Join(append(lines[:a:a], lines[b:]...), "\n")
}

// ApplyBody records r in the entry body. It is a no-op when r's marker is
// already there. A resolved verdict rewrites "## Resolution" (or appends it) as
// the three lines recall quotes, then the marker after the blank that ends them
// so the excerpt stops before it, then the old content under a "Suggested"
// heading; a persisting verdict adds one line to "## Unresolved". All text
// copied from r is untrusted and goes through oneLine.
func ApplyBody(body string, r providers.AppliedResolution) string {
	m := marker(r)
	if strings.Contains(body, m) {
		return body
	}
	// A fix has one latest verdict: recording the other one withdraws its record.
	if r.Verdict == "persisting" {
		body = withdrawApplied(body, r.FixPR)
	} else {
		body = withdrawUnresolved(body, r.FixPR)
	}
	if r.Verdict == "persisting" {
		line := "- The fix " + prLink(r.FixPR)
		if r.MergeSHA != "" {
			line += " (merge `" + r.MergeSHA + "`)"
		}
		line += " did not hold"
		if why := strings.TrimRight(oneLine(r.Why), "."); why != "" {
			line += ": " + why
		}
		line += "."
		content := line + "\n\n" + m
		if old, ok := sectionContent(body, "Unresolved"); ok {
			if old != "" {
				content = old + "\n" + line + "\n\n" + m
			}
			out, _ := splice(body, "Unresolved", content)
			return out
		}
		return strings.TrimRight(body, "\n") + "\n\n## Unresolved\n\n" + content + "\n"
	}

	lines := []string{"- **Applied:** " + prLink(r.FixPR)}
	if d := oneLine(r.Diff); d != "" {
		lines = append(lines, "- **Diff:** "+d)
	}
	if e := oneLine(r.Evidence); e != "" {
		lines = append(lines, "- **Soak evidence:** "+e)
	}
	block := strings.Join(lines, "\n") + "\n\n" + m
	old, found := sectionContent(body, "Resolution")
	switch {
	case !found:
		return strings.TrimRight(body, "\n") + "\n\n## Resolution\n\n" + block + "\n"
	case strings.Contains(old, markerPrefix):
		block += "\n\n" + old // an earlier fix's record stays, below the newest
	case old != "":
		block += "\n\n" + suggestedHeading + "\n\n" + old
	}
	out, _ := splice(body, "Resolution", block)
	return out
}

// Apply records r in the entry raw: the `resolution` frontmatter block when the
// fix resolved it (key order kept), or its removal when the same fix is reported
// persisting, then the body. changed is false, and out is raw, when r was already
// recorded.
func Apply(raw []byte, r providers.AppliedResolution) (out []byte, changed bool, err error) {
	if err := Validate(r); err != nil {
		return nil, false, err
	}
	if !okf.HasFrontmatter(raw) {
		return nil, false, fmt.Errorf("the entry has no frontmatter")
	}
	fm, body := catalog.SplitFrontmatter(raw)
	var doc yaml.Node
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil, false, fmt.Errorf("frontmatter: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("frontmatter is not a mapping")
	}
	// A repeat is a no-op when the body already holds this verdict for this fix
	// and the frontmatter agrees; either one out of step is reconciled.
	if strings.Contains(string(body), marker(r)) && resolvedFix(doc.Content[0], r.FixPR) == (r.Verdict == "resolved") {
		return raw, false, nil
	}
	setResolution(doc.Content[0], r)
	enc, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, false, err
	}
	out = []byte("---\n" + string(enc) + "---\n" + ApplyBody(string(body), r))
	return out, true, nil
}

// resolvedFix reports whether the frontmatter's resolution records fixPR.
func resolvedFix(m *yaml.Node, fixPR string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "resolution" {
			continue
		}
		v := m.Content[i+1]
		for j := 0; j+1 < len(v.Content); j += 2 {
			if v.Content[j].Value == "fix_pr" && v.Content[j+1].Value == fixPR {
				return true
			}
		}
	}
	return false
}

// setResolution writes the resolution block for a resolved verdict, or deletes
// the one recording this same fix for a persisting verdict.
func setResolution(m *yaml.Node, r providers.AppliedResolution) {
	idx := -1
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "resolution" {
			idx = i
		}
	}
	if r.Verdict == "persisting" {
		if idx < 0 {
			return
		}
		v := m.Content[idx+1]
		for i := 0; i+1 < len(v.Content); i += 2 {
			if v.Content[i].Value == "fix_pr" && v.Content[i+1].Value == r.FixPR {
				m.Content = append(m.Content[:idx], m.Content[idx+2:]...)
				return
			}
		}
		return
	}
	var val yaml.Node
	_ = val.Encode(struct {
		FixPR      string `yaml:"fix_pr"`
		MergeSHA   string `yaml:"merge_sha,omitempty"`
		Verdict    string `yaml:"verdict"`
		VerifiedAt string `yaml:"verified_at"`
	}{r.FixPR, r.MergeSHA, r.Verdict, r.VerifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00")})
	if idx >= 0 {
		m.Content[idx+1] = &val
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "resolution"}, &val)
}
