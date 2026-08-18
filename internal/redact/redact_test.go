// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"encoding/base64"
	"fmt"
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
		{"gitlab project access token", "PRIVATE-TOKEN: glpat-aBcDeFgHiJkLmNoPqRsT here", "glpat-aBcDeFgHiJkLmNoPqRsT", "here"},
		// GitLab's routable token format embeds dot-separated segments; the WHOLE
		// token must go, not just the part before the first dot.
		{"gitlab routable token", "PRIVATE-TOKEN: glpat-aBcDeFgHiJkLmNoPqRsT.01.1a2b3c4d5 here", "1a2b3c4d5", "here"},
		{"gitlab runner routable token", "runner registered with glrt-t1_aBcDeFgHiJkLmNoPqRsT.0a.02b3c4d5e now", "02b3c4d5e", "now"},
		// Widening the suffix class with '.' must not swallow a sentence-ending
		// period: the trailing \b forces the engine back off it.
		{"gitlab token ends a sentence", "rotate glpat-aBcDeFgHiJkLmNoPqRsT. Then restart", "glpat-aBcDeFgHiJkLmNoPqRsT", ". Then restart"},
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
		{"elasticsearch apikey auth header", "Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0=", "VnVhQ2ZHY0JDZGJrUW0=", "ApiKey"},
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
		// "apikey" is an ordinary word in prose. A rule matching any 8+ char word
		// after it planted [REDACTED] into findings that had leaked nothing —
		// "apikey rotation policy" -> "apikey [REDACTED] policy".
		"apikey rotation policy is documented in the runbook",
		"see the ApiKey documentation page for the header format",
		"the apikey settings were unchanged",
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

// TestApiKeyRedactionRequiresCredentialShape pins BOTH halves of the ApiKey
// rule split: the Authorization-header form masks any credential shape, the
// bare form only fires on something that is unmistakably a base64 credential,
// and prose is left completely alone. The original single rule
// (`(?i)(apikey\s+)[A-Za-z0-9+/=]{8,}`) redacted the prose cases too, which is
// not a safe failure — it makes a clean finding read as if it leaked a secret.
func TestApiKeyRedactionRequiresCredentialShape(t *testing.T) {
	redacted := []struct{ name, in, gone string }{
		{"authorization header, short credential",
			"Authorization: ApiKey VnVhQ2ZHY0JDZGJrUW0=", "VnVhQ2ZHY0JDZGJrUW0="},
		{"authorization header, lowercase scheme",
			"authorization: apikey VnVhQ2ZHY0JDZGJrUW0=", "VnVhQ2ZHY0JDZGJrUW0="},
		{"quoted YAML header value",
			`headers: {Authorization: "ApiKey VnVhQ2ZHY0JDZGJrUW0="}`, "VnVhQ2ZHY0JDZGJrUW0="},
		{"bare ApiKey with a real ES-sized base64 credential",
			"curl -H 'ApiKey ZFpUZW04SUJKSTZ4cVAwWVBXcXo6ekJTSHhCUlJTWkc0T29UMGJn'",
			"ZFpUZW04SUJKSTZ4cVAwWVBXcXo6ekJTSHhCUlJTWkc0T29UMGJn"},
	}
	for _, tc := range redacted {
		t.Run(tc.name, func(t *testing.T) {
			out := Secrets(tc.in)
			if strings.Contains(out, tc.gone) {
				t.Fatalf("credential survived redaction: %q -> %q", tc.in, out)
			}
			if !strings.Contains(out, "[REDACTED") {
				t.Fatalf("expected a redaction marker, got %q", out)
			}
		})
	}

	// Prose: the two strings the reviewer reproduced, plus neighbours.
	for _, s := range []string{
		"apikey rotation policy",
		"see the ApiKey documentation page",
		"APIKEY provisioning happens at bootstrap",
		"apikey lifecycle",
	} {
		if got := Secrets(s); got != s {
			t.Errorf("prose was over-redacted:\n  in:  %q\n  out: %q", s, got)
		}
	}
}

// TestSensitiveNameValuesMasksTheEnvVarShape closes the hole S1 reproduced: the
// string ruleset is KEY-NAME oriented — it masks `password: hunter2` — while
// Kubernetes' EnvVar inverts the shape. The sensitive word is the VALUE of `name`
// and the credential sits under the literal key `value`, which is in no keyword
// vocabulary, so `Secrets` walks straight past it.
//
// Before this walker, a Pod's `.spec.containers[].env[].value` reached the model
// verbatim.
func TestSensitiveNameValuesMasksTheEnvVarShape(t *testing.T) {
	// The exact shape reproduced against the real redactor, with a value carrying
	// NO recognizable prefix — a `ghp_`-shaped token would be masked by the string
	// rules and prove nothing about this walker.
	obj := map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"name": "app", "env": []any{
			map[string]any{"name": "POSTGRES_PASSWORD", "value": "pr0d-Pa55w0rd-x9"},
			map[string]any{"name": "REDIS_AUTH", "value": "hunter2hunter2"},
			map[string]any{"name": "DB_PASSWORD", "value": "hunter2supersecret"},
			// A reference is not a credential: valueFrom names a Secret key, it
			// does not carry it, and masking it would blind the model to WHICH
			// secret a workload consumes.
			map[string]any{"name": "API_TOKEN", "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": "api", "key": "token"}}},
			// Benign env survives: over-redaction destroys otherwise-correct
			// evidence, and the model needs to see ordinary configuration.
			map[string]any{"name": "LOG_LEVEL", "value": "debug"},
			map[string]any{"name": "GIT_AUTHOR_NAME", "value": "runlore"},
			map[string]any{"name": "TOKENIZER_MODE", "value": "fast"},
		}},
	}}}
	SensitiveNameValues(obj)

	env := obj["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	for i, want := range []string{mask, mask, mask, "", "debug", "runlore", "fast"} {
		e := env[i].(map[string]any)
		got, _ := e["value"].(string)
		if got != want {
			t.Errorf("env[%d] (%v): value = %q, want %q", i, e["name"], got, want)
		}
	}
	// The reference itself is untouched, keys and all.
	if ref := env[3].(map[string]any)["valueFrom"]; ref == nil {
		t.Error("valueFrom reference was dropped; the model can no longer see which Secret is consumed")
	}
	// Idempotent, like every other pass here.
	SensitiveNameValues(obj)
	if got := env[0].(map[string]any)["value"]; got != mask {
		t.Errorf("second pass changed a masked value: %q", got)
	}
}

// TestSensitiveNameValuesReachesEveryEmbeddedPodSpec: the shape is not only a bare
// Pod. A Deployment nests it under spec.template.spec, a CronJob two levels deeper,
// and any operator CRD may embed a PodSpec wholesale — so the walk is over the
// whole tree rather than a fixed list of paths.
func TestSensitiveNameValuesReachesEveryEmbeddedPodSpec(t *testing.T) {
	env := func() []any {
		return []any{map[string]any{"name": "APP_SECRET", "value": "leakme-please-1234"}}
	}
	obj := map[string]any{"spec": map[string]any{
		"template": map[string]any{"spec": map[string]any{
			"initContainers":      []any{map[string]any{"name": "init", "env": env()}},
			"containers":          []any{map[string]any{"name": "app", "env": env()}},
			"ephemeralContainers": []any{map[string]any{"name": "dbg", "env": env()}},
		}},
	}}
	SensitiveNameValues(obj)
	if s := fmt.Sprint(obj); strings.Contains(s, "leakme-please-1234") {
		t.Fatalf("a nested container env value survived:\n%s", s)
	}
}
