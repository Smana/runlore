// SPDX-License-Identifier: Apache-2.0

package github

import "testing"

// TestScalarAndComment is the mutation guard for the frontmatter value reader both
// stamps depend on. Getting it wrong is not cosmetic: a comment read as part of the
// value makes a perfectly good date unparseable, which the revalidation stamp reads
// as "nothing on record" — so the anti-spam gate never fires and the entry is
// restamped on every sweep. Reading a comment where there is none is the mirror
// failure: it would truncate real data.
func TestScalarAndComment(t *testing.T) {
	cases := []struct {
		name, in, scalar, comment string
	}{
		{"no comment", " 2026-07-20", " 2026-07-20", ""},
		{"comment after a space", " 2026-07-20 # confirmed by alice", " 2026-07-20", " # confirmed by alice"},
		{"comment after several spaces", " 2026-07-20   # note", " 2026-07-20", "   # note"},
		{"comment after a tab", " 2026-07-20\t# note", " 2026-07-20", "\t# note"},
		// A '#' glued to the value is part of it — YAML only opens a comment
		// after whitespace.
		{"hash with no leading space is data", " ns#1", " ns#1", ""},
		{"hash inside a double-quoted scalar is data", ` "alert #42 fired"`, ` "alert #42 fired"`, ""},
		{"hash inside a single-quoted scalar is data", ` 'alert #42'`, ` 'alert #42'`, ""},
		{"comment after a quoted scalar still splits", ` "2026-07-20" # note`, ` "2026-07-20"`, " # note"},
		{"whole value is a comment", " # nothing here", "", " # nothing here"},
		{"empty value", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scalar, comment := scalarAndComment(tc.in)
			if scalar != tc.scalar || comment != tc.comment {
				t.Errorf("scalarAndComment(%q) = (%q, %q), want (%q, %q)",
					tc.in, scalar, comment, tc.scalar, tc.comment)
			}
			// The split must be lossless, or a "surgical" edit silently drops bytes.
			if scalar+comment != tc.in {
				t.Errorf("split is lossy: %q + %q != %q", scalar, comment, tc.in)
			}
		})
	}
}

// TestInactiveStatus pins the reading of `status` to what a YAML reader — and
// therefore recall's entryActive — sees. A stamp that disagreed would edit an entry
// the catalog has already written off.
func TestInactiveStatus(t *testing.T) {
	inactive := []string{" retired", " draft", ` "retired"`, ` 'draft'`, " Retired", " DRAFT", "  retired  "}
	for _, s := range inactive {
		if !inactiveStatus(s) {
			t.Errorf("inactiveStatus(%q) = false, want true", s)
		}
	}
	// An absent or foreign status stays active (OKF §9 tolerance), so pre-status
	// catalogs behave exactly as before.
	active := []string{"", " active", " published", " retiring", " drafted"}
	for _, s := range active {
		if inactiveStatus(s) {
			t.Errorf("inactiveStatus(%q) = true, want false", s)
		}
	}
}

// TestFrontmatterValue pins the key/value split the stamps scan with.
func TestFrontmatterValue(t *testing.T) {
	key, scalar, comment, ok := frontmatterValue(`last_validated: "2026-07-20T08:00:00Z" # kept`)
	if !ok || key != "last_validated" || scalar != ` "2026-07-20T08:00:00Z"` || comment != " # kept" {
		t.Fatalf("got key=%q scalar=%q comment=%q ok=%v", key, scalar, comment, ok)
	}
	// An RFC3339 value contains colons; only the FIRST one separates the key.
	if _, s, _, _ := frontmatterValue("timestamp: 2026-07-20T08:00:00Z"); s != " 2026-07-20T08:00:00Z" {
		t.Errorf("scalar=%q, want the whole value past the first colon", s)
	}
	if _, _, _, ok := frontmatterValue("a line with no colon"); ok {
		t.Error("a keyless line must report ok=false")
	}
}
