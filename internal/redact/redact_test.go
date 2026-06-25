package redact

import (
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
		{"openai key", "OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwx", "sk-abcdefghijklmnopqrstuvwx", ""},
		{"slack token", "token xoxb-123456789012-abcdefuvwxyz", "xoxb-123456789012-abcdefuvwxyz", ""},
		{"aws key id", "AccessKeyId: " + secret, secret, "AccessKeyId"},
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
	}
	for _, s := range benign {
		if got := Secrets(s); got != s {
			t.Errorf("benign text was altered:\n  in:  %q\n  out: %q", s, got)
		}
	}
}
