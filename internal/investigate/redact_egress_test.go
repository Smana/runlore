// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestRedactInvestigation locks down the egress catch-all: secrets in any of a
// finished investigation's human-facing fields are masked before delivery to chat
// or a (possibly public) KB PR — even if they reached the finding via a non-model
// path (so ingress redaction wouldn't have seen them).
func TestRedactInvestigation(t *testing.T) {
	inv := &providers.Investigation{
		Title: "DB down: password=hunter2horse",
		RootCauses: []providers.Hypothesis{{
			Summary:         "leaked token ghp_0123456789abcdefghijABCDEFGHIJ0123",
			Evidence:        []string{"controller log: token xoxb-123456789012-abcdefuvwxyz"},
			SuggestedAction: "rotate key AKIAIOSFODNN7EXAMPLE",
		}},
		Unresolved: []string{"DB_SECRET=s3cr3t-value-xyz seen in events"},
		Actions: []providers.Action{{
			Name:        "suspend (password=hunter2horse)", // buildInvestigation copies the description into Name
			Description: "suspend (OPENAI_API_KEY=sk-abcdefghijklmnopqrst)",
		}},
	}
	redactInvestigation(inv)

	blob := strings.Join([]string{
		inv.Title, inv.RootCauses[0].Summary, inv.RootCauses[0].Evidence[0],
		inv.RootCauses[0].SuggestedAction, inv.Unresolved[0], inv.Actions[0].Name, inv.Actions[0].Description,
	}, "|")
	for _, secret := range []string{
		"hunter2horse", "ghp_0123456789abcdefghijABCDEFGHIJ0123", "xoxb-123456789012-abcdefuvwxyz",
		"AKIAIOSFODNN7EXAMPLE", "s3cr3t-value-xyz", "sk-abcdefghijklmnopqrst",
	} {
		if strings.Contains(blob, secret) {
			t.Fatalf("secret survived egress redaction: %q", secret)
		}
	}
	if !strings.Contains(blob, "[REDACTED]") {
		t.Fatalf("expected redaction markers, got %q", blob)
	}
}

const (
	// secretVal is a secret-shaped value the redactor is known to mask (the generic
	// key=value rule). EVERY exported string reachable from the fixture below is set
	// to it, so the reflection walk always has something detectable to scrub.
	secretVal = "password=hunter2horse"
	// secretMarker is the part of secretVal that must not survive redaction. The
	// "password=" half does survive (only the value is masked), so containment has to
	// be tested against this half alone.
	secretMarker = "hunter2horse"
	// maxRedactWalkDepth bounds both reflection walks below. Investigation's shape is
	// ~5 levels deep; a self-referential type added later would otherwise hang the
	// suite instead of failing it.
	maxRedactWalkDepth = 16
)

// egressVerbatim is this guard's OWN expectation of what egress redaction leaves
// untouched, keyed by the FULL field path from the Investigation root (slice, array
// and map elements do not add an index — every element of a slice sits at its
// field's path).
//
// It is hand-written on purpose. The previous version of this test pruned its walk
// with redactInvestigation's own redactionSkipField map and skipped the same struct
// types as redactionSkipTypes, so it exempted precisely what the redactor exempted:
// it agreed with the redactor by construction and could not, even in principle,
// observe a skip-list entry firing on the wrong field. That is how a skip-list keyed
// by the bare name "Path" — added for MatchedEntry.Path, a catalog path — silently
// also spared Change.Source.Path, which internal/providers/cloud/aws/cloudtrail.go
// packs a verbatim CloudTrail ErrorMessage into.
//
// So: a path here is a claim that the value is SERVER-DERIVED and must reach chat
// and the (possibly public) KB verbatim. Everything else must come back scrubbed.
// Adding an entry means writing the qualified path out where a reviewer can see
// which struct it belongs to.
var egressVerbatim = map[string]bool{
	// Investigation-level server-derived identity and links.
	"Investigation.Verdict":        true, // server-controlled classification enum
	"Investigation.CuratedURL":     true, // curator-set KB link
	"Investigation.PrevCuratedURL": true, // curator-set KB link (prior occurrence)
	"Investigation.RecalledEntry":  true, // catalog path the answer was recalled from
	"Investigation.Fingerprint":    true, // deterministic alert dedup id
	"Investigation.Fingerprints":   true, // coalesced batch dedup ids
	"Investigation.TriggerKey":     true, // deterministic incident-identity dedup key
	// Catalog paths and links of matched/prior entries.
	"Investigation.Prior.EntryPath":       true,
	"Investigation.MatchedKnowledge.Path": true,
	"Investigation.MatchedKnowledge.URL":  true,
	// Server-controlled action vocabulary and approval token.
	"Investigation.Actions.Op":         true,
	"Investigation.Actions.ApprovalID": true,
	// providers.Workload is a Kubernetes resource identifier the executor acts on,
	// never free text — spelled out per reachable occurrence rather than waved
	// through by type, so a new Workload-shaped field cannot be exempted silently.
	"Investigation.Resource.Kind":                 true,
	"Investigation.Resource.Name":                 true,
	"Investigation.Resource.Namespace":            true,
	"Investigation.AlertResource.Kind":            true,
	"Investigation.AlertResource.Name":            true,
	"Investigation.AlertResource.Namespace":       true,
	"Investigation.Actions.Target.Kind":           true,
	"Investigation.Actions.Target.Name":           true,
	"Investigation.Actions.Target.Namespace":      true,
	"Investigation.Changes.Workload.Kind":         true,
	"Investigation.Changes.Workload.Name":         true,
	"Investigation.Changes.Workload.Namespace":    true,
	"Investigation.Changes.BlastRadius.Kind":      true,
	"Investigation.Changes.BlastRadius.Name":      true,
	"Investigation.Changes.BlastRadius.Namespace": true,
}

// TestRedactInvestigationCoversEveryReachableString is the #197 guard, rebuilt so it
// can actually see what it is guarding.
//
// It builds an Investigation whose every exported string — through pointers, slices
// and maps — is set to a secret-shaped value, BY REFLECTION rather than by hand, so
// a string field added anywhere in the shape is exercised the day it is added. It
// then runs the egress redaction and walks the result comparing each path against
// egressVerbatim above, an expectation written independently of the redactor's own
// skip-list. A field that is neither scrubbed nor declared server-derived fails
// here, which is what makes "the single egress chokepoint" a checked claim rather
// than a comment.
func TestRedactInvestigationCoversEveryReachableString(t *testing.T) {
	inv := &providers.Investigation{}
	populateStrings(t, "Investigation", reflect.ValueOf(inv).Elem(), 0)

	// The fixture must genuinely reach through a pointer, a slice, and nested structs
	// before the redaction assertions mean anything. These are the paths a hand-written
	// fixture is most likely to miss — Changes.Source.Path is the one it did miss.
	populated := walkStrings(t, inv)
	for _, path := range []string{
		"Investigation.Prior.Cause",              // through a nil pointer
		"Investigation.RootCauses.Evidence",      // slice of strings inside a slice of structs
		"Investigation.Changes.Source.Path",      // nested struct inside a slice
		"Investigation.Changes.BlastRadius.Name", // slice of structs inside a slice of structs
		"Investigation.MatchedKnowledge.Title",   // through a nil pointer
	} {
		if got := populated[path]; got != secretVal {
			t.Fatalf("fixture did not populate %s (got %q) — the coverage claim below is void", path, got)
		}
	}

	redactInvestigation(inv)

	got := walkStrings(t, inv)
	for path, val := range got {
		switch {
		case egressVerbatim[path]:
			if val != secretVal {
				t.Errorf("%s is declared server-derived but was scrubbed to %q — "+
					"drop it from egressVerbatim, or add it to redactionSkipField", path, val)
			}
		case strings.Contains(val, secretMarker):
			t.Errorf("%s survived egress redaction: %q — either redact it, or (if it really is "+
				"server-derived) add the QUALIFIED path to redactionSkipField and to egressVerbatim", path, val)
		}
	}
	for path := range egressVerbatim {
		if _, ok := got[path]; !ok {
			t.Errorf("egressVerbatim claims %s is server-derived, but no such string is reachable "+
				"from Investigation — stale entry, or the field was renamed", path)
		}
	}
}

// populateStrings sets every exported string reachable from v to secretVal,
// allocating nil pointers and giving every empty slice and map one element. The
// fixture is therefore exhaustive BY CONSTRUCTION: nobody has to remember to extend
// a literal struct when a field is added, which is exactly how the previous
// hand-written fixture left Change.Source.Path unexercised.
//
// Map KEYS are deliberately left non-secret: redactStrings rewrites map values, not
// keys, so a secret-shaped key would assert a property the redactor does not claim.
func populateStrings(t *testing.T, path string, v reflect.Value, depth int) {
	t.Helper()
	if depth > maxRedactWalkDepth {
		t.Fatalf("fixture walk exceeded depth %d at %s — a self-referential type?", maxRedactWalkDepth, path)
	}
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(secretVal)
		}
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		populateStrings(t, path, v.Elem(), depth+1)
	case reflect.Struct:
		tp := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if tp.Field(i).PkgPath != "" { // unexported: never settable
				continue
			}
			populateStrings(t, path+"."+tp.Field(i).Name, v.Field(i), depth+1)
		}
	case reflect.Slice:
		if v.Len() == 0 {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}
		for i := 0; i < v.Len(); i++ {
			populateStrings(t, path, v.Index(i), depth+1)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			populateStrings(t, path, v.Index(i), depth+1)
		}
	case reflect.Map:
		if !v.CanSet() {
			return
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		key := reflect.New(v.Type().Key()).Elem()
		if key.Kind() == reflect.String {
			key.SetString("k")
		}
		val := reflect.New(v.Type().Elem()).Elem()
		populateStrings(t, path, val, depth+1)
		v.SetMapIndex(key, val)
	}
}

// walkStrings collects every exported string reachable from inv, keyed by its full
// field path. It prunes NOTHING — no skip-list, no skipped types — which is the
// whole point: the expectation lives in egressVerbatim, a table this walk cannot
// influence, so a redactor skip-list entry that fires on the wrong field shows up
// as a mismatch instead of being mirrored into agreement.
func walkStrings(t *testing.T, inv *providers.Investigation) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(path string, v reflect.Value, depth int)
	walk = func(path string, v reflect.Value, depth int) {
		if depth > maxRedactWalkDepth {
			t.Fatalf("verification walk exceeded depth %d at %s — a self-referential type?", maxRedactWalkDepth, path)
		}
		switch v.Kind() {
		case reflect.String:
			out[path] = v.String()
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(path, v.Elem(), depth+1)
			}
		case reflect.Struct:
			tp := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if tp.Field(i).PkgPath != "" {
					continue
				}
				walk(path+"."+tp.Field(i).Name, v.Field(i), depth+1)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(path, v.Index(i), depth+1)
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(path, v.MapIndex(k), depth+1)
			}
		}
	}
	walk("Investigation", reflect.ValueOf(inv).Elem(), 0)
	return out
}
