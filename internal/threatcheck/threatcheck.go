// SPDX-License-Identifier: Apache-2.0

// Package threatcheck screens text for what is meant to steer the agents that later read it. A
// knowledge-base entry quotes logs and alert labels, and every factory agent reads entries, so a
// crafted log line would otherwise become a stored injection.
//
// It is deterministic and offline on purpose: a model would be the thing being attacked. And it is a
// screen for obvious injection, not a proof of safety: rewording, another language, homoglyphs or a
// payload in a code span pass it. The boundary stays that every reader receives entries as quoted
// data. A false positive costs a human look, so the rules lean to catching, and stay narrow enough
// for an ordinary incident entry to pass.
package threatcheck

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The rules, in the order Check reports them.
const (
	// RuleChatMarkup is chat or role markup: <|im_start|>, [INST], <system>, </tool_result>,
	// <function_calls>, or a line beginning "system:" or "assistant:".
	RuleChatMarkup = "chat-markup"
	// RuleInstructionOverride is a sentence aimed at an agent: "ignore previous instructions",
	// "you are now", "disregard the above", "do not tell the user".
	RuleInstructionOverride = "instruction-override"
	// RuleHiddenCharacter is a character a reader does not see and a model does: a Unicode format
	// or control character other than tab and newline, or text that is not UTF-8.
	RuleHiddenCharacter = "hidden-character"
	// RuleEncodedPayload is a long base64 or hex run outside a code span, unless it has the shape of
	// a SHA or digest.
	RuleEncodedPayload = "encoded-payload"
	// RuleExfiltration is a shell shape that runs fetched code or reads a credential: curl into a
	// shell, a pipe to a shell, gh auth token, reading a credentials path.
	RuleExfiltration = "exfiltration"
)

// The code points hasHidden names.
const (
	emojiSelector      = 0xFE0F
	lineSeparator      = 0x2028
	paragraphSeparator = 0x2029
)

// maxDigestLabel bounds how far back of a run hasEncoded looks for its label: "sha512: ".
const maxDigestLabel = 8

var (
	chatMarkup = regexp.MustCompile(`(?im)` + strings.Join([]string{
		`<\|[^|\n]{0,64}\|>`,
		`\[/?INST\]`,
		`<</?SYS>>`,
		`</?(?:system|system[_-]prompt|assistant|developer|instructions|tool_use|tool_calls?|tool_results?|tool_responses?|function_calls?|function_results?|function_responses?|start_of_turn|end_of_turn)\b[^<>\n]*>`,
		`</(?:user|human)\s*>`,
		`<invoke\b[^<>\n]*>`,
		// A line that opens a turn. "user:" and "tool:" are not here, nor "system:" with no space
		// after it: kubeconfig keys and RBAC subjects (system:masters) are ordinary text.
		`^[ \t>*_#-]*(?:system|assistant|developer|human)[ \t]*:(?:[*_]*[ \t]|[*_]*$)`,
		`^[ \t>]*#{1,6}[ \t]*instructions?[ \t]*:`,
	}, "|"))

	// A verb, the words that may stand between it and its object, and the object: "ignore previous
	// alerts" and "ignore the upgrade instructions" read as ordinary incident prose.
	overrideVerb   = `\b(?:ignore|disregard|forget|override|bypass)\s+`
	overrideFiller = `(?:(?:all|any|every|everything|of|the|your|my|these|those|previous|prior|earlier|above|preceding|foregoing|original|former|system)\s+)*`
	overridePrior  = `(?:previous|prior|earlier|above|preceding|foregoing|original|former)`
	override       = regexp.MustCompile(`(?im)` + strings.Join([]string{
		overrideVerb + overrideFiller + `(?:instructions?|prompts?)\b`,
		overrideVerb + overrideFiller + overridePrior + `\s+(?:rules?|directions?|directives?|guidance|guidelines?|context|messages?|commands?|text|conversation|content)\b`,
		overrideVerb + `(?:(?:all|everything|anything|of|the)\s+)*(?:above|preceding)\s*(?:[.,;:!?)]|and\b|$)`,
		`\byou\s+are\s+now\b`,
		`\b(?:do\s+not|don['\x{2019}]?t|never)\s+(?:tell|inform|show|reveal\s+to|mention\s+(?:this\s+|it\s+)?to)\s+(?:the\s+|your\s+)?(?:user|human|operator|maintainer|reviewer)\b`,
		`\bnew\s+instructions\s*:`,
	}, "|"))

	// A shell at the end of a pipe: what follows it is an end of line, a separator, a quote or a
	// flag, so a table cell ("| sh |") and a word ("| show") do not read as one.
	pipeToShell = `\|[ \t]*(?:sudo[ \t]+)?(?:env[ \t]+)?(?:/(?:usr/)?(?:local/)?bin/)?(?:ba|z|da|k|fi|a)?sh\b[ \t]*(?:$|[;&)"'` + "`" + `]|-)`
	// A path that holds a credential, under a home directory or a pod's service account.
	sensitivePath = `(?:(?:~|\$\{?HOME\}?|/root|/home/[^/\s]+)/\.(?:aws/credentials|ssh/id_|netrc|npmrc|git-credentials|kube/config|docker/config\.json|config/gh/hosts\.yml|config/gcloud/)|/var/run/secrets/kubernetes\.io/serviceaccount/token)`
	exfiltration  = regexp.MustCompile(`(?im)` + strings.Join([]string{
		pipeToShell,
		`\b(?:curl|wget)\b[^\n|]*\|[ \t]*(?:sudo[ \t]+)?(?:python[\d.]*|perl|ruby|node|php)\b`,
		`\b(?:(?:ba|z|da|k|a)?sh[ \t]+(?:-c[ \t]+)?|eval[ \t]+)["']?(?:\$\(|<\(|` + "`" + `)[ \t]*(?:curl|wget)\b`,
		`\bgh\s+auth\s+(?:token\b|status\s+[^\n]*--show-token)`,
		`\bcurl\b[^\n]*(?:[ \t]-d[ \t]*@|--data(?:-binary|-raw|-urlencode|-ascii)?[= \t]+@|--upload-file\b|[ \t]-T[ \t])`,
		`\b(?:cat|less|more|head|tail|base64|xxd|od|strings|grep|cp|scp|tar|nc|curl)\b[^\n|;]*` + sensitivePath,
		`/proc/(?:self|\d+)/environ\b`,
	}, "|"))

	encodedRun = regexp.MustCompile(`[A-Za-z0-9+/_-]{40,}={0,2}`)
	escapedHex = regexp.MustCompile(`(?i)(?:\\x[0-9a-f]{2}){16,}`)
	// A digest's label: right before a run, or at its start where the separator is in the alphabet.
	digestLabel  = regexp.MustCompile(`(?i)(?:sha\d+|md5)[:=] ?$`)
	digestPrefix = regexp.MustCompile(`(?i)^(?:sha\d+|md5)-`)
	codeFence    = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
)

// Check returns the names of the rules text breaks, each once and in the rules' order, nil for none.
// It takes the whole of an entry, frontmatter included, and never returns the text it matched. Its
// regular expressions are RE2: linear, whatever the input.
func Check(text string) []string {
	var rules []string
	// NFKC first: fullwidth markup and a fullwidth "ignore" fold to what the rules name. CRLF ends a
	// line, for the rules that look for one.
	folded := strings.ReplaceAll(norm.NFKC.String(text), "\r\n", "\n")
	if chatMarkup.MatchString(folded) {
		rules = append(rules, RuleChatMarkup)
	}
	if override.MatchString(folded) {
		rules = append(rules, RuleInstructionOverride)
	}
	if hasHidden(text) {
		rules = append(rules, RuleHiddenCharacter)
	}
	if hasEncoded(outsideCode(text)) {
		rules = append(rules, RuleEncodedPayload)
	}
	if exfiltration.MatchString(folded) {
		rules = append(rules, RuleExfiltration)
	}
	return rules
}

// hasHidden reports a character that renders as nothing yet reaches the model, the set Maku's
// sanitiser strips: every default-ignorable code point, and controls but tab and newline. Two
// sequences stay, since ordinary text carries them: CRLF, and one emoji variation selector after a
// character (a run of them, or any other selector, is how bytes get smuggled).
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
			if i == 0 || unicode.Is(unicode.Variation_Selector, runes[i-1]) {
				return true
			}
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == lineSeparator, r == paragraphSeparator,
			unicode.Is(unicode.Variation_Selector, r), unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
			return true
		}
	}
	return false
}

// outsideCode is s without its fenced blocks and code spans, whose quoting is what the rule on encoded
// runs lets through. A span closes on its own line, and an unclosed fence runs to the end.
func outsideCode(s string) string {
	var b strings.Builder
	fence := ""
	for line := range strings.SplitSeq(s, "\n") {
		if m := codeFence.FindStringSubmatch(line); m != nil {
			if fence == "" {
				fence = m[1]
				continue
			}
			if m[1][0] == fence[0] && len(m[1]) >= len(fence) {
				fence = ""
				continue
			}
		}
		if fence == "" {
			b.WriteString(dropSpans(line))
			b.WriteByte('\n')
		}
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

func hasEncoded(s string) bool {
	if escapedHex.MatchString(s) {
		return true
	}
	for _, loc := range encodedRun.FindAllStringIndex(s, -1) {
		// Only the bytes a label can fill: a scan back to the start, per run, would be quadratic.
		if !digestLabel.MatchString(s[max(0, loc[0]-maxDigestLabel):loc[0]]) && looksEncoded(s[loc[0]:loc[1]]) {
			return true
		}
	}
	return false
}

// looksEncoded tells a payload from a digest, a slug or a long identifier. Hex is a payload unless it
// has a SHA's length. Anything else needs what base64 has and a name does not: lower case, upper case
// and digits, with upper case at least a quarter of the letters.
func looksEncoded(run string) bool {
	if digestPrefix.MatchString(run) {
		return false
	}
	var lower, upper, digit int
	hex := true
	for _, r := range run {
		switch {
		case r >= 'a' && r <= 'z':
			lower++
			hex = hex && r <= 'f'
		case r >= 'A' && r <= 'Z':
			upper++
			hex = hex && r <= 'F'
		case r >= '0' && r <= '9':
			digit++
		default:
			hex = false
		}
	}
	if hex {
		switch len(run) {
		case 40, 56, 64, 96, 128: // SHA-1, SHA-224, SHA-256, SHA-384, SHA-512
			return false
		}
		return true
	}
	return lower > 0 && digit > 0 && upper*4 >= lower+upper
}
