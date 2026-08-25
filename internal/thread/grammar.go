// SPDX-License-Identifier: Apache-2.0

package thread

import "strings"

// Intent is what a human addressed to RunLore in a thread asked for.
type Intent int

const (
	// IntentFreeform is anything without a recognised prefix — a question, or a
	// statement RunLore is left to interpret. With no model.chat configured
	// (the default) Responder.Handle writes nothing for this intent; with one
	// configured it costs exactly one model call, and the model may propose
	// note content that IS written — filed as model-drafted, never as the
	// human's words. See FreeformNotRecordedReply and Responder.freeform.
	IntentFreeform Intent = iota
	// IntentNote is an explicit "note:" — capture this verbatim into the KB.
	IntentNote
	// IntentReinvestigate is the RESERVED "reinvestigate:" prefix. It is parsed so
	// the grammar is stable, but deliberately not implemented: re-running costs a
	// full investigation and the capability already exists behind the
	// `reinvestigate` forge label. Reserving it now makes adding it later a
	// handler case rather than a grammar migration.
	IntentReinvestigate
	// IntentSilence is the "silence:" prefix: suppress re-investigating this
	// incident for the duration that follows ("silence: 4h"). Like
	// IntentReinvestigate it CHANGES BEHAVIOUR rather than recording knowledge,
	// which is why it sits at high priority in prefixes — a note that merely
	// contains it is refused as ambiguous rather than filed as the human's words.
	//
	// Unlike IntentReinvestigate, acting on it WRITES: Responder.Handle therefore
	// requires Anchored before touching the ledger, and answers
	// SilenceNotAnchoredReply otherwise. See Parse.
	IntentSilence
)

// String renders the intent for logs and metrics.
func (i Intent) String() string {
	switch i {
	case IntentNote:
		return "note"
	case IntentReinvestigate:
		return "reinvestigate"
	case IntentSilence:
		return "silence"
	default:
		return "freeform"
	}
}

// Parsed is one addressed message, split into what was asked and the text.
type Parsed struct {
	Intent Intent
	Text   string
	// Anchored reports whether the command token that produced Intent sat at
	// the START of the message, once leading mentions were stripped, rather
	// than somewhere inside it. It is meaningless for IntentFreeform, which no
	// token produced, and always false there.
	//
	// It exists so a CALLER can apply a policy Parse must not: Parse is pure
	// and knows nothing about how RunLore is configured, while whether an
	// unanchored "note:" should be treated as a command depends entirely on
	// whether the chat layer is wired. See Parse for the reasoning and
	// Responder.Handle for the one rule that reads this.
	Anchored bool
}

// prefixes maps a recognised command prefix to its intent, in PRIORITY order:
// the first entry whose token appears anywhere in the message wins, regardless
// of which one appears earlier in the text. "reinvestigate:" and "silence:"
// both lead, ahead of "note:", and for the identical reason: each CHANGES
// BEHAVIOUR rather than recording knowledge, so a note that also contains one
// of them is refused as ambiguous rather than filed as the human's words — see
// Parse for why that side is the right one to err on. Their relative order
// between themselves IS load-bearing for a message containing both tokens —
// Parse returns the first match in this slice's order, so swapping them would
// flip which one wins — and shipping "reinvestigate:" first is the safe side of
// that: it is the one whose outcome is a refusal whatever its position, whereas
// an ANCHORED "silence:" writes. "silence: 4h, and please reinvestigate: this
// tomorrow" is refused rather than silenced because reinvestigate leads.
var prefixes = []struct {
	prefix string
	intent Intent
}{
	{"reinvestigate:", IntentReinvestigate},
	{"silence:", IntentSilence},
	{"note:", IntentNote},
}

// Parse strips leading chat mentions and classifies the remainder. It is PURE:
// every prefix is matched as a whole, colon-anchored token ANYWHERE in the
// message, and nothing here knows how RunLore is configured. Whether an
// unanchored match is ACTED on differs between the two prefixes, and for
// "note:" it depends on configuration — so that decision lives with the
// configuration, in Responder.Handle, and Parse only reports the fact it needs
// (Parsed.Anchored).
//
// For "reinvestigate:" the anywhere-match is unconditional, and the reason is
// CORRECTNESS. A single filler word between an addressed mention and the
// command — "please reinvestigate: the network issue", "can you reinvestigate:
// this" — used to slip past a position-0-only check, fall through to
// IntentFreeform, and get treated exactly like an explicit "note:", silently
// opening a knowledge-base PR containing the operator's re-run request while
// telling them it was "Noted". That false success is worse than a refusal: the
// operator is left believing either a re-run started or their words were
// saved, when neither happened. A false POSITIVE costs nothing to match here,
// because the outcome is a refusal that writes nothing and spends nothing.
//
// For "silence:" the anywhere-match is likewise unconditional, but ACTING on an
// unanchored one is not, and the asymmetry is the whole point. Recording a
// silence WRITES a durable ledger event and switches investigation off for the
// window — so "note: we agreed on silence: 4h" matching here must not become a
// suppression, or one sentence of prose mutes an incident for four hours and
// throws away the note the human was actually writing. Handle refuses it with
// SilenceNotAnchoredReply; the match still happens here so the refusal can be
// specific rather than the sentence sliding into freeform and being billed.
//
// For "note:" the reason is COST, and it only holds when the chat layer is
// configured. Everything that is not a recognised prefix is IntentFreeform,
// and with model.chat wired IntentFreeform is a PAID model call: a counting
// model measured "hey <@U123> note: …", "<@U123> please note: …" and ":wave:
// <@U123> note: …" each costing one call while "<@U123> note: …" cost none. On
// Matrix it is worse: addressed() matches the bot's name as a whole word
// anywhere in the body while stripSelfMention only strips it at byte 0, so
// "hey runlore note: the cause was X" was addressed, unstripped, freeform and
// billed. The docs tell operators the deterministic path is free; an operator
// who is then billed for "please note:" was misled by the interface, not by
// the documentation.
//
// With NO chat layer — the default — that argument does not exist, and the
// widening would be a pure behavioural change in the other direction: "@runlore
// the runbook note: link is stale" answered "I didn't record that" before, and
// would instead open a real knowledge-base PR containing "link is stale",
// spending the thread's note allowance and a global forge write on a sentence
// nobody asked to record. A write is not a refusal — it cannot be undone by
// the human reading the reply — so an unanchored "note:" is treated as
// freeform there, which is byte-for-byte what it did before the chat layer
// existed. Responder.Handle applies that; see Parsed.Anchored.
//
// Two trades this makes, both deliberate and both worth stating plainly.
//
// A genuine note containing "reinvestigate:" as a colon-anchored token
// anywhere in it — "note: we agreed to reinvestigate: the DNS path next
// sprint" — is refused rather than recorded, because prefixes is scanned in
// priority order and reinvestigate leads. That is the right side to err on: a
// refusal comes back with a clear, actionable message the human can rephrase
// around, whereas a false "noted" convinces them something happened when it
// did not.
//
// And, with chat configured, an ordinary sentence containing "note:" mid-way —
// "the CNI note: was wrong" — is recorded as a note of "was wrong" rather than
// answered as a question. That direction is the cheap failure: it is
// deterministic, free, and the reply says exactly what was written and where,
// so the human can see it and correct it. The alternative failed the other
// way, spending money to answer something the operator had asked to be
// recorded.
//
// Ordinary prose using a bare command word with no trailing colon — "we had to
// reinvestigate the DNS path", "I made a note about it" — is unaffected, and a
// word merely ENDING in a prefix ("footnote:", "keynote:") is not a command
// either; see commandTokenIndex for the boundary rule that keeps both narrow.
//
// Mentions are stripped generically (any leading `<@…>` token) rather than
// matched against the bot's own id: the message reached us because the
// transport already decided we were addressed, so re-deriving that here would
// duplicate the decision and need a `auth.test` round-trip to learn our own id.
// Stripping them is also what makes Anchored mean "at the start of what the
// human actually wrote": "<@U0BOT> note: x" is anchored, "hey <@U0BOT> note: x"
// is not.
func Parse(raw string) Parsed {
	s := stripLeadingMentions(raw)
	for _, p := range prefixes {
		if i, ok := commandTokenIndex(s, p.prefix); ok {
			return Parsed{Intent: p.intent, Text: strings.TrimSpace(s[i+len(p.prefix):]), Anchored: i == 0}
		}
	}
	return Parsed{Intent: IntentFreeform, Text: s}
}

// commandTokenIndex returns the byte index of the first occurrence of a
// command prefix (e.g. "note:", "reinvestigate:") in s as a WHOLE TOKEN, matched
// case-insensitively: the byte immediately before the match, if any, must
// not be alphanumeric, so "prereinvestigate:" is never mistaken for the
// reserved command and "footnote:" / "keynote:" are never mistaken for the
// note command. There is deliberately no trailing-boundary check to mirror:
// prefix always ends in ':', which is itself never alphanumeric, so ordinary
// prose using the bare word without a trailing colon can never match at all,
// regardless of what follows it — that is what keeps Parse's anywhere-match
// narrow, for both prefixes.
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
func commandTokenIndex(s, prefix string) (int, bool) {
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
// boundary condition commandTokenIndex requires immediately before a match.
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
