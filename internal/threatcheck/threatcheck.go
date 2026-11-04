// SPDX-License-Identifier: Apache-2.0

// Package threatcheck screens text for what is meant to steer the agents that later read it. A
// knowledge-base entry quotes logs and alert labels, and every factory agent reads entries, so a
// crafted log line would otherwise become a stored injection.
//
// It is deterministic and offline on purpose: a model would be the thing being attacked. And it is a
// screen for obvious injection, not a proof of safety: rewording, another language, homoglyphs or a
// payload in a code span pass it (TestKnownPasses pins some). The intended boundary is that every
// reader receives entries as quoted data. A false positive costs a human look, so the rules lean to
// catching, and stay narrow enough for an ordinary incident entry to pass.
//
// Text is matched in the form a reader gets, not only as stored: frontmatter values are decoded from
// YAML and screened too, and base64, hex and escape runs are decoded and screened again.
package threatcheck

import (
	"encoding/base64"
	"encoding/hex"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
)

// The rules, in the order Check reports them.
const (
	// RuleChatMarkup is chat or role markup: <|im_start|>, [INST], <system>, </tool_result>,
	// <function_calls>, {"role":"system"}, or a turn opening with "system:" or "assistant:".
	RuleChatMarkup = "chat-markup"
	// RuleInstructionOverride is a sentence aimed at an agent: "ignore previous instructions",
	// "you are now", "disregard the above", "do not tell the user".
	RuleInstructionOverride = "instruction-override"
	// RuleHiddenCharacter is a character a reader does not see and a model does: a Unicode format
	// or control character other than tab and newline, or text that is not UTF-8.
	RuleHiddenCharacter = "hidden-character"
	// RuleEncodedPayload is a long base64 or hex run outside a code span that does not have a SHA's
	// length, or a shorter run that decodes to what another rule catches.
	RuleEncodedPayload = "encoded-payload"
	// RuleExfiltration is a shell shape that runs fetched code or reads a credential: curl into a
	// shell, a pipe to a shell, gh auth token, reading a credentials path.
	RuleExfiltration = "exfiltration"
	// RuleMalformedFrontmatter is frontmatter that does not parse, so its values cannot be screened.
	RuleMalformedFrontmatter = "malformed-frontmatter"
)

var ruleOrder = []string{
	RuleChatMarkup, RuleInstructionOverride, RuleHiddenCharacter, RuleEncodedPayload, RuleExfiltration, RuleMalformedFrontmatter,
}

const (
	// The code points hasHidden names.
	emojiSelector      = 0xFE0F
	lineSeparator      = 0x2028
	paragraphSeparator = 0x2029

	// longRun is where a run is a payload for its shape alone; a shorter one must decode to a hit.
	longRun = 40
	// longEscapes is the same for \xNN and \uNNNN runs. A log may quote a few bytes that way.
	longEscapes = 8
	// maxDecodeDepth bounds how many encodings hasEncoded peels off.
	maxDecodeDepth = 2
)

var (
	// chatLine reads the line view: a turn opens at the start of a line or right after a colon or a
	// quote, as a quoted log line does. "user:" and "tool:" are not here, nor "system:" with no space
	// after it: kubeconfig keys and RBAC subjects (system:masters) are ordinary text.
	chatLine = regexp.MustCompile(`(?im)` + strings.Join([]string{
		`(?:^[ \t>_#-]*|[:"'][ \t]*)(?:system|assistant|developer|human)[ \t]*:(?:_*[ \t]|_*$)`,
		`^[ \t>]*#{1,6}[ \t]*instructions?[ \t]*:`,
	}, "|"))

	// chatFlat reads the flat view, where a line split or spacing inside a tag is one space. A tag has
	// nothing but attributes after its name, so "load < system and y > 3" is no tag.
	chatFlat = regexp.MustCompile(`(?i)` + strings.Join([]string{
		`<\s*\|[^|]{0,64}\|\s*>`,
		`\[\s*/?\s*(?:INST|SYSTEM)\s*\]`,
		`<<\s*/?\s*SYS\s*>>`,
		`<\s*/?\s*(?:system|system[_-]prompt|assistant|developer|instructions|tool_use|tool_calls?|tool_results?|tool_responses?|function_calls?|function_results?|function_responses?|start_of_turn|end_of_turn|invoke|parameter)\s*(?:\s[\w:.-]+\s*=[^<>]*)?>`,
		`<\s*/\s*(?:user|human)\s*>`,
		`"role"\s*:\s*"(?:system|assistant|developer|tool)"`,
	}, "|"))

	// A verb, the words that may stand between it and its object, and the object: "ignore previous
	// alerts" and "ignore the upgrade instructions" read as ordinary incident prose. It reads the
	// phrase view: lower case, one space between words, no emphasis, "_" and "-" as spaces.
	overrideVerb   = `\b(?:ignore|disregard|forget|override|bypass|skip|do not follow|don't follow|dont follow|stop following|pay no attention to) `
	overrideFiller = `(?:(?:all|any|and|every|everything|of|the|your|my|these|those|that|this|previous|prior|earlier|above|preceding|foregoing|original|former|system|safety|\d+|two|three|four|five) )*`
	overridePrior  = `(?:previous|prior|earlier|above|preceding|foregoing|original|former)`
	overrideRules  = `(?:rules?|directions?|directives?|guidance|guidelines?|context|messages?|commands?|text|conversation|content|training|programming|constraints?)`
	audience       = `(?:the |your )?(?:user|human|operator|maintainer|reviewer)s?\b`
	override       = regexp.MustCompile(strings.Join([]string{
		overrideVerb + overrideFiller + `(?:instructions?|prompts?)\b`,
		overrideVerb + overrideFiller + overridePrior + ` ` + overrideRules + `\b`,
		overrideVerb + overrideFiller + `your ` + overrideFiller + overrideRules + `\b`,
		overrideVerb + `(?:(?:all|everything|anything|of|the) )+(?:above|preceding) ?(?:[.,;:!?)]|and\b|$)`,
		overrideVerb + `(?:everything|anything|all) (?:above|before|you were|you've been|that came)\b`,
		`\b(?:ignore|disregard|forget) all of (?:that|this)\b`,
		`\byou(?: are|'re|re) now\b`,
		`\bfrom now on,? you\b`,
		`\bact as (?:the |an? |your )?(?:system|root|admin|administrator|maintainer|owner)\b`,
		`\bpretend (?:that )?(?:you are|you're|to be)\b`,
		`\byour new (?:role|instructions?|task|rules?)\b`,
		`\b(?:new|updated) instructions ?:`,
		`\bsystem override\b`,
		`\b(?:do not|don't|dont|never) (?:tell|inform|show|reveal to|mention (?:this |it )?to) ` + audience,
		`\b(?:do not|don't|dont|never) tell anyone\b`,
		`\b(?:do not|don't|dont) let ` + audience + ` know\b`,
		`\bwithout (?:telling|informing) (?:anyone\b|` + audience + `)`,
		`\bhide (?:this|it) from ` + audience,
	}, "|"))

	// A shell at the end of a pipe. hasExfiltration then spares a table's cell.
	pipeToShell = regexp.MustCompile(`\|&?[ \t]*(?:sudo(?:[ \t]+-\w+)*[ \t]+)?(?:env[ \t]+)?(?:xargs(?:[ \t]+-\w+)*[ \t]+)?(?:/(?:usr/)?(?:local/)?bin/)?(?:ba|z|da|k|fi|a)?sh\b`)
	// A path that holds a credential: under a home directory, a mounted secret, or the system's own.
	sensitivePath = `(?:(?:~|\$\{?HOME\}?|/root|/home/[^/\s]+)/\.(?:aws/credentials|ssh/id_|netrc|npmrc|git-credentials|kube/config|docker/config\.json|config/gh/hosts\.yml|config/gcloud/)` +
		`|(?:/var)?/run/secrets/[^\s'"]+|/etc/(?:shadow|gshadow|sudoers)\b|[\s/'"=]\.env(?:$|[\s'".;]))`
	exfiltration = regexp.MustCompile(`(?im)` + strings.Join([]string{
		`\b(?:curl|wget)\b[^\n]*\|&?[ \t]*(?:sudo(?:[ \t]+-\w+)*[ \t]+)?(?:python[\d.]*|perl|ruby|node|php)\b`,
		`(?:^|[ \t;&|(])(?:(?:ba|z|da|k|a)?sh[ \t]+(?:-c[ \t]+)?|eval[ \t]+|source[ \t]+|\.[ \t]+)["']?(?:\$\(|<\(|` + "`" + `)[ \t]*(?:curl|wget)\b`,
		`\b(?:curl|wget)\b[^\n]*&&[ \t]*(?:ba|z|da|k|a)?sh[ \t]`,
		`\bgh\s+auth\s+(?:token\b|status\s+[^\n]*--show-token)`,
		`\bcurl\b[^\n]*(?:[ \t]-d[ \t]*@|--data(?:-binary|-raw|-urlencode|-ascii)?[= \t]+@|--upload-file\b|[ \t]-T[ \t]|[ \t](?:-F|--form)[ \t]*\S*=@)`,
		`\b(?:curl|wget)\b[^\n]*\$\((?:printenv|env|cat)\b`,
		`\b(?:cat|less|more|head|tail|base64|xxd|od|strings|grep|cp|scp|tar|nc|curl|awk|sed|python[\d.]*|perl|ruby|open)\b[^\n|;]*` + sensitivePath,
		`/proc/(?:self|thread-self|\d+)/environ\b`,
	}, "|"))

	encodedRun  = regexp.MustCompile(`[A-Za-z0-9+/_-]{12,}={0,2}`)
	wrappedRun  = regexp.MustCompile(`(?m)(?:^[ \t>]*[A-Za-z0-9+/_-]{16,}[ \t]*\n)+[ \t>]*[A-Za-z0-9+/_-]{4,}={0,2}[ \t]*$`)
	escapedRun  = regexp.MustCompile(`(?i)(?:\\x[0-9a-f]{2}){4,}|(?:\\u[0-9a-f]{4}){4,}`)
	codeFence   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	emphasis    = strings.NewReplacer("*", "", "~~", "")
	phraseMarks = strings.NewReplacer("_", " ", "-", " ", "`", "", "\u2019", "'")
)

// Check returns the names of the rules text breaks, each once and in the rules' order, nil for none.
// It takes the whole of an entry and never returns the text it matched. Its regular expressions are
// RE2: linear, whatever the input.
func Check(text string) []string {
	hits := map[string]bool{}
	screen(text, hits)
	values, err := frontmatterValues(text)
	if err != nil {
		hits[RuleMalformedFrontmatter] = true
	}
	for _, v := range values {
		screen(v, hits)
	}
	var rules []string
	for _, r := range ruleOrder {
		if hits[r] {
			rules = append(rules, r)
		}
	}
	return rules
}

// screen runs every rule over s, in the views each one reads.
func screen(s string, hits map[string]bool) {
	v := viewsOf(s)
	if v.hasMarkup() {
		hits[RuleChatMarkup] = true
	}
	if v.hasOverride() {
		hits[RuleInstructionOverride] = true
	}
	if hasHidden(s) {
		hits[RuleHiddenCharacter] = true
	}
	if hasEncoded(outsideCode(v.folded), maxDecodeDepth) {
		hits[RuleEncodedPayload] = true
	}
	if v.hasExfiltration() {
		hits[RuleExfiltration] = true
	}
}

// views are s as the rules read it. folded has HTML entities, compatibility forms (NFKC: fullwidth
// markup, math letters), line endings and Unicode spaces folded, and keeps code fences intact for the
// encoded rule. line also drops markdown emphasis. flat has one space for any run of whitespace, line
// splits included. phrase is flat in lower case, with "_" and "-" as spaces, for the rules on sentences.
type views struct{ folded, line, flat, phrase string }

func viewsOf(s string) views {
	folded := norm.NFKC.String(html.UnescapeString(s))
	folded = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.Is(unicode.Zs, r) {
			return ' '
		}
		return r
	}, strings.ReplaceAll(folded, "\r\n", "\n"))
	line := emphasis.Replace(folded)
	flat := strings.Join(strings.Fields(line), " ")
	phrase := strings.Join(strings.Fields(phraseMarks.Replace(strings.ToLower(flat))), " ")
	return views{folded: folded, line: line, flat: flat, phrase: phrase}
}

func (v views) hasMarkup() bool   { return chatLine.MatchString(v.line) || chatFlat.MatchString(v.flat) }
func (v views) hasOverride() bool { return override.MatchString(v.phrase) }

func (v views) hasExfiltration() bool {
	if exfiltration.MatchString(v.line) {
		return true
	}
	for _, loc := range pipeToShell.FindAllStringIndex(v.line, -1) {
		if !inTableCell(v.line, loc) {
			return true
		}
	}
	return false
}

// inTableCell reports that a match of pipeToShell is a markdown table's cell, "| sh |" or "| bash
// script |": on a line that opens and closes with a pipe, with no flag, redirect or end of line after
// it. So "| sh | tee log", "| bash -c x |" and a pipeline's continuation line still count.
func inTableCell(s string, loc []int) bool {
	start := strings.LastIndexByte(s[:loc[0]], '\n') + 1
	end := len(s)
	if i := strings.IndexByte(s[loc[1]:], '\n'); i >= 0 {
		end = loc[1] + i
	}
	line := strings.TrimSpace(s[start:end])
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return false
	}
	after := strings.TrimSpace(s[loc[1]:end])
	return after != "" && (after[0] == '|' || unicode.IsLetter(rune(after[0])))
}

// frontmatterValues is every scalar of text's frontmatter, keys included, decoded as a reader decodes
// them: the writer (yaml.v3, okf.Render) escapes tag characters, ESC and BOM, and a human may escape
// anything, so the stored form shows neither. Aliases are not followed: the anchor they point to is
// in the tree already, so a billion-laughs document costs nothing here.
func frontmatterValues(text string) ([]string, error) {
	fm, _ := catalog.SplitFrontmatter([]byte(text))
	if fm == nil {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil, err
	}
	var values []string
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			values = append(values, n.Value)
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return values, nil
}

// hasHidden reports a character that renders as nothing yet reaches the model: every default-ignorable
// code point and control but tab and newline (the set Maku's sanitiser strips), private-use characters
// and noncharacters. Two sequences stay, since ordinary text carries them: CRLF, and one emoji
// variation selector after a symbol or digit. After a letter, or in a run, it is a channel for bytes.
func hasHidden(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '\n' || r == '\t':
		case r == '\r':
			if i+1 == len(runes) || runes[i+1] != '\n' {
				return true
			}
		case r == emojiSelector:
			if i == 0 || !unicode.In(runes[i-1], unicode.S, unicode.Nd) {
				return true
			}
		case unicode.IsControl(r), unicode.In(r, unicode.Cf, unicode.Co, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point, unicode.Noncharacter_Code_Point),
			r == lineSeparator, r == paragraphSeparator:
			return true
		}
	}
	return false
}

// outsideCode is s without its fenced blocks and code spans, whose quoting is what the rule on encoded
// runs lets through. A span closes on its own line. A fence nothing closes is no fence: one stray
// marker must not exempt the rest of the file.
func outsideCode(s string) string {
	var b, fenced strings.Builder
	fence := ""
	for line := range strings.SplitSeq(s, "\n") {
		if m := codeFence.FindStringSubmatch(line); m != nil {
			if fence == "" {
				fence = m[1]
				fenced.Reset()
				fenced.WriteString(line + "\n")
				continue
			}
			if m[1][0] == fence[0] && len(m[1]) >= len(fence) {
				fence = ""
				continue
			}
		}
		if fence != "" {
			fenced.WriteString(line + "\n")
			continue
		}
		b.WriteString(dropSpans(line))
		b.WriteByte('\n')
	}
	if fence != "" {
		b.WriteString(fenced.String())
	}
	return b.String()
}

// dropSpans removes a line's code spans: a backtick run closed by a run of the same length. A run
// nothing closes is literal, as in CommonMark, so what follows it stays.
func dropSpans(line string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(line, '`')
		if i < 0 {
			b.WriteString(line)
			return b.String()
		}
		n := backticks(line[i:])
		b.WriteString(line[:i])
		rest := line[i+n:]
		if end, ok := closingRun(rest, n); ok {
			line = rest[end+n:]
			continue
		}
		b.WriteString(line[i : i+n])
		line = rest
	}
}

// backticks is the length of the backtick run s starts with.
func backticks(s string) int {
	return len(s) - len(strings.TrimLeft(s, "`"))
}

// closingRun finds the first backtick run of exactly n in s.
func closingRun(s string, n int) (int, bool) {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := backticks(s[i:])
		if run == n {
			return i, true
		}
		i += run
	}
	return 0, false
}

// hasEncoded reports a run that is a payload for its shape, or that decodes to what a rule catches.
func hasEncoded(s string, depth int) bool {
	for _, run := range escapedRun.FindAllString(s, -1) {
		text, units := decodeEscapes(run)
		if units >= longEscapes || decodedHit(text, depth) {
			return true
		}
	}
	for _, run := range wrappedRun.FindAllString(s, -1) {
		if runIsPayload(strings.Join(strings.FieldsFunc(run, func(r rune) bool { return unicode.IsSpace(r) || r == '>' }), ""), depth) {
			return true
		}
	}
	for _, run := range encodedRun.FindAllString(s, -1) {
		if runIsPayload(run, depth) {
			return true
		}
	}
	return false
}

func runIsPayload(run string, depth int) bool {
	bare := strings.TrimRight(run, "=")
	if raw, err := hex.DecodeString(bare); err == nil && decodedHit(string(raw), depth) {
		return true
	}
	std := strings.NewReplacer("-", "+", "_", "/").Replace(bare)
	if raw, err := base64.RawStdEncoding.DecodeString(std); err == nil && decodedHit(string(raw), depth) {
		return true
	}
	return len(bare) >= longRun && looksEncoded(bare)
}

// decodedHit runs the rules that read words over decoded text. hidden-character does not apply: any
// binary would break it.
func decodedHit(raw string, depth int) bool {
	if depth == 0 {
		return false
	}
	v := viewsOf(strings.ToValidUTF8(raw, ""))
	return v.hasMarkup() || v.hasOverride() || v.hasExfiltration() || hasEncoded(v.folded, depth-1)
}

// decodeEscapes decodes a run of \xNN or \uNNNN, as escapedRun matched it, and counts its units.
func decodeEscapes(run string) (string, int) {
	var b strings.Builder
	units := 0
	for i := 0; i < len(run); units++ {
		size := 4 // \xNN
		if run[i+1] == 'u' || run[i+1] == 'U' {
			size = 6 // \uNNNN
		}
		var r rune
		raw, _ := hex.DecodeString(run[i+2 : i+size]) // hex digits: escapedRun matched them
		for _, c := range raw {
			r = r<<8 | rune(c)
		}
		b.WriteRune(r)
		i += size
	}
	return b.String(), units
}

// looksEncoded tells a payload from a digest, a slug or a long identifier, by shape alone. Hex is a
// payload unless it has a SHA's length, the one exemption, whatever stands before it: real entries
// carry a bare revision. Anything else needs what base64 has and a name does not: lower case, upper
// case and digits, with upper case at least a quarter of the letters. A label such as "sha256:" exempts
// nothing.
func looksEncoded(run string) bool {
	var lower, upper, digit int
	isHex := true
	for _, r := range run {
		switch {
		case r >= 'a' && r <= 'z':
			lower++
			isHex = isHex && r <= 'f'
		case r >= 'A' && r <= 'Z':
			upper++
			isHex = isHex && r <= 'F'
		case r >= '0' && r <= '9':
			digit++
		default:
			isHex = false
		}
	}
	if isHex {
		switch len(run) {
		case 40, 56, 64, 96, 128: // SHA-1, SHA-224, SHA-256, SHA-384, SHA-512
			return false
		}
		return true
	}
	return lower > 0 && digit > 0 && upper*4 >= lower+upper
}
