// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tlsServer is a TLS server with a certificate of its own (httptest's built-in one is shared by every
// server), valid for 127.0.0.1 and self-signed, so it is its own CA.
func tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// certPEM is the certificate a tlsServer presents, as a PEM file.
func certPEM(t *testing.T, s *httptest.Server) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(s.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func get(c *http.Client, url string) error {
	resp, err := c.Get(url)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestSecureClientWithRoots(t *testing.T) {
	private, public := tlsServer(t), tlsServer(t)

	t.Run("trusted with the CA", func(t *testing.T) {
		roots, err := RootsWithCAFile(x509.NewCertPool(), certPEM(t, private))
		if err != nil {
			t.Fatal(err)
		}
		if err := get(SecureClientWithRoots(5*time.Second, roots), private.URL); err != nil {
			t.Fatalf("a server signed by the file's CA is trusted: %v", err)
		}
	})
	t.Run("refused without it", func(t *testing.T) {
		err := get(SecureClientWithRoots(5*time.Second, x509.NewCertPool()), private.URL)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("an unknown CA is refused: %v", err)
		}
		if err := get(SecureClient(5*time.Second), private.URL); err == nil {
			t.Fatal("the plain client still refuses a private CA")
		}
	})
	t.Run("the base pool stays trusted", func(t *testing.T) {
		// base stands in for the system pool, which holds the public roots.
		base := x509.NewCertPool()
		base.AppendCertsFromPEM(mustPEM(t, certPEM(t, public)))
		roots, err := RootsWithCAFile(base, certPEM(t, private))
		if err != nil {
			t.Fatal(err)
		}
		c := SecureClientWithRoots(5*time.Second, roots)
		if err := get(c, public.URL); err != nil {
			t.Fatalf("a root of the base pool still verifies: %v", err)
		}
		if err := get(c, private.URL); err != nil {
			t.Fatalf("and the file's CA too: %v", err)
		}
		if base.Equal(roots) {
			t.Fatal("the base pool is copied, not modified")
		}
	})
	t.Run("keeps the redirect guard and the timeout", func(t *testing.T) {
		c := SecureClientWithRoots(7*time.Second, x509.NewCertPool())
		if c.Timeout != 7*time.Second || c.CheckRedirect == nil {
			t.Fatalf("timeout %v, redirect guard %v", c.Timeout, c.CheckRedirect != nil)
		}
	})
}

func TestRootsWithCAFileRefuses(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "nope.pem"), "no certificate": junk} {
		if _, err := RootsWithCAFile(nil, path); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func mustPEM(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
