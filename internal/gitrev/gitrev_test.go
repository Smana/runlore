// SPDX-License-Identifier: Apache-2.0

package gitrev

import (
	"strings"
	"testing"
)

const (
	sha1Hex   = "1a2b3c4d5e6f70819293a4b5c6d7e8f901234567"                         // 40
	sha256Hex = "1a2b3c4d5e6f70819293a4b5c6d7e8f9012345671a2b3c4d5e6f708192939495" // 64
)

// TestRevisionSpellings is the one corpus both ends of the contract are read
// against: internal/config decides what an operator may write, internal/catalog
// decides what the written string means, and both now answer from the functions
// exercised here. Every row is a spelling an operator can actually type.
func TestRevisionSpellings(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		s                        string
		tagRef, objectID, pinned bool
		qualified                string
	}{
		{name: "empty", s: "", qualified: "refs/tags/"},
		{name: "bare branch name", s: "main", qualified: "refs/tags/main"},
		{name: "branch ref", s: "refs/heads/main", qualified: "refs/tags/refs/heads/main"},
		{name: "remote branch ref", s: "refs/remotes/origin/main", qualified: "refs/tags/refs/remotes/origin/main"},
		{name: "bare tag name", s: "v1.2.0", qualified: "refs/tags/v1.2.0"},
		{name: "tag ref", s: "refs/tags/v1.2.0", tagRef: true, pinned: true, qualified: "refs/tags/v1.2.0"},
		{name: "sha-1 id", s: sha1Hex, objectID: true, pinned: true, qualified: sha1Hex},
		// Upper case is what `git rev-parse` output pasted from some tools looks like,
		// and a rule that accepts it in config but reads it as a branch name in the
		// syncer is the failure this package exists to make impossible.
		{name: "sha-1 id upper case", s: strings.ToUpper(sha1Hex), objectID: true, pinned: true, qualified: strings.ToUpper(sha1Hex)},
		{name: "sha-1 id mixed case", s: "1A2b3C4d5E6f70819293A4b5C6d7E8f901234567", objectID: true, pinned: true, qualified: "1A2b3C4d5E6f70819293A4b5C6d7E8f901234567"},
		// SHA-256 object ids: git repositories can be created with them today, and a
		// 64-hex pin that only one side recognises would be cloned as refs/heads/<64 hex>.
		{name: "sha-256 id", s: sha256Hex, objectID: true, pinned: true, qualified: sha256Hex},
		{name: "sha-256 id upper case", s: strings.ToUpper(sha256Hex), objectID: true, pinned: true, qualified: strings.ToUpper(sha256Hex)},
		// Abbreviations and near-misses are ambiguous, so they are tag names.
		{name: "abbreviated id", s: "1a2b3c4", qualified: "refs/tags/1a2b3c4"},
		{name: "half an id", s: "1a2b3c4d5e6f7081", qualified: "refs/tags/1a2b3c4d5e6f7081"},
		{name: "one char short of sha-1", s: sha1Hex[:39], qualified: "refs/tags/" + sha1Hex[:39]},
		{name: "one char past sha-1", s: sha1Hex + "8", qualified: "refs/tags/" + sha1Hex + "8"},
		{name: "one char short of sha-256", s: sha256Hex[:63], qualified: "refs/tags/" + sha256Hex[:63]},
		{name: "40 chars but not hex", s: strings.Repeat("g", 40), qualified: "refs/tags/" + strings.Repeat("g", 40)},
		{name: "40 chars with a separator", s: "1a2b3c4d5e6f70819293a4b5c6d7e8f9012345-7", qualified: "refs/tags/1a2b3c4d5e6f70819293a4b5c6d7e8f9012345-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTagRef(tc.s); got != tc.tagRef {
				t.Errorf("IsTagRef(%q) = %v, want %v", tc.s, got, tc.tagRef)
			}
			if got := IsObjectID(tc.s); got != tc.objectID {
				t.Errorf("IsObjectID(%q) = %v, want %v", tc.s, got, tc.objectID)
			}
			if got := IsPinned(tc.s); got != tc.pinned {
				t.Errorf("IsPinned(%q) = %v, want %v", tc.s, got, tc.pinned)
			}
			if got := QualifyTag(tc.s); got != tc.qualified {
				t.Errorf("QualifyTag(%q) = %q, want %q", tc.s, got, tc.qualified)
			}
		})
	}
}
