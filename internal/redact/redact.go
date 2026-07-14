// SPDX-License-Identifier: Apache-2.0

// Package redact masks secret-shaped values in free text before it crosses a
// trust boundary — specifically before tool output (pod/controller logs, git
// diffs, status/event messages) is fed to the LLM provider, from where the
// model's quoted evidence would otherwise flow on into a (possibly public) KB
// pull request and chat. It is deliberately HIGH-PRECISION: it targets clearly
// secret-shaped tokens and sensitive key=value pairs, masking the *value* while
// preserving surrounding structure (the key name, the diff line) so the
// investigation can still reason ("the password field changed") without the
// secret leaving the boundary. It is not a guarantee — redaction is a mitigation,
// not a substitute for not logging secrets.
package redact

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
)

const mask = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string // may reference ${1}, ${2}
}

// rules run in order; each is independent and idempotent over already-masked text.
var rules = []rule{
	// PEM private key blocks (RSA/EC/OPENSSH/PGP/…).
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
	// JWT (header.payload.signature, base64url).
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), mask},
	// GitHub tokens (ghp_/gho_/ghu_/ghs_/ghr_).
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), mask},
	// GitHub fine-grained personal access token.
	{regexp.MustCompile(`\bgithub_pat_[0-9A-Za-z_]{22,}\b`), mask},
	// OpenAI / Anthropic-style keys (anchored so a benign "sk-" inside a word
	// like "task-management" is not matched).
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`), mask},
	// Stripe live keys (secret / restricted).
	{regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{16,}\b`), mask},
	// Google OAuth access token.
	{regexp.MustCompile(`\bya29\.[0-9A-Za-z_-]{20,}`), mask},
	// Slack tokens.
	{regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), mask},
	// AWS access key id.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), mask},
	// AWS secret access key, but ONLY when an AWS context cue is adjacent. The
	// ':' / '=' separated forms (aws_secret_access_key=..., "SecretAccessKey":
	// "...") are already covered by the generic key=value rule below; this adds
	// the whitespace-separated case while deliberately NOT introducing a bare
	// [A-Za-z0-9/+]{40} rule, which would false-positive on git SHAs and
	// base64 log blobs.
	{regexp.MustCompile(`(?i)(aws[_-]?secret[_-]?access[_-]?key\s+)[A-Za-z0-9/+]{40}\b`), `${1}[REDACTED]`},
	// Google API key.
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), mask},
	// Credentials in a URL: scheme://user:PASSWORD@host → mask the password.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:@/]+:)[^\s@/]+(@)`), `${1}[REDACTED]${2}`},
	// HTTP auth header tokens — keep the scheme, mask the credential.
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+/=]{8,}`), `${1}[REDACTED]`},
	// Sensitive key = value / key: value (the value is masked, the key kept). An
	// env-var-style prefix (DB_SECRET, OPENAI_API_KEY) is allowed before the keyword.
	{regexp.MustCompile(`(?i)([\w.\-]*(?:password|passwd|secret|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|client[_-]?secret|token|credentials?|dsn|connection[_-]?string)"?\s*[:=]\s*"?)([^\s"',}]+)`), `${1}[REDACTED]`},
}

// Secrets masks secret-shaped substrings in s, returning the redacted text.
// It is safe to call on already-redacted text (idempotent).
func Secrets(s string) string {
	if s == "" {
		return s
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	s, learned := k8sSecretData(s)
	return scrubLearned(s, learned)
}

// diffPrefixRE matches an optional git-diff line marker ("+ ", "- ", or a single
// leading space used for context lines). It is captured so the marker can be
// preserved while the YAML body is inspected/rewritten.
var diffPrefixRE = regexp.MustCompile(`^([+\- ]?)(.*)$`)

// docMarkerRE matches a YAML document separator ("---", possibly trailing
// content) after any diff marker has been stripped.
var docMarkerRE = regexp.MustCompile(`^---(\s.*)?$`)

// kindSecretRE matches a top-level `kind: Secret` line (with optional quoting),
// after any diff marker has been stripped. It deliberately anchors at the start
// of the (de-marked) line so an inner "kind:" value cannot trip it.
var kindSecretRE = regexp.MustCompile(`^kind:\s*["']?Secret["']?\s*$`)

// kindAnyRE matches any top-level `kind:` line, used to detect a switch to a
// non-Secret document.
var kindAnyRE = regexp.MustCompile(`^kind:\s*\S`)

// dataKeyRE matches a `data:` or `stringData:` mapping key (no inline value),
// capturing its indentation and which of the two it is (data: values are
// base64, stringData: values are plaintext). Anchored after diff-marker
// stripping.
var dataKeyRE = regexp.MustCompile(`^(\s*)(data|stringData):\s*$`)

// dataEntryRE matches a `  key: value` mapping entry inside a data block,
// capturing indentation, the "key:" portion, and the value. Block scalars
// (`key: |` / `key: >`) are handled separately so their following lines are
// masked too.
var dataEntryRE = regexp.MustCompile(`^(\s*)([^\s:][^:]*:\s*)(\S.*)$`)

// k8sSecretData performs a line-oriented pass that masks the VALUES under a
// `data:`/`stringData:` block of a `kind: Secret` document, preserving keys and
// all surrounding structure. Non-Secret documents (e.g. kind: ConfigMap) are
// left untouched. It tolerates git-diff line markers ("+ ", "- ", leading
// space) because a Secret most often surfaces inside a `what_changed` diff.
//
// Every masked value is also LEARNED (second return): the raw token and, for a
// base64 `data:` value, its decoded plaintext. The caller scrubs those literals
// from the whole payload — the same secret quoted decoded in a log line or
// encoded in an event must not outlive the manifest that names it.
//
// A data block ends at: a dedent to a column <= the data key's indent, a new
// top-level key, a `kind:` line, or a YAML document separator ("---"). The pass
// is idempotent: once a value is the mask string it stays the mask string.
func k8sSecretData(s string) (string, []string) {
	if !strings.Contains(s, "kind:") {
		return s, nil
	}
	// Preserve a trailing-newline / no-trailing-newline shape exactly.
	lines := strings.Split(s, "\n")

	inSecret := false    // current YAML document is a kind: Secret
	inDataBlock := false // currently inside that Secret's data:/stringData: block
	dataIndent := 0      // indent (in columns) of the data: key
	stringData := false  // the current block is stringData: (plaintext values)
	var learned []string // secret literals to scrub payload-wide

	for i, raw := range lines {
		m := diffPrefixRE.FindStringSubmatch(raw)
		prefix, body := m[1], m[2]

		// Document boundary: a separator resets all document state.
		if docMarkerRE.MatchString(body) {
			inSecret, inDataBlock = false, false
			continue
		}

		// A new document's kind: line (re)sets whether we are in a Secret.
		if kindAnyRE.MatchString(body) {
			inSecret = kindSecretRE.MatchString(body)
			inDataBlock = false
			continue
		}

		if !inSecret {
			continue
		}

		if inDataBlock {
			indent := leadingSpaces(body)
			// Block ends on dedent to <= the data key indent, or a blank line
			// that is not part of the block. Blank lines inside indented data
			// are unusual; treat a blank line as ending the block.
			if strings.TrimSpace(body) == "" {
				inDataBlock = false
				continue
			}
			if indent <= dataIndent {
				inDataBlock = false
				// fall through: this line may itself open another data block or
				// be a sibling key — re-evaluate below.
			} else {
				if entry := dataEntryRE.FindStringSubmatch(body); entry != nil {
					val := strings.TrimRight(entry[3], " ")
					if val != mask {
						learned = append(learned, learnSecretValues(val, stringData)...)
						lines[i] = prefix + entry[1] + entry[2] + mask
					}
				}
				continue
			}
		}

		// Detect the start of a data:/stringData: block within the Secret.
		if dk := dataKeyRE.FindStringSubmatch(body); dk != nil {
			inDataBlock = true
			dataIndent = leadingSpaces(dk[1])
			stringData = dk[2] == "stringData"
			continue
		}
	}
	return strings.Join(lines, "\n"), learned
}

// minLearnedSecretLen is the floor for payload-wide scrubbing of a learned
// secret literal. Below it, the risk flips: masking every occurrence of a short
// common value ("prod", "true") would blind the model to benign evidence, while
// real secrets this short are rare. The block value itself is masked regardless.
const minLearnedSecretLen = 6

// learnSecretValues extracts the literals to scrub payload-wide from one
// data-block value: the raw token (surrounding quotes stripped) and, for a
// base64 `data:` value, its decoded plaintext. stringData values are plaintext
// by definition — no decode step.
func learnSecretValues(val string, stringData bool) []string {
	tok := strings.Trim(val, `"'`)
	if !stringData {
		// data: values are single base64 tokens; drop anything after whitespace
		// (a trailing YAML comment) so the decode sees only the blob.
		if f := strings.Fields(tok); len(f) > 0 {
			tok = strings.Trim(f[0], `"'`)
		}
	}
	var out []string
	if len(tok) >= minLearnedSecretLen && tok != mask {
		out = append(out, tok)
	}
	if !stringData {
		dec, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			dec, err = base64.RawStdEncoding.DecodeString(tok)
		}
		if err == nil && len(dec) >= minLearnedSecretLen {
			out = append(out, string(dec))
		}
	}
	return out
}

// scrubLearned masks every occurrence of the learned secret literals in s.
// Longest first, so a literal containing another is masked whole rather than
// left as a recognizable fragment around an inner mask.
func scrubLearned(s string, learned []string) string {
	if len(learned) == 0 {
		return s
	}
	sort.Slice(learned, func(i, j int) bool { return len(learned[i]) > len(learned[j]) })
	for _, lit := range learned {
		s = strings.ReplaceAll(s, lit, mask)
	}
	return s
}

// leadingSpaces counts leading space/tab characters (column-ish indent). Tabs
// are invalid YAML indentation, so counting them as one each is sufficient for
// the dedent comparison.
func leadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' || r == '\t' {
			n++
			continue
		}
		break
	}
	return n
}
