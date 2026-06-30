package httpx

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxRedirects caps a redirect chain (Go's implicit default is also 10; we make it
// explicit because installing a CheckRedirect replaces that default).
const maxRedirects = 10

// sensitiveAuthHeaders are request headers that carry a provider credential. Go's
// net/http strips Authorization/Cookie itself on a host-changing redirect but NOT
// custom headers, so DenyInternalRedirect deletes these explicitly (canonical form;
// http.Header.Del is case-insensitive). x-api-key = Anthropic, x-goog-api-key = Gemini.
var sensitiveAuthHeaders = []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization"}

// SecureClient returns an http.Client with the given timeout and the
// DenyInternalRedirect policy. Use it for every outbound call to an operator- or
// externally-configurable endpoint (model, forge, notifier, metrics/logs) so a
// redirect can't be steered at an internal address.
func SecureClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: DenyInternalRedirect}
}

// DenyInternalRedirect is an http.Client CheckRedirect that caps the redirect chain
// and refuses a redirect to a private/loopback/link-local target — closing the
// redirect → 169.254.169.254 (cloud metadata) SSRF exfil path. It fails CLOSED on an
// unresolvable target; public redirects are still followed.
//
// It guards only chains that ORIGINATED at a public address — the actual SSRF threat
// (a public endpoint steered at an internal target). A chain that started internal (an
// in-cluster model/metrics/logs backend behind a proxy, or a plain http→https / trailing
// -slash redirect on a private host) is legitimate and is allowed, so the guard never
// breaks in-cluster traffic. Dial-time DNS-rebinding protection is out of scope.
func DenyInternalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	// Strip provider key headers when a redirect changes host, so a credential is never
	// replayed to a different host (a compromised/MITM upstream, or an http endpoint
	// 3xx-ing elsewhere). Hostname-only compare (ignore port) keeps a same-host
	// http→https upgrade authenticated. Guard the nil entries that the cap test passes.
	if n := len(via); n > 0 && via[n-1] != nil {
		if !strings.EqualFold(req.URL.Hostname(), via[n-1].URL.Hostname()) {
			for _, h := range sensitiveAuthHeaders {
				req.Header.Del(h)
			}
		}
	}
	// In-cluster-origin chains redirect among private addresses legitimately — only
	// guard chains that began at a public endpoint.
	if len(via) > 0 && hostIsInternal(via[0].URL.Hostname()) {
		return nil
	}
	host := req.URL.Hostname()
	ips, err := lookupIP(host)
	if err != nil {
		return fmt.Errorf("redirect host %q: %w", host, err)
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return fmt.Errorf("refusing redirect to internal address %s (host %q)", ip, host)
		}
	}
	return nil
}

// hostIsInternal reports whether host resolves entirely to internal addresses. A
// resolution failure or empty result returns false (treat as possibly external, so
// the redirect target is still guarded).
func hostIsInternal(host string) bool {
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !isInternalIP(ip) {
			return false
		}
	}
	return true
}

// lookupIP resolves a redirect host to IPs. A package var so tests can stub DNS
// without touching the network; an IP literal short-circuits resolution.
var lookupIP = func(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.LookupIP(host)
}

func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
