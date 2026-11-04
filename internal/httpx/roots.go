// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// RootsWithCAFile returns a copy of base with the PEM certificates of the file at path added: the
// system roots stay trusted, so a private CA widens trust for one client and never narrows it. A nil
// base starts from the system pool. A file that cannot be read, or holds no certificate, is an error.
func RootsWithCAFile(base *x509.CertPool, path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // an operator-configured path
	if err != nil {
		return nil, err
	}
	if base == nil {
		if base, err = x509.SystemCertPool(); err != nil || base == nil {
			base = x509.NewCertPool()
		}
	}
	roots := base.Clone()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificate found in %s", path)
	}
	return roots, nil
}

// SecureClientWithRoots is SecureClient that verifies server certificates against roots instead of
// the system pool alone. The timeout and the redirect policy are SecureClient's; so is the proxy
// from the environment, since the transport is a clone of the default one.
func SecureClientWithRoots(timeout time.Duration, roots *x509.CertPool) *http.Client {
	c := SecureClient(timeout)
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t = t.Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	c.Transport = t
	return c
}
