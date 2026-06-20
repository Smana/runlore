package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AppTokenSource mints and caches GitHub App installation tokens.
type AppTokenSource struct {
	appID     int64
	installID int64
	key       *rsa.PrivateKey
	baseURL   string
	http      *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

// NewAppTokenSource builds a token source from an App ID, installation ID, and
// RSA private key. baseURL may be empty (defaults to DefaultBaseURL).
func NewAppTokenSource(baseURL string, appID, installID int64, key *rsa.PrivateKey) *AppTokenSource {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &AppTokenSource{appID: appID, installID: installID, key: key,
		baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

// Token returns a valid installation token, refreshing when near expiry.
func (s *AppTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.exp.Add(-1*time.Minute)) {
		return s.token, nil
	}
	jwt, err := mintJWT(s.appID, s.key, time.Now())
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens", s.baseURL, s.installID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("installation token: status %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	s.token, s.exp = out.Token, out.ExpiresAt
	return s.token, nil
}

// mintJWT builds a short-lived RS256 JWT signed with the App private key.
func mintJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	header, err := enc(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := enc(map[string]any{
		"iat": now.Add(-30 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + claims
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ParsePrivateKey parses a PEM-encoded RSA private key (PKCS#1 or PKCS#8).
func ParsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}
	return rk, nil
}
