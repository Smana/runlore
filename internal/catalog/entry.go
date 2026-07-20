// SPDX-License-Identifier: Apache-2.0

// Package catalog loads and searches an OKF knowledge catalog (a directory of
// markdown files with YAML frontmatter) — the read half of RunLore's Learn pillar.
package catalog

// Entry is one OKF knowledge entry.
type Entry struct {
	Type        string // frontmatter: type (Playbook, Incident, …)
	Title       string // frontmatter: title
	Description string // frontmatter: description
	Resource    string // frontmatter: resource — the affected resource (where the fault was found)
	// AlertResource is frontmatter: alert_resource — the resource the ORIGINATING ALERT
	// fired on, when it differs from Resource. Empty on hand-written entries and on every
	// entry curated before this field existed; the structural gate treats it as an
	// ADDITIONAL way to agree, never a replacement, so those entries behave exactly as before.
	AlertResource string
	Tags          []string // frontmatter: tags
	Timestamp     string   // frontmatter: timestamp (OKF-recommended, RFC3339; "" when absent)
	Fingerprint   string   // frontmatter: fingerprint (curator.DupFingerprint identity; "" on hand-written entries)
	// Status is frontmatter: status — the entry's lifecycle state ("", "active",
	// "retired", "draft", or any foreign value). Recall treats anything other than
	// retired/draft as active (OKF §9: consumers tolerate unknown vocabulary), so
	// absent-or-unknown behaves exactly as before the field existed.
	Status string
	// LastValidated is frontmatter: last_validated — when a human last confirmed the
	// entry still works (date or RFC3339; "" when absent). Kept as the raw string,
	// like Timestamp: the loader stays tolerant, consumers parse.
	LastValidated string
	Body          string // markdown body (after frontmatter)
	Path          string // file path relative to the bundle root
}
