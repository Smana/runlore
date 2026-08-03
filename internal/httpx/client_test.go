// SPDX-License-Identifier: Apache-2.0

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

// TestDenyInternalRedirectBlocksLinkLocalFromInternalOrigin pins the one target the
// internal-origin exemption must NOT cover. Most RunLore egress (metrics, logs, MCP,
// an in-cluster model) starts at a ClusterIP, so that exemption applies to the
// majority of outbound traffic — and it is right for private↔private, where a
// backend legitimately redirects to itself. Link-local is different in kind: nothing
// in a cluster 3xx-redirects to 169.254.0.0/16, and the one thing living there is the
// cloud metadata service. A compromised in-cluster backend (or a hostile MCP server
// on a private address) answering 302 → 169.254.169.254 would otherwise have its
// IMDSv1 credentials read and handed to the model, which publishes them into a KB PR,
// a Slack card or a webhook — a complete exfiltration path.
func TestDenyInternalRedirectBlocksLinkLocalFromInternalOrigin(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(host string) ([]net.IP, error) {
		if host == "loki.monitoring.svc.cluster.local" {
			return []net.IP{net.ParseIP("10.96.0.10")}, nil // in-cluster origin
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	origin := mkreq(t, "http://loki.monitoring.svc.cluster.local/loki/api/v1/query")
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://metadata.example/latest/meta-data/", // via DNS, same destination
	} {
		if err := DenyInternalRedirect(mkreq(t, target), []*http.Request{origin}); err == nil {
			t.Fatalf("a redirect to link-local %s must be denied even from an in-cluster origin", target)
		}
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

// mkreqWithKeys builds a redirect-target request carrying every provider credential
// header. PRIVATE-TOKEN is set with GitLab's own (non-canonical) casing on purpose:
// Header.Set canonicalizes to "Private-Token", which is what sensitiveAuthHeaders
// must list for Header.Del to match — pinning that here keeps the two in step.
func mkreqWithKeys(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	r := mkreq(t, rawurl)
	r.Header.Set("X-Api-Key", "sk-secret")
	r.Header.Set("X-Goog-Api-Key", "goog-secret")
	r.Header.Set("Authorization", "Bearer tok")
	r.Header.Set("PRIVATE-TOKEN", "glpat-aBcDeFgHiJkLmNoPqRsT")
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
	for _, h := range []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization", "PRIVATE-TOKEN"} {
		if got := target.Header.Get(h); got != "" {
			t.Fatalf("header %s must be stripped on cross-host redirect, got %q", h, got)
		}
	}
}

// TestDenyInternalRedirectStripsGitLabTokenPublicToPublic is the GitLab-specific
// half of the header-stripping contract, kept as its own test because the two
// guards that LOOK like they already cover it do not: Go's net/http strips only
// Authorization/Cookie (PRIVATE-TOKEN is a custom header, so it is replayed), and
// DenyInternalRedirect's internal-target check never fires on a public→public
// redirect. The credential in question carries GitLab's full `api` scope, so
// replaying it to an attacker-controlled host is a total KB-project compromise.
func TestDenyInternalRedirectStripsGitLabTokenPublicToPublic(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil } // public

	origin := mkreq(t, "https://gitlab.com/api/v4/projects/g%2Fp/merge_requests")
	target := mkreq(t, "https://attacker.example/collect")
	target.Header.Set("PRIVATE-TOKEN", "glpat-aBcDeFgHiJkLmNoPqRsT")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("public cross-host redirect should be allowed (header stripped), got %v", err)
	}
	if got := target.Header.Get("PRIVATE-TOKEN"); got != "" {
		t.Fatalf("PRIVATE-TOKEN must not survive a cross-host redirect, got %q", got)
	}
}

// TestDenyInternalRedirectKeepsGitLabTokenOnSameHost is the other half: a
// self-managed instance doing an http→https upgrade or a trailing-slash redirect
// on ITSELF must stay authenticated, or every forge call 401s.
func TestDenyInternalRedirectKeepsGitLabTokenOnSameHost(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil } // public

	origin := mkreq(t, "http://gitlab.example.com/api/v4/projects/g%2Fp")
	target := mkreq(t, "https://gitlab.example.com/api/v4/projects/g%2Fp")
	target.Header.Set("PRIVATE-TOKEN", "glpat-aBcDeFgHiJkLmNoPqRsT")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("same-host redirect should be allowed, got %v", err)
	}
	if target.Header.Get("PRIVATE-TOKEN") == "" {
		t.Fatal("PRIVATE-TOKEN must be retained on a same-host redirect")
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
	if target.Header.Get("X-Api-Key") == "" || target.Header.Get("X-Goog-Api-Key") == "" ||
		target.Header.Get("Authorization") == "" || target.Header.Get("PRIVATE-TOKEN") == "" {
		t.Fatal("key headers must be retained on a same-host redirect")
	}
}
