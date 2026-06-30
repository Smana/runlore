package httpx

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func mkreq(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("new request %q: %v", rawurl, err)
	}
	return r
}

func TestDenyInternalRedirect(t *testing.T) {
	cases := []struct {
		name string
		url  string
		deny bool
	}{
		{"cloud metadata (link-local)", "http://169.254.169.254/latest/meta-data/", true},
		{"loopback", "http://127.0.0.1:8080/x", true},
		{"private 10/8", "http://10.0.0.5/x", true},
		{"private 192.168", "http://192.168.1.1/x", true},
		{"unspecified", "http://0.0.0.0/x", true},
		{"public IP", "http://8.8.8.8/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DenyInternalRedirect(mkreq(t, tc.url), nil)
			if tc.deny && err == nil {
				t.Fatalf("expected redirect to %s to be denied", tc.url)
			}
			if !tc.deny && err != nil {
				t.Fatalf("expected redirect to %s to be allowed, got %v", tc.url, err)
			}
		})
	}
}

func TestDenyInternalRedirectHostname(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()

	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil }
	if err := DenyInternalRedirect(mkreq(t, "http://evil.example.com/"), nil); err == nil {
		t.Fatal("expected deny when a hostname resolves to a private IP (DNS-based SSRF)")
	}

	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	if err := DenyInternalRedirect(mkreq(t, "http://ok.example.com/"), nil); err != nil {
		t.Fatalf("expected allow when a hostname resolves to a public IP, got %v", err)
	}
}

func TestDenyInternalRedirectFailsClosedOnResolveError(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return nil, net.UnknownNetworkError("nope") }
	if err := DenyInternalRedirect(mkreq(t, "http://wherever.example.com/"), nil); err == nil {
		t.Fatal("expected deny (fail closed) when the redirect host cannot be resolved")
	}
}

func TestDenyInternalRedirectCap(t *testing.T) {
	via := make([]*http.Request, maxRedirects)
	if err := DenyInternalRedirect(mkreq(t, "http://8.8.8.8/"), via); err == nil {
		t.Fatalf("expected error once the redirect cap (%d) is reached", maxRedirects)
	}
}

func TestSecureClient(t *testing.T) {
	c := SecureClient(5 * time.Second)
	if c.Timeout != 5*time.Second {
		t.Fatalf("timeout not set: %v", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("SecureClient must install a CheckRedirect policy")
	}
}

func TestDenyInternalRedirectAllowsInternalOrigin(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	// Both the in-cluster origin and the redirect target resolve to a ClusterIP — e.g.
	// an in-cluster backend doing http→https or a trailing-slash redirect on itself.
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.96.0.10")}, nil }
	origin := mkreq(t, "http://vllm.ai.svc.cluster.local/v1/chat")
	target := mkreq(t, "https://vllm.ai.svc.cluster.local/v1/chat")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("an internal-origin internal redirect must be allowed, got %v", err)
	}
}

func TestDenyInternalRedirectBlocksExternalToInternal(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(host string) ([]net.IP, error) {
		if host == "api.example.com" {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil // public origin
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil // anything else → metadata
	}
	origin := mkreq(t, "https://api.example.com/x")
	target := mkreq(t, "http://metadata.example/latest")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err == nil {
		t.Fatal("a public→internal (metadata) redirect must be blocked")
	}
}

// mkreqWithKeys builds a redirect-target request carrying the three provider key headers.
func mkreqWithKeys(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	r := mkreq(t, rawurl)
	r.Header.Set("X-Api-Key", "sk-secret")
	r.Header.Set("X-Goog-Api-Key", "goog-secret")
	r.Header.Set("Authorization", "Bearer tok")
	return r
}

func TestDenyInternalRedirectStripsKeyOnCrossHost(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil } // public

	origin := mkreq(t, "https://api.anthropic.com/v1/messages")
	target := mkreqWithKeys(t, "https://attacker.example/v1/messages")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("public cross-host redirect should be allowed (headers stripped), got %v", err)
	}
	for _, h := range []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization"} {
		if got := target.Header.Get(h); got != "" {
			t.Fatalf("header %s must be stripped on cross-host redirect, got %q", h, got)
		}
	}
}

func TestDenyInternalRedirectKeepsKeyOnSameHost(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil } // public

	// Same hostname, http→https upgrade (port 80→443): the key must be retained.
	origin := mkreq(t, "http://api.anthropic.com/v1/messages")
	target := mkreqWithKeys(t, "https://api.anthropic.com/v1/messages")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("same-host redirect should be allowed, got %v", err)
	}
	if target.Header.Get("X-Api-Key") == "" || target.Header.Get("X-Goog-Api-Key") == "" || target.Header.Get("Authorization") == "" {
		t.Fatal("key headers must be retained on a same-host redirect")
	}
}
