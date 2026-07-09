// SPDX-License-Identifier: Apache-2.0

// Package catalog loads and searches an OKF knowledge catalog (a directory of
// markdown files with YAML frontmatter) — the read half of RunLore's Learn pillar.
package catalog

// Entry is one OKF knowledge entry.
type Entry struct {
	Type        string   // frontmatter: type (Playbook, Incident, …)
	Title       string   // frontmatter: title
	Description string   // frontmatter: description
	Resource    string   // frontmatter: resource
	Tags        []string // frontmatter: tags
	Timestamp   string   // frontmatter: timestamp (OKF-recommended, RFC3339; "" when absent)
	Fingerprint string   // frontmatter: fingerprint (curator.DupFingerprint identity; "" on hand-written entries)
	Body        string   // markdown body (after frontmatter)
	Path        string   // file path relative to the bundle root
}
