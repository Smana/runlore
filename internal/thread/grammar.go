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
var prefixes = []struct {
	prefix string
	intent Intent
}{
	{"note:", IntentNote},
	{"reinvestigate:", IntentReinvestigate},
}

// Parse strips leading chat mentions and classifies the remainder.
//
// Mentions are stripped generically (any leading `<@…>` token) rather than
// matched against the bot's own id: the message reached us because the
// transport already decided we were addressed, so re-deriving that here would
// duplicate the decision and need a `auth.test` round-trip to learn our own id.
func Parse(raw string) Parsed {
	s := stripLeadingMentions(raw)
	lower := strings.ToLower(s)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p.prefix) {
			return Parsed{Intent: p.intent, Text: strings.TrimSpace(s[len(p.prefix):])}
		}
	}
	return Parsed{Intent: IntentFreeform, Text: s}
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
