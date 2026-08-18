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
	// GitLab tokens: personal/project/group access tokens (glpat-), CI/CD job
	// tokens (glcbt-), runner authentication tokens (glrt-), pipeline trigger
	// tokens (glptt-), and feed tokens (glft-) all share this prefixed-random-
	// suffix shape. The forge client sends this value as the PRIVATE-TOKEN
	// header on every request — this rule is the backstop that keeps it from
	// reaching a log even if it ever ends up quoted in tool output or an error.
	// The suffix class includes '.' because GitLab's newer ROUTABLE tokens embed
	// dot-separated segments (`glpat-<payload>.<version>.<crc>`); without it the
	// mask would stop at the first dot and leak the rest of the token verbatim.
	// The trailing \b still anchors the match, so a sentence-ending period after
	// a non-routable token is not swallowed (the engine backtracks off it).
	{regexp.MustCompile(`\bgl(?:pat|cbt|rt|ptt|ft)-[A-Za-z0-9_.-]{20,}\b`), mask},
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
	// ApiKey is Elasticsearch/OpenSearch's own auth scheme (Authorization: ApiKey
	// <base64 id:key>) — the documented workaround for an ES API key, which
	// (unlike a bearer token) has no recognizable prefix of its own to match on.
	//
	// Two rules, deliberately, because "apikey" is also an ordinary English-ish
	// word that shows up in prose all over log lines, runbooks and findings. A
	// single `(?i)(apikey\s+)[A-Za-z0-9+/=]{8,}` rule masked ANY 8+ character
	// word following it — "apikey rotation policy" became "apikey [REDACTED]
	// policy" and "see the ApiKey documentation page" became "ApiKey [REDACTED]
	// page". Planting spurious [REDACTED] markers in a finding is not a safe
	// failure: it makes the investigation output look like it leaked a secret and
	// destroys otherwise-correct text.
	//
	//  1. With the Authorization-header context, ANY credential shape is masked —
	//     "Authorization: ApiKey <x>" is never prose.
	//  2. Bare "ApiKey <x>" (a header dump with the key stripped, a curl snippet)
	//     only when the token is unmistakably a base64 credential: 20+ characters
	//     from the base64 alphabet. A real ES API key is ~50-60; the longest words
	//     that trip rule 1's old form ("documentation", 13) fall well short.
	{regexp.MustCompile(`(?i)(authorization"?\s*[:=]?\s*"?apikey\s+)[A-Za-z0-9._~+/=-]{8,}`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(\bapikey\s+)[A-Za-z0-9+/]{20,}={0,2}`), `${1}[REDACTED]`},
	// Sensitive key = value / key: value (the value is masked, the key kept). An
	// env-var-style prefix (DB_SECRET, OPENAI_API_KEY) is allowed before the keyword.
	{regexp.MustCompile(`(?i)([\w.\-]*(?:` + sensitiveKeywords + `)"?\s*[:=]\s*"?)([^\s"',}]+)`), `${1}[REDACTED]`},
}

// sensitiveKeywords is the vocabulary of key names whose VALUE is treated as a
// credential. It is shared by the generic key=value rule above and by
// SensitiveNameValues below, so the two cannot drift into disagreeing about what
// counts as sensitive.
const sensitiveKeywords = `password|passwd|secret|api[_-]?key|access[_-]?key|secret[_-]?key|` +
	`private[_-]?key|client[_-]?secret|token|credentials?|dsn|connection[_-]?string`

// sensitiveNameRE matches a NAME — not a key — that identifies a credential, for
// the {name, value} shape SensitiveNameValues handles.
//
// It requires the keyword to be a whole SEGMENT of the name (delimited by a
// non-alphanumeric character or an end of string) rather than any substring:
// POSTGRES_PASSWORD and REDIS_AUTH match, while GIT_AUTHOR_NAME ("auth" + "or")
// and TOKENIZER_MODE ("token" + "izer") do not. Masking those would plant a
// spurious [REDACTED] in otherwise-correct evidence, which is not a safe failure.
//
// The vocabulary is the shared one plus three names that only ever appear as an
// identifier and would be too broad as a free-text key: `auth` (REDIS_AUTH), `pwd`
// and `passphrase`.
var sensitiveNameRE = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:` + sensitiveKeywords + `|auth|pwd|passphrase)(?:$|[^a-z0-9])`)

// SensitiveNameValues masks, IN PLACE, the `value` of every {name: …, value: …}
// pair in a decoded object tree (map[string]any / []any, as unmarshalled from
// YAML or JSON) whose `name` is credential-shaped.
//
// It exists because the string ruleset structurally cannot see this shape.
// Secrets is KEY-NAME oriented: it masks `password: hunter2` because "password"
// is the key. Kubernetes' EnvVar inverts that — the sensitive word is the VALUE
// of `name`, and the credential sits under the literal key `value`, which is in
// no keyword vocabulary:
//
//   - name: POSTGRES_PASSWORD
//     value: pr0d-Pa55w0rd-x9      # untouched by Secrets
//
// The walk is over the whole tree rather than a fixed list of paths, because the
// shape appears at spec.containers[] on a Pod, spec.template.spec.containers[] on
// a Deployment, one level deeper again on a CronJob, and anywhere at all in an
// operator CRD that embeds a PodSpec. It is deliberately NOT limited to `env`:
// {name, value} with a credential name means the same thing wherever it appears.
//
// `valueFrom` (a secretKeyRef) is left alone: it names a Secret key rather than
// carrying it, and the model needs to see WHICH secret a workload consumes.
//
// Idempotent: a value that is already the mask is left as it is.
func SensitiveNameValues(v any) {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && sensitiveNameRE.MatchString(name) {
			// Any type of value, not just a string: a CRD may carry {name, value}
			// with a non-string value, and masking must not depend on the shape of
			// what is being masked.
			if cur, has := t["value"]; has && cur != nil && cur != mask {
				t["value"] = mask
			}
		}
		for _, child := range t {
			SensitiveNameValues(child)
		}
	case []any:
		for _, child := range t {
			SensitiveNameValues(child)
		}
	}
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
// capturing indentation, the "key:" portion, and the value. An entry also OWNS
// every following line indented deeper than its key — see k8sSecretData.
var dataEntryRE = regexp.MustCompile(`^(\s*)([^\s:][^:]*:\s*)(\S.*)$`)

// blockScalarHeaderRE matches a YAML block-scalar header used as an entry's
// value: `|` or `>` plus the optional indentation/chomping indicators (`|-`,
// `|+`, `>2-`) and an optional trailing comment. The header is STRUCTURE, not
// value — it says "a multi-line value was here" and nothing about its content —
// so it is preserved while the lines it owns are masked.
var blockScalarHeaderRE = regexp.MustCompile(`^[|>][0-9+-]{0,2}(\s+#.*)?$`)

// k8sSecretData performs a line-oriented pass that masks the VALUES under a
// `data:`/`stringData:` block of a `kind: Secret` document, preserving keys and
// all surrounding structure. Non-Secret documents (e.g. kind: ConfigMap) are
// left untouched. It tolerates git-diff line markers ("+ ", "- ", leading
// space) because a Secret most often surfaces inside a `what_changed` diff.
//
// A value may be MULTI-LINE: `key: |` (or `>`, or a plain scalar continued on
// the next line) puts the secret on the following, more-indented lines. Every
// line indented deeper than an entry's key therefore belongs to that entry and
// is masked in place, indentation preserved. Blank lines belong to the value too
// (YAML block scalars keep them), so they do not end the block — otherwise
// everything after the first blank line would come through verbatim. The
// ownership test is the indent, never the `|` marker: the generic key=value rule
// runs BEFORE this pass and rewrites `password: |` to `password: [REDACTED]`,
// erasing the marker while leaving the body — which is exactly the partial mask
// this handling exists to close.
//
// Every masked SINGLE-LINE value is also LEARNED (second return): the raw token
// and, for a base64 `data:` value, its decoded plaintext. The caller scrubs those
// literals from the whole payload — the same secret quoted decoded in a log line
// or encoded in an event must not outlive the manifest that names it. Multi-line
// bodies are deliberately NOT learned: their lines are fragments of one value,
// and scrubbing a fragment payload-wide would mask ordinary text (a
// `retries: 3` line out of an embedded config file) everywhere else it appears.
// Masking in place stops the leak; scrubbing fragments would only blind the model.
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
	entryIndent := -1    // indent of the open entry key; it owns deeper lines (-1: none)
	stringData := false  // the current block is stringData: (plaintext values)
	var learned []string // secret literals to scrub payload-wide

	for i, raw := range lines {
		m := diffPrefixRE.FindStringSubmatch(raw)
		prefix, body := m[1], m[2]

		// Document boundary: a separator resets all document state.
		if docMarkerRE.MatchString(body) {
			inSecret, inDataBlock, entryIndent = false, false, -1
			continue
		}

		// A new document's kind: line (re)sets whether we are in a Secret.
		if kindAnyRE.MatchString(body) {
			inSecret = kindSecretRE.MatchString(body)
			inDataBlock, entryIndent = false, -1
			continue
		}

		if !inSecret {
			continue
		}

		if inDataBlock {
			indent := leadingSpaces(body)
			blank := strings.TrimSpace(body) == ""

			// Continuation of the open entry's value: deeper than its key, or
			// blank (a blank line is part of a block scalar, so it must not end
			// the value — see the doc comment).
			if entryIndent >= 0 && (blank || indent > entryIndent) {
				if txt := strings.TrimSpace(body); txt != "" && txt != mask {
					lines[i] = prefix + body[:indent] + mask
				}
				continue
			}
			entryIndent = -1

			// No entry open: a blank line ends the block (blank lines between
			// indented data keys are not a shape real manifests produce).
			if blank {
				inDataBlock = false
				continue
			}
			if indent <= dataIndent {
				inDataBlock = false
				// fall through: this line may itself open another data block or
				// be a sibling key — re-evaluate below.
			} else {
				if entry := dataEntryRE.FindStringSubmatch(body); entry != nil {
					// The entry now owns every following deeper-indented line.
					entryIndent = leadingSpaces(entry[1])
					val := strings.TrimRight(entry[3], " ")
					switch {
					case blockScalarHeaderRE.MatchString(val):
						// Keep the header; the lines it owns are masked above.
					case val == mask:
						// Already masked (this pass, or a rule that ran before it).
					default:
						learned = append(learned, learnSecretValues(val, stringData)...)
						lines[i] = prefix + entry[1] + entry[2] + mask
					}
				}
				continue
			}
		}

		// Detect the start of a data:/stringData: block within the Secret.
		if dk := dataKeyRE.FindStringSubmatch(body); dk != nil {
			inDataBlock, entryIndent = true, -1
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
