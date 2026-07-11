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

// secretVal is a secret-shaped value the redactor is known to mask (the generic
// key=value rule). Every free-text string in the fully-populated fixture below is
// set to this so the reflection walk has something detectable to scrub.
const secretVal = "password=hunter2horse"

// TestRedactInvestigationReflectionCoverage is the #197 guard: it builds an
// Investigation with EVERY exported string set to a secret-shaped value, runs the
// egress redaction, then reflectively asserts that every exported string carries
// the scrub marker — EXCEPT the server-derived skip-list. Adding a new
// model-authored string field without redaction fails HERE (not a future audit),
// because the fixture-builder sets it to secretVal and this walk finds it
// un-scrubbed. It also explicitly pins the previously-missed fields (RuledOut,
// DataGaps, Hypothesis.ChangeRef).
func TestRedactInvestigationReflectionCoverage(t *testing.T) {
	inv := &providers.Investigation{
		Title:      secretVal,
		RootCauses: []providers.Hypothesis{{Summary: secretVal, ChangeRef: secretVal, Evidence: []string{secretVal}, SuggestedAction: secretVal}},
		Changes:    []providers.Change{{ManagedBy: secretVal, FromRev: secretVal, ToRev: secretVal, DiffRef: secretVal}},
		Unresolved: []string{secretVal},
		RuledOut:   []string{secretVal},
		DataGaps:   []string{secretVal},
		Verdict:    providers.Verdict(secretVal),                                               // skip-listed: server-controlled enum, kept verbatim
		Resource:   providers.Workload{Kind: secretVal, Name: secretVal, Namespace: secretVal}, // skip-listed type
		// Trigger-time facts (free text derived from untrusted alert labels) — scrubbed.
		Severity:    secretVal,
		Environment: secretVal,
		Cluster:     secretVal,
		Tenant:      secretVal,
		AlertName:   secretVal,
		Actions: []providers.Action{{
			Name: secretVal, Description: secretVal,
			Op:         secretVal,                           // skip-listed enum
			ApprovalID: secretVal,                           // skip-listed server id
			Target:     providers.Workload{Name: secretVal}, // skip-listed type
		}},
		// Server-derived identity/links — all skip-listed, must stay verbatim.
		CuratedURL:     secretVal,
		Fingerprint:    secretVal,
		Fingerprints:   []string{secretVal},
		TriggerKey:     secretVal,
		RecalledEntry:  secretVal,
		PrevCuratedURL: secretVal,
		Prior:          &providers.PriorKnowledge{Cause: secretVal, Resolution: secretVal, EntryPath: secretVal}, // EntryPath skip-listed
		MatchedKnowledge: &providers.MatchedEntry{
			Path: secretVal, URL: secretVal, // both skip-listed
			Title: secretVal, // NOT skip-listed: it is KB entry text → scrubbed
		},
	}

	redactInvestigation(inv)

	// Spot-check the audit's named gaps first, with clear failure messages.
	if strings.Contains(inv.RuledOut[0], "hunter2horse") {
		t.Error("RuledOut was not scrubbed (the #197 gap)")
	}
	if strings.Contains(inv.DataGaps[0], "hunter2horse") {
		t.Error("DataGaps was not scrubbed (the #197 gap)")
	}
	if strings.Contains(inv.RootCauses[0].ChangeRef, "hunter2horse") {
		t.Error("Hypothesis.ChangeRef was not scrubbed (the #197 gap)")
	}

	// The skip-list must remain verbatim (server-derived).
	for name, got := range map[string]string{
		"Verdict":               string(inv.Verdict),
		"Resource.Name":         inv.Resource.Name,
		"CuratedURL":            inv.CuratedURL,
		"PrevCuratedURL":        inv.PrevCuratedURL,
		"Fingerprint":           inv.Fingerprint,
		"Fingerprints[0]":       inv.Fingerprints[0],
		"TriggerKey":            inv.TriggerKey,
		"RecalledEntry":         inv.RecalledEntry,
		"Prior.EntryPath":       inv.Prior.EntryPath,
		"MatchedKnowledge.Path": inv.MatchedKnowledge.Path,
		"MatchedKnowledge.URL":  inv.MatchedKnowledge.URL,
		"Action.Op":             inv.Actions[0].Op,
		"Action.ApprovalID":     inv.Actions[0].ApprovalID,
		"Action.Target.Name":    inv.Actions[0].Target.Name,
	} {
		if got != secretVal {
			t.Errorf("skip-listed server-derived field %s was scrubbed (must stay verbatim): %q", name, got)
		}
	}

	// Total coverage: reflectively walk the redacted investigation and assert that
	// every exported string reachable OUTSIDE the skip-list no longer carries the
	// secret. Skip subtrees the redactor deliberately spares (Workload type,
	// skip-listed field names) so this mirrors the redactor's contract exactly.
	var walk func(path string, v reflect.Value)
	walk = func(path string, v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			if strings.Contains(v.String(), "hunter2horse") {
				t.Errorf("exported string %s survived egress redaction: %q — add it to the redactor or the skip-list", path, v.String())
			}
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(path, v.Elem())
			}
		case reflect.Struct:
			if v.Type() == reflect.TypeOf(providers.Workload{}) {
				return // skip-listed type
			}
			tp := v.Type()
			for i := 0; i < v.NumField(); i++ {
				f := tp.Field(i)
				if f.PkgPath != "" || redactionSkipField[f.Name] {
					continue
				}
				walk(path+"."+f.Name, v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(path, v.Index(i))
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(path, v.MapIndex(k))
			}
		}
	}
	walk("Investigation", reflect.ValueOf(inv).Elem())
}
