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

import "regexp"

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
	// OpenAI / Anthropic-style keys.
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`), mask},
	// Slack tokens.
	{regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), mask},
	// AWS access key id.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), mask},
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
	return s
}
