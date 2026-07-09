// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTokenExchange(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"token":"inst-tok","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	ts := NewAppTokenSource(srv.URL, 123, 42, key)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "inst-tok" {
		t.Fatalf("token = %q", tok)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("auth = %q (want a Bearer JWT)", gotAuth)
	}
	if gotPath != "/app/installations/42/access_tokens" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestMintJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(12345, key, time.Unix(1_000_000, 0))
	if err != nil {
		t.Fatalf("mintJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 JWT segments, got %d", len(parts))
	}
	// signature verifies against the public key
	signing := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signing))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	// claims carry the app id as issuer
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(claimsJSON, &claims)
	if fmt.Sprint(claims["iss"]) != "12345" {
		t.Fatalf("iss=%v", claims["iss"])
	}
}
