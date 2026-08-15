// SPDX-License-Identifier: Apache-2.0

package thread

import "strings"

// Intent is what a human addressed to RunLore in a thread asked for.
type Intent int

const (
	// IntentFreeform is anything without a recognised prefix — a question, or a
	// statement RunLore is left to interpret.
	IntentFreeform Intent = iota
	// IntentNote is an explicit "note:" — capture this verbatim into the KB.
	IntentNote
	// IntentReinvestigate is the RESERVED "reinvestigate:" prefix. It is parsed so
	// the grammar is stable, but deliberately not implemented: re-running costs a
	// full investigation and the capability already exists behind the
	// `reinvestigate` forge label. Reserving it now makes adding it later a
	// handler case rather than a grammar migration.
	IntentReinvestigate
)

// String renders the intent for logs and metrics.
func (i Intent) String() string {
	switch i {
	case IntentNote:
		return "note"
	case IntentReinvestigate:
		return "reinvestigate"
	default:
		return "freeform"
	}
}

// Parsed is one addressed message, split into what was asked and the text.
type Parsed struct {
	Intent Intent
	Text   string
}

// prefixes maps a recognised command prefix to its intent. Matched
// case-insensitively against the text remaining after leading mentions.
// reserved marks a prefix that must be caught even when it is NOT at
// position 0 — see Parse.
var prefixes = []struct {
	prefix   string
	intent   Intent
	reserved bool
}{
	{"note:", IntentNote, false},
	{"reinvestigate:", IntentReinvestigate, true},
}

// Parse strips leading chat mentions and classifies the remainder.
//
// A RESERVED prefix (see IntentReinvestigate) is looked for FIRST, and
// matched as a whole, colon-anchored token ANYWHERE in the remainder — not
// only at position 0 like the ordinary prefixes below. This is deliberate,
// not an oversight: a single filler word between an addressed mention and
// the command — "please reinvestigate: the network issue", "can you
// reinvestigate: this" — used to slip past a position-0-only check, fall
// through to IntentFreeform, and get treated exactly like an explicit
// "note:" — silently opening a knowledge-base PR containing the operator's
// re-run request while telling them it was "Noted". That false success is
// worse than a refusal: the operator is left believing either a re-run
// started or their words were saved, when neither happened.
//
// The trade this makes is deliberate too, and is worth stating plainly: a
// genuine note that happens to contain a reserved prefix as a colon-anchored
// token anywhere in it — e.g. "note: we agreed to reinvestigate: the DNS
// path next sprint" — is now refused rather than recorded, even though the
// human meant to record a note. That is the right side to err on: a refusal
// comes back with a clear, actionable message the human can rephrase around,
// whereas a false "noted" convinces them something happened when it did not.
// Ordinary prose using the bare word with no trailing colon — "we had to
// reinvestigate the DNS path" — is unaffected; see reservedTokenIndex for
// why the colon anchor keeps that narrow.
//
// Every other prefix (currently only "note:") still matches at position 0
// only: it is not reserved, so there is no false-success risk to guard
// against by widening it, and doing so anyway would just as happily start
// matching "note:" out of the middle of an ordinary sentence.
//
// Mentions are stripped generically (any leading `<@…>` token) rather than
// matched against the bot's own id: the message reached us because the
// transport already decided we were addressed, so re-deriving that here would
// duplicate the decision and need a `auth.test` round-trip to learn our own id.
func Parse(raw string) Parsed {
	s := stripLeadingMentions(raw)

	for _, p := range prefixes {
		if !p.reserved {
			continue
		}
		if i, ok := reservedTokenIndex(s, p.prefix); ok {
			return Parsed{Intent: p.intent, Text: strings.TrimSpace(s[i+len(p.prefix):])}
		}
	}
	lower := strings.ToLower(s)
	for _, p := range prefixes {
		if p.reserved {
			continue // already scanned above, anywhere in the text rather than only at position 0
		}
		if strings.HasPrefix(lower, p.prefix) {
			return Parsed{Intent: p.intent, Text: strings.TrimSpace(s[len(p.prefix):])}
		}
	}
	return Parsed{Intent: IntentFreeform, Text: s}
}

// reservedTokenIndex returns the byte index of the first occurrence of a
// reserved prefix (e.g. "reinvestigate:") in s as a WHOLE TOKEN, matched
// case-insensitively: the byte immediately before the match, if any, must
// not be alphanumeric, so "prereinvestigate:" is never mistaken for the
// reserved command. There is deliberately no trailing-boundary check to
// mirror: prefix always ends in ':', which is itself never alphanumeric, so
// ordinary prose using the bare word without a trailing colon can never
// match at all, regardless of what follows it — that is what keeps Parse's
// reserved-anywhere match narrow.
//
// Matching is done window-by-window against s itself with strings.EqualFold,
// deliberately NOT by pre-lowering the whole string once and searching that
// copy for prefix: strings.ToLower is not guaranteed to preserve a
// character's UTF-8 byte length — two code points in the entire Unicode
// range ('Ⱥ'/'Ⱦ', U+023A/U+023E) lower-case to a LONGER byte sequence — so a
// byte offset found by scanning a separately lower-cased copy can silently
// desynchronise from the same offset in s, and using it to slice s (as the
// caller does with the index this function returns) risks an
// index-out-of-range panic or a mis-sliced Text for a message containing one
// of those characters ahead of the match. Comparing byte windows of s
// directly has no such hazard: prefix is pure ASCII, and an ASCII byte can
// never appear as part of a multi-byte UTF-8 sequence, so any window that
// case-folds equal to prefix is itself already a run of single-byte ASCII
// characters — scanning every byte offset of s is therefore always safe,
// never misaligned, regardless of what precedes it.
func reservedTokenIndex(s, prefix string) (int, bool) {
	for i := 0; i+len(prefix) <= len(s); i++ {
		if !strings.EqualFold(s[i:i+len(prefix)], prefix) {
			continue
		}
		if i > 0 && isASCIIAlphanumeric(s[i-1]) {
			continue
		}
		return i, true
	}
	return 0, false
}

// isASCIIAlphanumeric reports whether b is an ASCII letter or digit — the
// boundary condition reservedTokenIndex requires immediately before a match.
func isASCIIAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// stripLeadingMentions removes every leading `<@…>` token (with the surrounding
// whitespace) and trims the result.
func stripLeadingMentions(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "<@") {
		end := strings.IndexByte(s, '>')
		if end < 0 {
			break
		}
		s = strings.TrimSpace(s[end+1:])
	}
	return strings.TrimSpace(s)
}
