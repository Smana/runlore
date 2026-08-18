// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/thread"
)

// TestThreadDefaultsMatchTheDocs pins every `notify.thread` default the shipped
// docs quote as a NUMBER to the constant that actually decides it.
//
// The defect this exists for: notify.thread.chat_tokens_per_hour's default is
// DERIVED — maxChatCallTokens x DefaultChatCallsPerHour x 2/3 — so it changes
// when an unrelated byte cap in internal/thread changes, and nobody editing that
// byte cap thinks of the docs. It landed at 107720 while SECURITY.md, the
// configuration reference and both notification pages still said 200000, the
// value from before the derivation. Both directions of that gap hurt an
// operator: one budgeting for 200000 hits the real ceiling at ~54% of the volume
// they planned, after which every threaded question degrades to the
// deterministic reply for the rest of the sliding hour; one who pastes the
// sample's `chat_tokens_per_hour: 200000` as "the default" silently raises the
// true ceiling by 1.86x. Neither shows up as an error anywhere.
//
// It parses the real pages rather than checking a hand-kept list of known
// snippets, so a new page that quotes one of these defaults is covered the day
// it is written — the failure mode of a curated list is that the page nobody
// remembered to add is exactly the page that rots.
//
// What counts as a claim is deliberately narrow (see threadDefaultClaims): a
// number is checked only when the nearest thing before it is one of these keys
// AND the text between them says "default", or the number is the key's YAML
// value, or it sits in a later cell of the same table row. That is what keeps
// prose like "only `max_note_bytes` moves it) · the whole assembled prompt
// (fixed at ~15 KB…)" from being read as a claim that max_note_bytes is 15.
// The cost of that narrowness is a claim phrased backwards ("the default of
// 200000 for `chat_tokens_per_hour`") going unseen, which is why the inertness
// checks at the bottom refuse to pass on finding nothing.
func TestThreadDefaultsMatchTheDocs(t *testing.T) {
	pinned := map[string]int64{
		"chat_tokens_per_hour":  thread.DefaultChatTokensPerHour,
		"chat_calls_per_hour":   int64(thread.DefaultChatCallsPerHour),
		"max_note_bytes":        int64(thread.DefaultMaxNoteBytes),
		"max_notes_per_thread":  int64(thread.DefaultMaxNotesPerThread),
		"forge_writes_per_hour": int64(thread.DefaultForgeWritesPerHour),
		"registry_max":          int64(thread.DefaultRegistryMax),
	}

	seenIn := make(map[string]map[string]bool, len(pinned))
	for _, path := range threadDocPages(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel := relToRepo(t, path)
		for _, c := range threadDefaultClaims(rel, string(src), pinned) {
			if seenIn[c.key] == nil {
				seenIn[c.key] = map[string]bool{}
			}
			seenIn[c.key][c.file] = true
			if c.val == pinned[c.key] {
				continue
			}
			t.Errorf("%s:%d states notify.thread.%s defaults to %d, but the code says %d.\n"+
				"  claim: %s\n"+
				"  Restate the number here, or change the constant — do not leave them disagreeing. "+
				"chat_tokens_per_hour in particular is DERIVED from the chat layer's per-call token "+
				"ceiling and chat_calls_per_hour, so an edit to a byte cap in internal/thread moves it "+
				"without touching this page; internal/config's "+
				"TestThreadDefaultsHaveTheirDocumentedValues pins the literal.",
				c.file, c.line, c.key, c.val, pinned[c.key], c.text)
		}
	}

	// Guard the guard. Every one of these keys is quoted with its default
	// somewhere in the shipped docs today; finding none means the parse stopped
	// matching (a page moved, a table changed shape, a sample was rewritten) and
	// the guard is reporting success over nothing.
	for _, key := range sortedStrings(pinned) {
		if len(seenIn[key]) == 0 {
			t.Errorf("found no stated default for notify.thread.%s in any shipped page — "+
				"either the docs stopped documenting it or threadDefaultClaims no longer "+
				"recognises how they phrase it, and this guard is inert for that key", key)
		}
	}
	// chat_tokens_per_hour is the one that already drifted, and it is stated on
	// four pages (SECURITY.md, the configuration reference, the Slack page, the
	// Matrix page). Requiring several holds the guard to catching a partial fix —
	// the failure where someone corrects the reference page and forgets the
	// transport pages that copy it.
	if n := len(seenIn["chat_tokens_per_hour"]); n < 3 {
		t.Errorf("chat_tokens_per_hour's default is stated on only %d page(s); it is documented on "+
			"SECURITY.md, the configuration reference and both notification pages, so a count this "+
			"low means the guard has stopped seeing most of them", n)
	}
}

// TestAnnounceKBUpdatesDefaultMatchesTheDocs pins the default of
// notify.thread.announce_kb_updates against every page that states it.
//
// The numeric guard parses numbers only, so a page stating this key's default
// was pinned by nothing at all: it could be flipped to opt-OUT in code and four
// pages would keep telling an operator it was off. That is worse than the
// numeric drift this file was written for, because the key decides whether note
// content — what someone typed in one thread — is broadcast to every configured
// sink. A doc saying "off unless you set it" while the code ships it on is a
// privacy claim the software does not honour.
//
// The default comes from decoding an EMPTY config through the real type, not
// from a literal repeated here: that is what "the default" means operationally
// — what an operator who wrote no such key gets.
//
// # Why this is no longer the boolean guard it replaces
//
// This was a bool-only parse, and that had become a trap. The key stopped being
// a boolean when it grew destinations (config.AnnounceMode: off, channel,
// thread, both), but the parse still recognised only `true|false`. It passed
// solely because every page happens to write `default **false**` before any
// other boolean token — so rewriting a page to the equally correct
// `default **off**`, the obvious follow-up edit, would have made the guard walk
// past it, find the `true` in "# also true (= channel)" as the next boolean
// after the word "default", and report that the docs claim the key defaults to
// TRUE. The failure would have pointed the reader at changing the code default
// on the strength of a sentence explaining an alias.
//
// So the vocabulary is the key's OWN, and a claim is now recognised only where
// the value token immediately follows the word "default" (see
// docAnnounceDefaultRE). Both halves matter: widening the vocabulary alone would
// have made every ordinary "the thread reply" and "the channel post" in this
// feature's prose a candidate claim, which is a far bigger surface of English
// than `true|false` ever was.
//
// # What it does and does not verify
//
// It compares DESTINATIONS, not just on/off, through announceDocModes — which
// is also where `false` and `off`, and `true` and `channel`, are recorded as the
// same claim. That equivalence is the compatibility promise config.AnnounceMode
// makes to an operator, so a page may state the default in either spelling and a
// page that starts saying `channel` where the code says off still fails.
//
// It bites from BOTH sides now. It used to be a half guard — AnnounceKBUpdates
// was a plain bool with no unmarshaler, so decoding "{}" could only ever yield
// false and no edit to internal/config could make an unconfigured deployment
// announce. The type has a custom unmarshaler and several states today, so a
// default that resolved to anything but off — by a mistake in the unmarshaler,
// or by someone deciding the empty state should announce — fails here against
// every page that promises otherwise.
func TestAnnounceKBUpdatesDefaultMatchesTheDocs(t *testing.T) {
	var c config.Config
	if err := yaml.Unmarshal([]byte("{}"), &c); err != nil {
		t.Fatalf("decode an empty config: %v", err)
	}
	want := canonAnnounceMode(c.Notify.Thread.AnnounceKBUpdates)

	// The pages are compared against the resolved DESTINATION above, but every
	// consumer branches on On(), which is a different function. A default that
	// reads "off" while On() answers true would satisfy every page below and
	// still announce, so the two are pinned to each other here — otherwise this
	// guard's claim ("no page promises an off state the code does not honour")
	// rests on a value nothing downstream reads.
	if got := c.Notify.Thread.AnnounceKBUpdates.On(); got != (want != config.AnnounceOff) {
		t.Fatalf("an empty config resolves announce_kb_updates to destination %q but On() answers %v — "+
			"the value these pages are checked against and the switch that decides whether anything is "+
			"announced disagree, so every page below is being compared to the wrong thing",
			string(want), got)
	}

	seenIn := map[string]bool{}
	for _, path := range threadDocPages(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel := relToRepo(t, path)
		for _, claim := range announceDefaultClaims(rel, string(src)) {
			seenIn[claim.file] = true
			if canonAnnounceMode(announceDocModes[claim.text]) == want {
				continue
			}
			t.Errorf("%s:%d states notify.thread.announce_kb_updates defaults to %q, but an empty config resolves it to %q.\n"+
				"  Restate it here, or change the default — do not leave them disagreeing. "+
				"announce_kb_updates decides whether a note written in one thread is broadcast to every "+
				"configured sink, so a page promising it is off while the code ships it on is a claim about "+
				"where an operator's words go that the software does not honour.",
				claim.file, claim.line, claim.text, string(want))
		}
	}

	// Guard the guard, exactly as the numeric test does: the key is documented
	// today, so finding nothing means the parse stopped matching and this is
	// reporting success over nothing.
	//
	// It is stated on the configuration reference, both notification pages and the
	// reviewing-knowledge concept page. Requiring several holds the guard to
	// catching a PARTIAL fix — the failure where someone corrects one page and
	// leaves the others promising the opposite.
	if len(seenIn) < 3 {
		t.Errorf("announce_kb_updates' default is stated on %d page(s); it is documented on the "+
			"configuration reference, both notification pages and the reviewing-knowledge concept page, "+
			"so a count this low means docAnnounceDefaultRE has stopped recognising how they phrase it "+
			"and this guard is inert", len(seenIn))
	}
}

// announceDocModes maps every spelling a page may state this default in onto the
// destination it means. `false` and `off` are the same claim, and so are `true`
// and `channel` — that equivalence is the compatibility promise
// config.AnnounceMode makes, so a page is free to use either.
var announceDocModes = map[string]config.AnnounceMode{
	"false":   config.AnnounceOff,
	"off":     config.AnnounceOff,
	"true":    config.AnnounceChannel,
	"channel": config.AnnounceChannel,
	"thread":  config.AnnounceThread,
	"both":    config.AnnounceBoth,
}

// canonAnnounceMode folds the absent key onto the explicit "off" so a doc
// stating either spelling compares equal to a config that resolved to the other.
func canonAnnounceMode(m config.AnnounceMode) config.AnnounceMode {
	if m == "" {
		return config.AnnounceOff
	}
	return m
}

// docAnnounceDefaultRE matches a stated default for this key: the word
// "default", then at most a few bytes of markup or punctuation, then one of the
// key's own values. The gap covers "default **false**", "default `off`",
// "default false" and "default: channel"; it deliberately does NOT let arbitrary
// prose sit between the two, which is what stops "…so a note written in one
// thread…" from being read as a claim about a key mentioned a sentence earlier.
var docAnnounceDefaultRE = regexp.MustCompile(`(?i)default[^\w]{0,4}(true|false|off|channel|thread|both)\b`)

// announceDefaultClaims extracts the default claims one markdown page makes
// about announce_kb_updates, attributed the way threadDefaultClaims attributes
// numbers: the nearest config key before the match, so a "default off" belonging
// to some other key is never read as this one's.
func announceDefaultClaims(file, src string) []docClaim {
	var claims []docClaim
	for _, u := range splitDocUnits(src) {
		keys := docKeyRE.FindAllStringIndex(u.text, -1)
		for _, m := range docAnnounceDefaultRE.FindAllStringSubmatchIndex(u.text, -1) {
			key, _ := nearestKeyBefore(u.text, keys, m[0])
			if key != "announce_kb_updates" {
				continue
			}
			claims = append(claims, docClaim{
				file: file,
				line: u.lineAt(m[0]),
				key:  key,
				text: strings.ToLower(u.text[m[2]:m[3]]),
			})
		}
	}
	return claims
}

// docQuoteCapRE matches the announcement's note-quote ceiling as the transport
// pages phrase it. It reads a joined UNIT rather than a raw line because both
// pages hard-wrap the sentence, and each wraps it in a different place: slack.md
// breaks before "capped at", matrix.md between "capped at" and the number.
//
// "bytes" is the whole of what keeps this narrow. Every other "capped at" in the
// shipped docs states a MiB size or a probability, so this phrase belongs to the
// quoted note and nothing else; a page that needs to state a different byte cap
// should name its config key and be read by threadDefaultClaims instead.
var docQuoteCapRE = regexp.MustCompile(`capped at (\d[\d,_]*) bytes`)

// TestAnnouncedNoteQuoteCapMatchesTheDocs pins the "capped at 512 bytes"
// sentence on the Slack and Matrix pages to notify.kbNotePreviewBytes, the
// constant that actually cuts the quote.
//
// The numeric guard above cannot: it only reads a number the docs attribute to a
// `notify.thread.*` KEY, and this ceiling has no key — it is not configurable,
// deliberately, because it is derived from the transports' own message limits
// (see the const block in internal/notify/kbupdate.go). So both pages stated a
// number that nothing in the build compared against anything, on a sentence an
// operator uses to decide whether announcing is safe for their channel: told the
// quote stops at 512 bytes, they get however much the constant currently emits.
//
// The constant is read out of the source file rather than imported, for the same
// reason logsProviderIDs parses detect.go: it is unexported, and exporting a
// notifier's internal ceiling so a guard can see it would widen an API to serve
// a test. A rename or a non-literal value makes this guard INERT rather than
// wrong, which is what the error below reports.
func TestAnnouncedNoteQuoteCapMatchesTheDocs(t *testing.T) {
	want, err := notifyIntConst(repoRoot(t), "kbNotePreviewBytes")
	if err != nil {
		t.Fatalf("%v", err)
	}

	seen := map[string]bool{}
	for _, path := range threadDocPages(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel := relToRepo(t, path)
		for _, u := range splitDocUnits(string(src)) {
			for _, m := range docQuoteCapRE.FindAllStringSubmatchIndex(u.text, -1) {
				seen[rel] = true
				lit := u.text[m[2]:m[3]]
				got, err := strconv.ParseInt(strings.NewReplacer(",", "", "_", "").Replace(lit), 10, 64)
				if err != nil || got == want {
					continue
				}
				t.Errorf("%s:%d says the announced note quote is capped at %d bytes, but "+
					"notify.kbNotePreviewBytes cuts it at %d.\n  claim: %s\n"+
					"  Restate the number here, or change the constant — do not leave them disagreeing. "+
					"This sentence is what an operator reads to decide how much of a private note "+
					"reaches every configured sink.",
					rel, u.lineAt(m[0]), got, want, docSnippet(u.text, m[0]-60, m[1]+20))
			}
		}
	}

	// Guard the guard. Both notification pages state this today; finding fewer
	// means the sentence was reworded and this is reporting success over nothing.
	if len(seen) < 2 {
		t.Errorf("found the announced note quote's cap stated on %d page(s), want the Slack and "+
			"Matrix notification pages at least — either they stopped saying it or docQuoteCapRE "+
			"no longer recognises how they phrase it, and this guard is inert", len(seen))
	}
}

// notifyIntConst returns the value of one untyped integer constant declared in
// internal/notify/kbupdate.go, read from the source file itself.
//
// Go cannot enumerate another package's unexported constants at runtime, and the
// alternative — repeating the number here — is the drift this whole package
// exists to catch, reproduced inside the catcher: the guard would compare the
// docs against a copy free to disagree with the code.
func notifyIntConst(root, name string) (int64, error) {
	path := filepath.Join(root, "internal", "notify", "kbupdate.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					return 0, fmt.Errorf("%s: %s is no longer a plain integer literal, so this guard "+
						"can no longer read it and is inert", path, name)
				}
				return strconv.ParseInt(strings.NewReplacer(",", "", "_", "").Replace(lit.Value), 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("%s declares no const %s — it was renamed or moved, and this guard is inert", path, name)
}

// docClaim is one number a page states as a key's default, kept with enough
// provenance to point an operator-facing failure at the exact line.
type docClaim struct {
	file string
	line int
	key  string
	text string
	val  int64
}

var (
	// docKeyRE matches a config key as the docs write it: snake_case, optionally
	// dotted-prefixed (`notify.thread.max_note_bytes`). Every such key is an
	// attribution barrier even when it is not pinned, which is what stops
	// `model.chat.max_tokens`'s 1024 from being attributed to the pinned key
	// mentioned earlier in the same sentence.
	docKeyRE = regexp.MustCompile(`(?:[a-z][a-z0-9]*\.)*[a-z][a-z0-9]*(?:_[a-z0-9]+)+`)
	// docNumRE matches an integer literal as prose, YAML or a table cell writes
	// it, tolerating either thousands separator.
	docNumRE = regexp.MustCompile(`\d[\d,_]*`)
	// docYAMLValueRE recognises the gap between a YAML key and its value, so
	// `chat_tokens_per_hour: 107720` counts as a stated default without needing
	// the word.
	docYAMLValueRE = regexp.MustCompile(`^:\s*$`)
	docListItemRE  = regexp.MustCompile(`^(?:[-*+]\s|\d+\.\s)`)
)

// threadDefaultClaims extracts the default claims a single markdown page makes
// about the pinned keys.
//
// It works over units (see splitDocUnits) rather than raw lines because these
// pages are hard-wrapped: the key and the number it is a default for routinely
// land on different lines, so a line-at-a-time parse would miss most of them.
func threadDefaultClaims(file, src string, pinned map[string]int64) []docClaim {
	var claims []docClaim
	for _, u := range splitDocUnits(src) {
		keys := docKeyRE.FindAllStringIndex(u.text, -1)
		prevNumEnd := 0
		for _, num := range docNumRE.FindAllStringIndex(u.text, -1) {
			key, end := nearestKeyBefore(u.text, keys, num[0])
			anchor := max(end, prevNumEnd)
			prevNumEnd = num[1]
			if key == "" {
				continue
			}
			if _, ok := pinned[key]; !ok {
				continue
			}
			// The gap starts at whichever came last, the key or the previous
			// number: a "default" already spent on an earlier number in the same
			// sentence must not be read as introducing this one, which is how
			// "(default 8192 bytes) · … (fixed at ~15 KB" would otherwise claim
			// max_note_bytes is 15.
			between := u.text[anchor:num[0]]
			stated := strings.Contains(strings.ToLower(between), "default") ||
				docYAMLValueRE.MatchString(between) ||
				(u.table && strings.Contains(between, "|"))
			if !stated {
				continue
			}
			lit := u.text[num[0]:num[1]]
			val, err := strconv.ParseInt(strings.NewReplacer(",", "", "_", "").Replace(lit), 10, 64)
			if err != nil {
				continue
			}
			claims = append(claims, docClaim{
				file: file,
				line: u.lineAt(num[0]),
				key:  key,
				text: docSnippet(u.text, num[0]-60, num[1]+20),
				val:  val,
			})
		}
	}
	return claims
}

// docSnippet quotes the text around [from, to) for a failure message, widening
// to rune boundaries first: these pages are full of "·", "—" and "≈", and a
// byte-sliced excerpt would hand the reader a replacement character where the
// evidence should be.
func docSnippet(s string, from, to int) string {
	from = max(from, 0)
	to = min(to, len(s))
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	for to < len(s) && !utf8.RuneStart(s[to]) {
		to++
	}
	return strings.TrimSpace(s[from:to])
}

// nearestKeyBefore returns the base name of the last key starting before off,
// with the offset just past it. A number with no key before it in its unit
// belongs to nobody and is not a claim.
func nearestKeyBefore(text string, keys [][]int, off int) (key string, end int) {
	for _, k := range keys {
		if k[0] >= off {
			break
		}
		if k[1] > off {
			continue // the number is inside the identifier itself
		}
		name := text[k[0]:k[1]]
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		key, end = name, k[1]
	}
	return key, end
}

// docUnit is one attribution scope: the span within which "the key nearest
// before this number" is a safe reading. Prose wraps, so a unit is a whole
// paragraph or list item joined back into one string; a table row, a fenced
// code line and an indented code line each stand alone, because in those a
// neighbouring line is a different statement rather than a continuation.
type docUnit struct {
	text   string
	starts []int
	lines  []int
	table  bool
}

// lineAt maps a byte offset in the joined text back to the source line, so a
// failure names the line an editor has to open.
func (u docUnit) lineAt(off int) int {
	line := 0
	for i, s := range u.starts {
		if s > off {
			break
		}
		line = u.lines[i]
	}
	return line
}

func splitDocUnits(src string) []docUnit {
	var (
		units   []docUnit
		cur     *docUnit
		inFence bool
	)
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.text) != "" {
			units = append(units, *cur)
		}
		cur = nil
	}
	appendLine := func(raw string, n int, table bool) {
		if cur == nil {
			cur = &docUnit{table: table}
		}
		if cur.text != "" {
			cur.text += " "
		}
		cur.starts = append(cur.starts, len(cur.text))
		cur.lines = append(cur.lines, n)
		cur.text += raw
	}
	alone := func(raw string, n int, table bool) {
		flush()
		appendLine(raw, n, table)
		flush()
	}

	for i, raw := range strings.Split(src, "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			inFence = !inFence
			flush()
		case inFence, strings.HasPrefix(raw, "    "), strings.HasPrefix(raw, "\t"):
			// Code, fenced or indented: each line is its own statement.
			alone(raw, n, false)
		case trimmed == "":
			flush()
		default:
			body := strings.TrimLeft(trimmed, "> ") // blockquote markers are decoration
			switch {
			case strings.HasPrefix(body, "|"):
				alone(raw, n, true)
			case strings.HasPrefix(body, "#"):
				flush()
			case docListItemRE.MatchString(body):
				flush()
				appendLine(raw, n, false)
			default:
				appendLine(raw, n, false)
			}
		}
	}
	flush()
	return units
}

// threadDocPages returns every shipped markdown page an operator reads: the
// repo-root documents (SECURITY.md and its neighbours) and the whole published
// site content tree. Discovered by walking, not listed, so a page added
// tomorrow is checked tomorrow.
func threadDocPages(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)

	var pages []string
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			pages = append(pages, filepath.Join(root, e.Name()))
		}
	}

	content := filepath.Join(root, "website", "content")
	err = filepath.WalkDir(content, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", content, err)
	}
	if len(pages) == 0 {
		t.Fatal("no markdown pages found — the docs tree moved and this guard is inert")
	}
	sort.Strings(pages)
	return pages
}

func relToRepo(t *testing.T, path string) string {
	t.Helper()
	rel, err := filepath.Rel(repoRoot(t), path)
	if err != nil {
		return path
	}
	return rel
}

func sortedStrings[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMaxNoteBytesFloorMatchesTheDocs pins the one notify.thread number the
// defaults guard above cannot see.
//
// That guard reads DEFAULTS — a number counts as a claim only when the text
// between the key and the number says "default" — and this is a FLOOR: the
// smallest positive max_note_bytes config.Validate accepts. It is a number an
// operator acts on (a config below it fails to load) and one nothing else
// checks, which is exactly the shape that rots: thread.MinMaxNoteBytes is
// derived from the truncation marker's own width, so it moves when that wording
// moves, and nobody editing a marker string thinks of a configuration page.
//
// The oracle is the constant, and the search is for the number as the page
// actually writes it. It refuses to pass on finding nothing, for the reason the
// guard above states about inertness: a page that stopped mentioning the floor
// leaves an operator with no warning that their config will be rejected, and a
// test that silently passes on an absent claim is how that happens quietly.
func TestMaxNoteBytesFloorMatchesTheDocs(t *testing.T) {
	page := filepath.Join(repoRoot(t), "website", "content", "docs", "configuration", "configuration.md")
	src, err := os.ReadFile(page) //nolint:gosec // G304: a fixed path under the repo root
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	text := string(src)

	// The sentence that states the floor, located by its subject rather than by
	// its number, so a WRONG number is found and reported rather than missed.
	const marker = "a positive `max_note_bytes` under "
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("%s no longer states the max_note_bytes floor. An operator whose value is refused at "+
			"load has nothing to read; restore the sentence or delete this guard deliberately.", page)
	}
	rest := text[i+len(marker):]
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end <= 0 {
		t.Fatalf("the floor sentence in %s names no number: %.80q", page, rest)
	}
	got, err := strconv.Atoi(rest[:end])
	if err != nil {
		t.Fatalf("parse the floor from %s: %v", page, err)
	}
	if got != thread.MinMaxNoteBytes {
		t.Errorf("%s says a positive max_note_bytes under %d is refused; config.Validate refuses under %d. "+
			"An operator sizing to the documented figure gets a config that will not load.",
			page, got, thread.MinMaxNoteBytes)
	}
}
