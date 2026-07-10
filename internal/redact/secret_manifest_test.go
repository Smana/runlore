// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"strings"
	"testing"
)

// TestSecretsMasksK8sSecretData covers the REDACT-B64 gap: base64-encoded values
// inside a Kubernetes `kind: Secret` manifest (the `data:` / `stringData:` block)
// are masked, keys and surrounding structure preserved. ConfigMaps and other
// kinds are left untouched to avoid over-masking.
func TestSecretsMasksK8sSecretData(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		gone  []string // substrings that must NOT survive
		keeps []string // structure that SHOULD survive
	}{
		{
			name: "secret data two base64 values",
			in: "apiVersion: v1\n" +
				"kind: Secret\n" +
				"metadata:\n" +
				"  name: db-creds\n" +
				"  namespace: prod\n" +
				"type: Opaque\n" +
				"data:\n" +
				"  username: YWRtaW4=\n" +
				"  password: c3VwM3JzM2NyZXQ=\n",
			gone:  []string{"YWRtaW4=", "c3VwM3JzM2NyZXQ="},
			keeps: []string{"kind: Secret", "name: db-creds", "namespace: prod", "username:", "password:"},
		},
		{
			name: "secret data inside git diff plus prefix",
			in: "diff --git a/secret.yaml b/secret.yaml\n" +
				"@@ -1,8 +1,9 @@\n" +
				"+apiVersion: v1\n" +
				"+kind: Secret\n" +
				"+metadata:\n" +
				"+  name: db-creds\n" +
				"+type: Opaque\n" +
				"+data:\n" +
				"+  username: YWRtaW4=\n" +
				"+  password: c3VwM3JzM2NyZXQ=\n",
			gone:  []string{"YWRtaW4=", "c3VwM3JzM2NyZXQ="},
			keeps: []string{"kind: Secret", "username:", "password:"},
		},
		{
			name: "stringData values masked",
			in: "kind: Secret\n" +
				"metadata:\n" +
				"  name: api\n" +
				"stringData:\n" +
				"  token: my-plaintext-token\n" +
				"  apiKey: another-plaintext-value\n",
			gone:  []string{"my-plaintext-token", "another-plaintext-value"},
			keeps: []string{"kind: Secret", "token:", "apiKey:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Secrets(tc.in)
			for _, g := range tc.gone {
				if strings.Contains(out, g) {
					t.Fatalf("secret value survived redaction: %q\n -> %q", g, out)
				}
			}
			if !strings.Contains(out, mask) {
				t.Fatalf("expected a redaction marker, got %q", out)
			}
			for _, k := range tc.keeps {
				if !strings.Contains(out, k) {
					t.Fatalf("structure %q should survive, got %q", k, out)
				}
			}
			// Idempotent: redacting again changes nothing.
			if again := Secrets(out); again != out {
				t.Fatalf("not idempotent: %q -> %q", out, again)
			}
		})
	}
}

// TestSecretsDoesNotMaskConfigMap guards against over-masking: a non-Secret
// document with a `data:` block (here a ConfigMap) must be left intact.
func TestSecretsDoesNotMaskConfigMap(t *testing.T) {
	in := "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n" +
		"  name: app-config\n" +
		"data:\n" +
		"  log_level: info\n" +
		"  replicas: \"3\"\n"
	if got := Secrets(in); got != in {
		t.Fatalf("ConfigMap data must not be masked:\n  in:  %q\n  out: %q", in, got)
	}
}

// TestSecretsConfigMapThenSecret pins the document-boundary behaviour: a `---`
// separator ends the ConfigMap document, and the following Secret document's
// data block IS masked while the ConfigMap's data block is NOT.
func TestSecretsConfigMapThenSecret(t *testing.T) {
	in := "kind: ConfigMap\n" +
		"data:\n" +
		"  log_level: info\n" +
		"---\n" +
		"kind: Secret\n" +
		"data:\n" +
		"  password: c3VwM3JzM2NyZXQ=\n"
	out := Secrets(in)
	if strings.Contains(out, "c3VwM3JzM2NyZXQ=") {
		t.Fatalf("Secret value should be masked, got %q", out)
	}
	if !strings.Contains(out, "log_level: info") {
		t.Fatalf("ConfigMap value should survive, got %q", out)
	}
}

// TestSecretsSecretBlockEndsAtTopLevelKey ensures masking stops when the
// data/stringData block ends at a sibling top-level key, so values outside the
// block are not masked.
func TestSecretsSecretBlockEndsAtTopLevelKey(t *testing.T) {
	in := "kind: Secret\n" +
		"data:\n" +
		"  password: c3VwM3JzM2NyZXQ=\n" +
		"type: Opaque\n"
	out := Secrets(in)
	if strings.Contains(out, "c3VwM3JzM2NyZXQ=") {
		t.Fatalf("Secret data value should be masked, got %q", out)
	}
	if !strings.Contains(out, "type: Opaque") {
		t.Fatalf("sibling top-level key should survive intact, got %q", out)
	}
}
