// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSecretsMasks(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	cases := []struct {
		name  string
		in    string
		gone  string // substring that must NOT survive
		keeps string // structure that SHOULD survive (optional)
	}{
		{"github token", "found ghp_0123456789abcdefghijABCDEFGHIJ0123 here", "ghp_0123456789abcdefghijABCDEFGHIJ0123", "found"},
		{"github fine-grained pat", "token github_pat_11ABCDE0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGH used", "github_pat_11ABCDE0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGH", "token"},
		{"openai key", "OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwx", "sk-abcdefghijklmnopqrstuvwx", ""},
		{"openai key mid sentence", "the key sk-abcdefghijklmnopqrstuvwx is here", "sk-abcdefghijklmnopqrstuvwx", "the key"},
		{"stripe live secret key", "stripe sk_live_0123456789abcdefABCDEF here", "sk_live_0123456789abcdefABCDEF", "stripe"},
		{"stripe live restricted key", "stripe rk_live_0123456789abcdefABCDEF here", "rk_live_0123456789abcdefABCDEF", "stripe"},
		{"google oauth token", "Authorization uses ya29.A0ARrdaM9abcdefghij_klmnopqrstuvw-XYZ123 today", "ya29.A0ARrdaM9abcdefghij_klmnopqrstuvw-XYZ123", "today"},
		{"slack token", "token xoxb-123456789012-abcdefuvwxyz", "xoxb-123456789012-abcdefuvwxyz", ""},
		{"aws key id", "AccessKeyId: " + secret, secret, "AccessKeyId"},
		{"aws secret kv equals", "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws_secret_access_key"},
		{"aws secret kv quoted json", `"aws_secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws_secret_access_key"},
		{"aws secret cue whitespace", "aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws_secret_access_key"},
		{"jwt", "auth eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "eyJzdWIiOiIxIn0", ""},
		{"password kv", `password: hunter2horse`, "hunter2horse", "password"},
		{"secret env", "DB_SECRET=s3cr3t-value-xyz", "s3cr3t-value-xyz", ""},
		{"url creds", "postgres://app:sup3rs3cret@db.svc:5432/x", "sup3rs3cret", "postgres://app:"},
		{"bearer", "Authorization: Bearer abcDEF123456ghiJKL789", "abcDEF123456ghiJKL789", "Bearer"},
		{"private key", "k:\n-----BEGIN RSA PRIVATE KEY-----\nMIIBwetcetc\n-----END RSA PRIVATE KEY-----\n", "MIIBwetcetc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Secrets(tc.in)
			if strings.Contains(out, tc.gone) {
				t.Fatalf("secret survived redaction: %q -> %q", tc.in, out)
			}
			if !strings.Contains(out, "[REDACTED") {
				t.Fatalf("expected a redaction marker, got %q", out)
			}
			if tc.keeps != "" && !strings.Contains(out, tc.keeps) {
				t.Fatalf("structure %q should survive, got %q", tc.keeps, out)
			}
			// Idempotent: redacting again changes nothing.
			if again := Secrets(out); again != out {
				t.Fatalf("not idempotent: %q -> %q", out, again)
			}
		})
	}
}

// A Secret's data: values are KNOWN secrets — masking them in the block is not
// enough (REDACT-B64): the same material elsewhere in the payload, decoded in a
// log line or base64-encoded in an event, must be scrubbed too. This is the
// tractable half of the disclosed "never decodes base64" gap: with the manifest
// in the payload, we have ground truth and can scrub with full precision.
func TestSecretsKnownSecretScrubbedPayloadWide(t *testing.T) {
	plain := "hunter2-stallion"
	blob := base64.StdEncoding.EncodeToString([]byte(plain))
	in := "kind: Secret\nmetadata:\n  name: db\ndata:\n  pw: " + blob + "\n  quoted: \"" + blob + "\"\n---\n" +
		"log line: login failed for " + plain + " on db-0\n" +
		"event: mounted value " + blob + " into pod\n"
	out := Secrets(in)
	if strings.Contains(out, plain) {
		t.Fatalf("decoded data: value survived elsewhere in the payload:\n%s", out)
	}
	if strings.Contains(out, blob) {
		t.Fatalf("base64 data: value survived elsewhere in the payload:\n%s", out)
	}
	for _, keep := range []string{"login failed for", "on db-0", "mounted value", "into pod"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("structure %q should survive, got:\n%s", keep, out)
		}
	}
	if again := Secrets(out); again != out {
		t.Fatalf("not idempotent:\n%s\n->\n%s", out, again)
	}
}

// stringData: values are plaintext secrets by position — the same payload-wide
// scrub applies without a decode step.
func TestSecretsStringDataValueScrubbedPayloadWide(t *testing.T) {
	plain := "plaintext-cred-77"
	in := "kind: Secret\nstringData:\n  cred: " + plain + "\n---\nmsg says " + plain + " was used\n"
	out := Secrets(in)
	if strings.Contains(out, plain) {
		t.Fatalf("stringData value survived elsewhere in the payload:\n%s", out)
	}
	if !strings.Contains(out, "msg says") || !strings.Contains(out, "was used") {
		t.Fatalf("structure should survive, got:\n%s", out)
	}
}

// Precision guard: a decoded value shorter than the learning floor ("prod",
// "true") is NOT scrubbed payload-wide — masking every occurrence of a short
// common word would blind the model to benign evidence. The block value itself
// is still masked.
func TestSecretsShortDecodedValueNotScrubbedGlobally(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte("prod")) // cHJvZA==
	in := "kind: Secret\ndata:\n  env: " + blob + "\n---\nnamespace prod is healthy\n"
	out := Secrets(in)
	if !strings.Contains(out, "env: [REDACTED]") {
		t.Fatalf("block value must still be masked, got:\n%s", out)
	}
	if !strings.Contains(out, "namespace prod is healthy") {
		t.Fatalf("short decoded value must not be scrubbed globally, got:\n%s", out)
	}
}

// TestSecretsKeepsBenign guards against over-redaction of ordinary investigation
// text (config values, image tags, diff markers, metrics) — false positives would
// blind the model to real evidence.
func TestSecretsKeepsBenign(t *testing.T) {
	benign := []string{
		"replicas: 3",
		"image: registry.k8s.io/pause:3.9",
		"@@ -1,3 +1,4 @@",
		"cpu: 250m\nmemory: 512Mi",
		"reason: CrashLoopBackOff, restartCount: 7",
		"level=info msg=\"reconcile succeeded\" duration=1.2s",
		// 40-char git SHA (looks like an AWS secret but has no AWS cue).
		"merged commit a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 to main",
		// 40-char hex value with no AWS cue, in a benign field.
		"checksum: da39a3ee5e6b4b0d3255bfef95601890afd80709",
		// base64 log blob with no AWS cue.
		"payload ZHVtbXliYXNlNjRibG9iZGF0YXdpdGhvdXRhd3ljdWVoZXJl in trace",
		// "sk-" as a substring inside ordinary words must not trip the token rule.
		"this is task-management, disk-usage and ask-me-anything",
		// "ya29." substring inside a larger word must not match.
		"the library libya29things is fine",
	}
	for _, s := range benign {
		if got := Secrets(s); got != s {
			t.Errorf("benign text was altered:\n  in:  %q\n  out: %q", s, got)
		}
	}
}

// TestAWSSecretRequiresCue pins the high-precision contract for the 40-char AWS
// secret: it is redacted only when an AWS context cue is adjacent. The exact
// same value with no cue (it is shaped like a base64 blob / SHA) must survive,
// so we never false-positive on benign 40-char tokens.
func TestAWSSecretRequiresCue(t *testing.T) {
	const val = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40 chars

	// With an AWS cue adjacent (whitespace-separated), the value is masked.
	withCue := "aws_secret_access_key " + val
	if got := Secrets(withCue); strings.Contains(got, val) {
		t.Fatalf("AWS secret with cue should be redacted, got %q", got)
	}

	// With no AWS cue, the identical 40-char value must be left intact.
	noCue := "blob value " + val + " end"
	if got := Secrets(noCue); got != noCue {
		t.Fatalf("40-char value without AWS cue must survive, got %q", got)
	}
}
