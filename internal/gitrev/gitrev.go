// SPDX-License-Identifier: Apache-2.0

// Package gitrev spells the single rule that tells an IMMUTABLE git revision from
// a moving branch.
//
// It is a package of its own because two packages read the same string from
// opposite ends: internal/config decides what an operator may WRITE
// (catalog.commons.branch vs .ref), internal/catalog decides what the string that
// reaches the syncer MEANS. Held apart, the two copies were identical and nothing
// observed both — a rule that accepted a SHA-256 or upper-case id on the config
// side while the syncer read it as a branch name would have passed every test in
// both suites and left a corpus the operator believes is frozen tracking main.
//
// A leaf package rather than one importing the other: internal/config is what
// every component reads, and pointing it at internal/catalog would drag the search
// index (bleve) and go-git into packages that have no business linking them.
package gitrev

import (
	"encoding/hex"
	"strings"
)

// TagRefPrefix is the fully-qualified namespace of a git tag.
const TagRefPrefix = "refs/tags/"

// IsTagRef reports whether s is a fully-qualified tag ref (refs/tags/<name>).
func IsTagRef(s string) bool {
	return strings.HasPrefix(s, TagRefPrefix)
}

// IsObjectID reports whether s is a full-length git object id (SHA-1, 40 hex
// chars; SHA-256, 64). Abbreviations are deliberately NOT accepted: they are
// ambiguous, and a 7-character hex string is as likely to be a tag name as a
// commit.
func IsObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	// Both accepted lengths are even, so DecodeString's odd-length rejection can
	// never fire here; what is left is exactly "every character is a hex digit",
	// upper or lower case.
	_, err := hex.DecodeString(s)
	return err == nil
}

// IsPinned reports whether s names a revision that no upstream push can move.
// Anything else — a bare name, refs/heads/…, an abbreviated id — tracks.
func IsPinned(s string) bool {
	return IsTagRef(s) || IsObjectID(s)
}

// QualifyTag spells a pin the way git spells it, which is what lets a pin travel
// on the single revision string a syncer already takes rather than on a second
// field. Anything already immutable passes through; a bare name is a tag — the
// only kind of immutable revision that has one.
func QualifyTag(ref string) string {
	if IsPinned(ref) {
		return ref
	}
	return TagRefPrefix + ref
}
