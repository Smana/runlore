// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/providers"
)

// Every ceiling in this file is written as a LITERAL, and every assertion states
// the number instead of reading it back out of the constant it is checking — the
// same rule post_bound_test.go states, for the same reason: this package has
// already shipped a byte ceiling asserted against itself, which could be tripled
// with the suite still green.

// deliveredMatrixContent runs a real Matrix.Deliver against a fake homeserver and
// returns the event content exactly as it left the process. Nothing here is
// composed by the test: the stamp, the legacy trigger field and both bodies are
// whatever Deliver actually put on the wire.
func deliveredMatrixContent(t *testing.T, inv providers.Investigation) map[string]any {
	t.Helper()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"event_id":"$1"}`))
	}))
	t.Cleanup(srv.Close)

	if err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if raw == nil {
		t.Fatal("no event reached the homeserver")
	}
	var content map[string]any
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("the delivered event is not JSON: %v", err)
	}
	return content
}

// canonicalEventBytes is the size the HOMESERVER measures the event at, which is
// not the size Go put on the wire.
//
// The Matrix spec caps an event at 65,536 bytes "encoded as Canonical JSON", and
// canonical JSON escapes only what JSON requires — the quote, the backslash and
// the control characters below 0x20. Go's encoding/json additionally escapes
// "<", ">" and "&" as six-byte \u003c-style escapes by default, which would make
// an HTML formatted_body measure about twice its canonical size here and cut the
// card for a limit no homeserver applies. SetEscapeHTML(false) is what makes this
// the homeserver's arithmetic rather than Go's.
func canonicalEventBytes(t *testing.T, content map[string]any) int {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(content); err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	return buf.Len() - len("\n") // Encode appends a newline that no event carries
}

// deliveredStamp returns the io.runlore.thread stamp Deliver put on the event,
// decoded the same way contextFromContent decodes it off the wire.
func deliveredStamp(t *testing.T, inv providers.Investigation) threadStamp {
	t.Helper()
	raw, ok := deliveredMatrixContent(t, inv)[threadContentField]
	if !ok {
		t.Fatal("Deliver stamped no io.runlore.thread field")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-marshal stamp: %v", err)
	}
	var stamp threadStamp
	if err := json.Unmarshal(b, &stamp); err != nil {
		t.Fatalf("decode stamp: %v", err)
	}
	return stamp
}

// pathologicalInvestigation is a finding whose every stamped field is far past
// any event's budget. Not a contrivance: buildInvestigation
// (internal/investigate/tools.go) copies every string out of submit_findings
// verbatim with no length cap, Resource.Ref() renders model-written free text,
// and TriggerKey is assembled from untrusted alert labels.
func pathologicalInvestigation() providers.Investigation {
	inv := hugeInvestigation()
	inv.Title = strings.Repeat("the harbor-db pod is CrashLoopBackOff ", 2000)
	inv.TriggerKey = strings.Repeat("k", 4096)
	inv.CuratedURL = "https://forge.example/o/r/pull/" + strings.Repeat("9", 4096)
	inv.RecalledEntry = strings.Repeat("catalog/entry/path/", 400)
	inv.Resource = providers.Workload{Namespace: "tooling", Name: strings.Repeat("harbor-", 2000)}
	inv.Verdict = providers.Verdict(strings.Repeat("action_required ", 500))
	return inv
}

// TestMatrixDeliverEventFitsTheSpecCeiling is the defect itself, stated as the
// only assertion that actually matters: the WHOLE event, not any one field of it.
//
// Deliver bounded its two bodies and then assigned an unbounded stamp beside
// them, so a 64 KiB model title produced a 78 KiB stamp next to a 33 KiB pair of
// bodies — a 115 KiB event against a 65,536-byte spec ceiling, rejected outright
// by the homeserver. That is the exact silent-delivery failure the body bound was
// written to prevent, reached one field to the right of it.
//
// The budget below is the whole event minus what Deliver does not write. The
// homeserver adds the federation envelope — room_id, sender, origin_server_ts,
// depth, prev_events, auth_events — plus the content hash and one signature per
// participating server, and none of that is visible from here. 24,576 bytes is
// held back for it, an order of magnitude more than a real envelope costs,
// because guessing low is a rejected event while guessing high only costs card
// text.
func TestMatrixDeliverEventFitsTheSpecCeiling(t *testing.T) {
	content := deliveredMatrixContent(t, pathologicalInvestigation())
	if got := canonicalEventBytes(t, content); got > 40_960 {
		t.Errorf("the delivered event is %d canonical bytes, over the 40,960 the content may claim "+
			"of a 65,536-byte event with 24,576 held back for the federation envelope, hashes and "+
			"signatures — a homeserver rejects the whole event, so an investigation that ran and "+
			"reached a verdict is one nobody is ever told about", got)
	}
}

// TestMatrixDeliverEventFitsWhenEveryFieldIsEscapeHeavy is the same ceiling
// against the input that makes a bound measured on the SOURCE useless.
//
// A byte ceiling counted on the composed string is not a ceiling on the event:
// canonical JSON renders one control character as a six-byte \u001b escape,
// so 16,384 source bytes of ESC arrive as 98,304 — and the two bodies
// alone then come to three times the whole spec ceiling while each still measures
// exactly at its documented limit. ESC is not a hypothetical byte either: it is
// what a container log line carrying ANSI colour codes puts into evidence text,
// which Format splices into both bodies verbatim.
func TestMatrixDeliverEventFitsWhenEveryFieldIsEscapeHeavy(t *testing.T) {
	for name, filler := range map[string]string{
		"ansi escape":  "\x1b",
		"double quote": `"`,
		"backslash":    `\`,
	} {
		inv := pathologicalInvestigation()
		inv.Title = strings.Repeat(filler, 100_000)
		inv.RootCauses[0].Summary = strings.Repeat(filler, 100_000)
		content := deliveredMatrixContent(t, inv)
		if got := canonicalEventBytes(t, content); got > 40_960 {
			t.Errorf("%s: the delivered event is %d canonical bytes, over 40,960 — the bound was "+
				"measured on the string as composed rather than on the escaped bytes the homeserver "+
				"counts", name, got)
		}
	}
}

// TestMatrixStampCeilingsHaveTheirDocumentedValues pins the numbers and the
// derivation that produced them, so the ceilings cannot drift apart from the
// event they have to fit inside.
func TestMatrixStampCeilingsHaveTheirDocumentedValues(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
		why  string
	}{
		{"matrixStampBytes", matrixStampBytes, 4 << 10,
			"the encoded stamp's share of the event: two of them (the stamp and the legacy trigger " +
				"field it feeds) beside two 16,384-byte bodies leave 24,576 bytes for the envelope"},
		{"matrixStampTitleBytes", matrixStampTitleBytes, 200,
			"the largest budget any reader of thread.Context.Title applies (thread's own " +
				"maxChatIdentityFieldBytes), so the stamp never hands a reader less than it would " +
				"have re-cut for itself"},
		{"matrixStampIDBytes", matrixStampIDBytes, 512,
			"generous for every identifier stamped (a namespace/name, a forge URL, a catalog path, " +
				"an alert-derived trigger key) and small enough that all six together stay under " +
				"the stamp's share"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d — %s", c.name, c.got, c.want, c.why)
		}
	}

	// The per-field caps must sum UNDER the encoded ceiling, or the shed in
	// boundStamp would be load-bearing for an ordinary card rather than a
	// backstop against escape expansion.
	if fields, framing := 200+6*512, 256; fields+framing > 4<<10 {
		t.Errorf("the field caps total %d bytes and the stamp's JSON framing about %d more, over the "+
			"4,096-byte encoded ceiling: an ordinary card would start shedding identity", fields, framing)
	}
	// And the whole content budget must leave the envelope its share.
	if content := 2*(16<<10) + 2*(4<<10); content > 40_960 {
		t.Errorf("two 16,384-byte bodies and two 4,096-byte stamps total %d bytes, over the 40,960 "+
			"the content may claim of a 65,536-byte event", content)
	}
}

// TestMatrixStampTruncatesTheTitleAndKeepsEveryIdentifier is the design decision
// this file exists for, as an executable claim.
//
// The title is PROSE and the only field on the stamp that is. A truncated title
// is still a true and useful answer to "which investigation is this thread
// about" — every reader of thread.Context.Title already re-cuts it (200 bytes in
// Chat.renderContext, 120 in conceptDescription), so the cut takes off only what
// a reader would have taken off anyway. So it is truncated, and marked with an
// ellipsis so a reader can tell.
func TestMatrixStampTruncatesTheTitleAndKeepsEveryIdentifier(t *testing.T) {
	inv := sampleInvestigation()
	inv.Title = strings.Repeat("the harbor-db pod is CrashLoopBackOff ", 2000)
	inv.TriggerKey = "harbordown|tooling|helmrelease|harbor|eu-west-1"
	inv.CuratedURL = "https://forge.example/o/r/pull/42"
	inv.RecalledEntry = "catalog/incidents/harbor-down.md"

	stamp := deliveredStamp(t, inv)
	if len(stamp.Title) > 200 {
		t.Errorf("the stamped title is %d bytes, over the 200-byte prose ceiling", len(stamp.Title))
	}
	if !utf8.ValidString(stamp.Title) {
		t.Error("the stamped title is not valid UTF-8: the cut split a rune")
	}
	if !strings.HasPrefix(inv.Title, strings.TrimSuffix(stamp.Title, "…")) {
		t.Errorf("the stamped title is not a prefix of the finding's own title:\n%q", stamp.Title)
	}
	if !strings.HasSuffix(stamp.Title, "…") {
		t.Errorf("a shortened title must be marked as shortened — a model handed a silently cut "+
			"title answers about an investigation it has been told the wrong name for:\n%q", stamp.Title)
	}
	// Truncating the prose must not cost the identity around it.
	for name, pair := range map[string][2]string{
		"trigger_key":    {stamp.TriggerKey, inv.TriggerKey},
		"resource":       {stamp.Resource, inv.Resource.Ref()},
		"verdict":        {stamp.Verdict, string(inv.Verdict)},
		"curated_url":    {stamp.CuratedURL, inv.CuratedURL},
		"recalled_entry": {stamp.RecalledEntry, inv.RecalledEntry},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q — bounding the prose must not disturb the identifiers",
				name, pair[0], pair[1])
		}
	}
}

// TestMatrixStampDropsAnOversizedIdentifierRatherThanTruncatingIt is the other
// half of that decision, and the half that is not symmetric with it.
//
// Truncating an identifier does not shorten it, it CHANGES it. A cut trigger_key
// names no incident at all, or — on a prefix collision — a different one, and
// that is a thumbs-up recorded against the wrong finding in the outcome ledger,
// which weights future trust. A cut curated_url is a write destination that is
// not the pull request it claims to be. Absence, by contrast, is a state every
// reader already handles: an empty trigger_key falls through to the legacy field
// and then to "not one of ours", and an empty curated_url makes the thread open
// its own PR. A wrong identity is a state nothing handles, because nothing can
// detect it.
func TestMatrixStampDropsAnOversizedIdentifierRatherThanTruncatingIt(t *testing.T) {
	inv := sampleInvestigation()
	inv.Title = "HarborDown"
	inv.TriggerKey = strings.Repeat("k", 4096)
	inv.Fingerprint = ""
	inv.CuratedURL = "https://forge.example/o/r/pull/" + strings.Repeat("9", 4096)
	inv.RecalledEntry = strings.Repeat("catalog/entry/", 400)

	stamp := deliveredStamp(t, inv)
	for name, got := range map[string]string{
		"trigger_key":    stamp.TriggerKey,
		"curated_url":    stamp.CuratedURL,
		"recalled_entry": stamp.RecalledEntry,
	} {
		if got != "" {
			t.Errorf("%s survived as %d bytes (%.40q) — an over-long identifier must be dropped "+
				"whole, never cut down to a shorter identifier naming something else", name, len(got), got)
		}
	}
	if stamp.Title != "HarborDown" {
		t.Errorf("title = %q, want %q — dropping an oversized identifier must not cost the prose "+
			"that was within budget", stamp.Title, "HarborDown")
	}
}

// TestMatrixStampAndLegacyTriggerKeyCarryTheSameValue pins the sibling field.
//
// io.runlore.trigger_key is the pre-stamp field the reaction listener still
// reads, and contextFromContent falls back to it when the stamp's own
// trigger_key is empty. Bounding one and not the other would leave the event
// unbounded through the field nobody was looking at, and would also make the
// fallback disagree with what it is a fallback FOR — the decoded stamp saying "no
// trigger identity" while the legacy field beside it named one.
func TestMatrixStampAndLegacyTriggerKeyCarryTheSameValue(t *testing.T) {
	ordinary := sampleInvestigation()
	ordinary.TriggerKey = "harbordown|tooling|helmrelease|harbor|eu-west-1"

	fallback := sampleInvestigation()
	fallback.TriggerKey, fallback.Fingerprint = "", "abc123def456"

	oversized := sampleInvestigation()
	oversized.TriggerKey, oversized.Fingerprint = strings.Repeat("k", 4096), ""

	for name, inv := range map[string]providers.Investigation{
		"ordinary":             ordinary,
		"fingerprint fallback": fallback,
		"oversized":            oversized,
	} {
		legacy, _ := deliveredMatrixContent(t, inv)[triggerKeyContentField].(string)
		stamp := deliveredStamp(t, inv)
		if legacy != stamp.TriggerKey {
			t.Errorf("%s: io.runlore.trigger_key = %q but the stamp's trigger_key = %q — the legacy "+
				"field is the stamp's own fallback and must neither contradict it nor escape its bound",
				name, legacy, stamp.TriggerKey)
		}
		if len(legacy) > 512 {
			t.Errorf("%s: io.runlore.trigger_key is %d bytes, over the 512-byte identifier ceiling — "+
				"the field beside the stamp is the same event", name, len(legacy))
		}
	}
}

// TestMatrixStampUnderBudgetIsUnchanged is the ordinary card, which is every card
// in practice: nothing about the stamp may change for a finding that fits.
func TestMatrixStampUnderBudgetIsUnchanged(t *testing.T) {
	inv := sampleInvestigation()
	inv.TriggerKey = "harbordown|tooling|helmrelease|harbor|eu-west-1"
	inv.CuratedURL = "https://forge.example/o/r/pull/42"
	inv.RecalledEntry = "catalog/incidents/harbor-down.md"
	inv.Title = "HarborDown: registry unreachable"

	got := deliveredStamp(t, inv)
	want := threadStamp{
		TriggerKey:    inv.TriggerKey,
		Title:         inv.Title,
		Resource:      inv.Resource.Ref(),
		Verdict:       string(inv.Verdict),
		CuratedURL:    inv.CuratedURL,
		RecalledEntry: inv.RecalledEntry,
	}
	if got != want {
		t.Errorf("a finding within budget had its stamp altered:\ngot  %+v\nwant %+v", got, want)
	}
}

// TestMatrixStampShedsProseBeforeIdentity covers what the per-field caps alone
// cannot: escaping. Every field below is within its own cap as composed, and the
// encoded stamp is still far over its share, because canonical JSON renders each
// of these bytes as a six-byte \u001b escape.
//
// What is given up first is the prose. A rebuilt context with no title still
// names the right incident; one with no trigger_key names nothing at all.
func TestMatrixStampShedsProseBeforeIdentity(t *testing.T) {
	inv := sampleInvestigation()
	inv.Title = strings.Repeat("\x1b", 200)
	inv.TriggerKey = strings.Repeat("\x1b", 512)
	inv.CuratedURL = strings.Repeat("\x1b", 512)
	inv.RecalledEntry = strings.Repeat("\x1b", 512)
	inv.Resource = providers.Workload{
		Namespace: strings.Repeat("\x1b", 250),
		Name:      strings.Repeat("\x1b", 250),
	}

	stamp := deliveredStamp(t, inv)
	b, err := json.Marshal(stamp)
	if err != nil {
		t.Fatalf("marshal stamp: %v", err)
	}
	if len(b) > 4<<10 {
		t.Errorf("the encoded stamp is %d bytes, over its 4,096-byte share: the ceiling was applied "+
			"to the fields as composed, not to the bytes they encode to", len(b))
	}
	if stamp.Title != "" {
		t.Errorf("title survived a shed that dropped identity — prose is the first thing given up, "+
			"because a context with no title still names the right incident: %q", stamp.Title)
	}
	if stamp.TriggerKey == "" {
		t.Error("trigger_key was shed while other fields survived: it is the join to the incident " +
			"and the last field the stamp gives up")
	}
}

// TestContextFromStampBoundsAStampItDidNotWrite is the READ side, and it is not
// belt-and-braces: a room's history already holds events this process did not
// send. Every investigation delivered before this bound existed carries an
// unbounded stamp, and the registry-miss path fetches exactly those events after
// a restart, a TTL expiry or an eviction — then hands what it decodes to a model
// prompt (thread.Chat.renderContext) and to a knowledge-base entry body
// (thread.NoteBody's "Thread:" line, which caps nothing).
func TestContextFromStampBoundsAStampItDidNotWrite(t *testing.T) {
	content := map[string]any{
		triggerKeyContentField: "legacy-key",
		threadContentField: map[string]any{
			"trigger_key":    strings.Repeat("k", 4096),
			"title":          strings.Repeat("prose ", 20_000),
			"resource":       strings.Repeat("r", 4096),
			"curated_url":    strings.Repeat("u", 4096),
			"recalled_entry": strings.Repeat("p", 4096),
		},
	}
	tc, ok := contextFromContent(content, "$root:hs", "!room:hs")
	if !ok {
		t.Fatal("a stamped event must still decode")
	}
	if len(tc.Title) > 200 {
		t.Errorf("Title decoded to %d bytes: an event written before the bound existed is still an "+
			"event this process reads back", len(tc.Title))
	}
	for name, got := range map[string]string{
		"TriggerKey":    tc.TriggerKey,
		"Resource":      tc.Resource,
		"CuratedURL":    tc.CuratedURL,
		"RecalledEntry": tc.RecalledEntry,
	} {
		if len(got) > 512 {
			t.Errorf("%s decoded to %d bytes, over the 512-byte identifier ceiling", name, len(got))
		}
	}
	// Dropping the stamp's own oversized trigger_key must fall through to the
	// legacy field, exactly as an absent one always has.
	if tc.TriggerKey != "legacy-key" {
		t.Errorf("TriggerKey = %q, want the legacy field %q — a dropped stamp key is an ABSENT key, "+
			"and an absent key has always fallen back", tc.TriggerKey, "legacy-key")
	}
	// Bounding must not widen what is trusted: root and room still come from the
	// fetch's own parameters, never from the stamp.
	if tc.Root != "$root:hs" || tc.Channel != "!room:hs" {
		t.Errorf("root/room came from the stamp: root=%q channel=%q", tc.Root, tc.Channel)
	}
}
