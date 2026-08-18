// SPDX-License-Identifier: Apache-2.0

// Package okf serializes providers.KBEntry values as OKF markdown files
// (YAML frontmatter + body). It is the single write-side counterpart of
// catalog.Load: the GitHub forge and `lore kb import` both render through it,
// so every entry RunLore writes parses back identically.
package okf

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Meta carries the file-level frontmatter that is not part of a drafted
// KBEntry: the forge stamps Timestamp at render time; import preserves the
// source document's own timestamp/status/last_validated instead.
type Meta struct {
	Timestamp     string // OKF-recommended; RFC3339 or bare date
	Status        string // lifecycle: "", active, retired, draft
	LastValidated string // date a human last confirmed the entry works
}

// frontmatter is the YAML frontmatter of an OKF entry. Marshaled (not string-
// formatted) so a newline-bearing title/description from LLM output can't
// inject extra frontmatter keys. Keys mirror catalog.entryMeta (the loader).
type frontmatter struct {
	Type          string   `yaml:"type"`
	Title         string   `yaml:"title"`
	Description   string   `yaml:"description"`
	Resource      string   `yaml:"resource,omitempty"`
	AlertResource string   `yaml:"alert_resource,omitempty"`
	Tags          []string `yaml:"tags,omitempty"`
	Timestamp     string   `yaml:"timestamp,omitempty"`
	Status        string   `yaml:"status,omitempty"`
	LastValidated string   `yaml:"last_validated,omitempty"`
	Fingerprint   string   `yaml:"fingerprint,omitempty"`
	Confidence    float64  `yaml:"confidence,omitempty"`
	Provenance    []string `yaml:"provenance,omitempty"`
}

// Render serializes a KBEntry as OKF markdown. The body is written verbatim —
// callers that render untrusted (LLM-authored) bodies sanitize BEFORE calling
// (the GitHub forge neutralizes image markdown); a human runbook being
// imported must survive byte-for-byte.
func Render(e providers.KBEntry, m Meta) string {
	fm, _ := yaml.Marshal(frontmatter{
		Type: e.Type, Title: e.Title, Description: e.Description, Resource: e.Resource,
		AlertResource: e.AlertResource, Tags: e.Tags,
		Timestamp: m.Timestamp, Status: m.Status, LastValidated: m.LastValidated,
		Fingerprint: e.Fingerprint, Confidence: e.Confidence, Provenance: e.Provenance,
	})
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n")
	b.WriteString(e.Body)
	b.WriteString("\n")
	return b.String()
}

// AsEntry adapts a KBEntry to the catalog.Entry the loader produces — the
// read-side twin of Render, and the ONE projection every caller that must judge
// an entry it has not yet written shares.
//
// It exists because that projection kept being written out by hand. Each copy
// carried a slightly different subset of the fields Render actually writes, so
// each answered a slightly different question than the merge gate would: the
// curator's copy dropped the file-level metadata, `lore kb import`'s dropped
// alert_resource and fingerprint, and nothing made either of them wrong until an
// entry that turned on a dropped field reached the catalog repo's CI days later.
// Rendering and reading are one field list, so they get one adapter;
// TestAsEntryIsWhatTheLoaderWouldProduce holds it to the real loader's output
// rather than to a restated list.
//
// path is the entry's bundle-relative destination, and is the entry's identity in
// the catalog. A draft that does not have one yet passes "" — the validator does
// not read Path, and inventing one would be a claim about where the entry landed.
func AsEntry(e providers.KBEntry, m Meta, path string) catalog.Entry {
	return catalog.Entry{
		Type: e.Type, Title: e.Title, Description: e.Description,
		Resource: e.Resource, AlertResource: e.AlertResource, Tags: e.Tags,
		Timestamp: m.Timestamp, Status: m.Status, LastValidated: m.LastValidated,
		Fingerprint: e.Fingerprint, Body: e.Body, Path: path,
	}
}

// Slugify lowercases s and collapses every non-[a-z0-9] run into one dash.
// (Moved verbatim from internal/forge/github.)
func Slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
