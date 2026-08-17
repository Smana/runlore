// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/sourcerepo"
)

// TestRemoteClonesSSHRepoURLOverHTTPS is the RunLore #495 regression.
//
// Argo CD / Flux routinely point at their source repo over SSH because the
// engine authenticates with a deploy key. RunLore has no SSH credential — auth
// only ever mints an HTTP basic auth from the GitHub App token — so go-git's SSH
// transport rejected every such clone with "invalid auth method" and what_changed
// lost change correlation for EVERY object, visible only as a data-gaps line.
//
// The clone is driven with an already-cancelled context so it never touches the
// network: go-git formats the target URL into the error before doing any I/O,
// which is exactly the assertion we need — WHICH url was dialled.
func TestRemoteClonesSSHRepoURLOverHTTPS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &Differ{
		TokenSource:    func(context.Context) (string, error) { return "ghs_tok", nil },
		SSHRewriteHost: "github.com",
	}
	_, err := d.Remote(ctx, "git@github.com:Aqemia/engineering.git", "a", "b", "")
	if err == nil {
		t.Fatal("want an error from a cancelled clone, got nil")
	}
	if strings.Contains(err.Error(), "invalid auth method") {
		t.Fatalf("SSH repoURL still dialled over SSH with an HTTPS-only credential (#495): %v", err)
	}
	if !strings.Contains(err.Error(), "https://github.com/Aqemia/engineering.git") {
		t.Fatalf("want the clone to target the HTTPS equivalent, got %v", err)
	}
}

func TestSSHToHTTPS(t *testing.T) {
	tests := []struct {
		name, in, want string
		ok             bool
	}{
		{"scp-style github", "git@github.com:Aqemia/engineering.git", "https://github.com/Aqemia/engineering.git", true},
		{"scp-style gitlab subgroup", "git@gitlab.com:group/sub/proj.git", "https://gitlab.com/group/sub/proj.git", true},
		{"scp-style self-hosted gitlab", "git@gitlab.example.com:org/repo.git", "https://gitlab.example.com/org/repo.git", true},
		{"ssh scheme", "ssh://git@github.com/acme/x.git", "https://github.com/acme/x.git", true},
		{"ssh scheme drops the ssh port", "ssh://git@gitlab.example.com:2222/org/repo.git", "https://gitlab.example.com/org/repo.git", true},
		{"ssh scheme without userinfo", "ssh://github.com/acme/x", "https://github.com/acme/x", true},
		{"surrounding whitespace", "  git@github.com:acme/x.git\n", "https://github.com/acme/x.git", true},
		// The RFC 3986 authority of this URL is evil.example — go-git would dial
		// exactly that over SSH, and the rewrite must not "helpfully" prefer the
		// userinfo's lookalike host. Same host in, same host out.
		{"userinfo lookalike keeps the real host", "ssh://git@github.com@evil.example/acme/x", "https://evil.example/acme/x", true},
		// '@' after the first '/' is path, not authority: the host stays github.com.
		{"at-sign in the path is not authority", "git@github.com:x@evil.example/y.git", "https://github.com/x@evil.example/y.git", true},
		{"https is left alone", "https://github.com/acme/x.git", "", false},
		{"http is left alone", "http://github.com/acme/x", "", false},
		{"git protocol is left alone", "git://github.com/acme/x.git", "", false},
		{"local path is left alone", "/srv/fixtures/repo", "", false},
		{"empty is left alone", "", "", false},
		{"traversal segment is refused", "git@github.com:../../etc/passwd", "", false},
		{"traversal segment under ssh scheme is refused", "ssh://git@github.com/acme/../../x", "", false},
		// The round-trip guard: an SSH path is opaque bytes, an HTTPS path is
		// not, so anything whose meaning would shift in the rewrite is refused
		// rather than guessed at.
		{"percent-encoded traversal is refused", "git@github.com:acme/%2e%2e/evil/x.git", "", false},
		{"query-shaped path is refused", "git@github.com:acme/x?a=b", "", false},
		{"fragment-shaped path is refused", "git@github.com:acme/x#ssh://git@evil.example/a", "", false},
		{"control character in the host is refused", "git@github.com\x00evil.example:acme/x.git", "", false},
		// The credential-exfiltration shape. go-git's scp syntax takes the FIRST
		// '@' as userinfo, so it reads the host as the whole "github.com@evil.example"
		// and fails; net/url takes the LAST, so an unguarded rewrite would emit a
		// perfectly valid clone of evil.example with "github.com" demoted to
		// userinfo — and what_changed, which attaches its App token to every host,
		// would post the token there. Refused.
		{"double-@ host is refused, never demoted to userinfo", "git@github.com@evil.example:acme/x.git", "", false},
		{"double-@ host is refused (short form)", "git@a@b.example:acme/x.git", "", false},
		{"hostless scp form is refused", "git@:acme/x.git", "", false},
		// go-git's scp regex splits this at the FIRST colon, yielding host
		// "[fd00" — a rewrite would name a host nobody asked for, so it is not
		// attempted. (The ssh:// spelling below parses correctly and is fine.)
		{"scp-style bracketed IPv6 is refused, not guessed", "git@[fd00::1]:acme/x.git", "", false},
		{"ssh-scheme IPv6 host survives", "ssh://git@[fd00::1]/acme/x.git", "https://[fd00::1]/acme/x.git", true},
		// go-git resolves the host to "user" here; the rewrite must report that
		// same host and never promote the later, more plausible-looking one.
		{"no host promotion from a later lookalike", "git@user:pass@github.com:acme/x.git", "https://user/pass@github.com:acme/x.git", true},
		// Non-ASCII hosts. hostOf lowercases with strings.ToLower — Unicode SIMPLE
		// case mapping — while net/http resolves through idna.Lookup.ToASCII, and
		// the two disagree: ToLower maps U+0130 to 'i', so "gİthub.com" reads as
		// "github.com" to the authorization check while net/http dials the
		// separately registrable "xn--github-qyd.com". Refuse the class, not the
		// one rune.
		{"homoglyph host U+0130 is refused", "git@gİthub.com:org/repo.git", "", false},
		{"homoglyph host under ssh scheme is refused", "ssh://git@gİthub.com/org/repo.git", "", false},
		{"kelvin-sign host is refused", "git@bacKend.example:org/repo.git", "", false},
		{"fullwidth host is refused", "git@ｇithub.com:org/repo.git", "", false},
		{"zero-width space in the host is refused", "git@git\u200bhub.com:org/repo.git", "", false},
		{"soft hyphen in the host is refused", "git@git\u00adhub.com:org/repo.git", "", false},
		// A lone continuation byte is not valid UTF-8, so no rune-based check
		// sees it and the round-trip guard passes it cleanly — only the byte
		// scan rejects it. This pins the boundary at exactly 0x80: every valid
		// non-ASCII rune has a lead byte >= 0xC2, so nothing else in this table
		// would notice the boundary drifting upward.
		{"raw 0x80 byte in the host is refused", "git@a\x80b.example:org/repo.git", "", false},
		// Punycode written out is plain ASCII and stays its own distinct host —
		// it must not be decoded back into a lookalike of anything.
		{"punycode spelling is its own host", "git@xn--github-qyd.com:org/repo.git", "https://xn--github-qyd.com/org/repo.git", true},
		// A non-ASCII PATH is not a host-confusion risk: it cannot change which
		// host is dialled, and net/http escapes it on the wire.
		{"non-ascii path is allowed", "git@github.com:org/répo.git", "https://github.com/org/répo.git", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sshToHTTPS(tc.in)
			if ok != tc.ok {
				t.Fatalf("sshToHTTPS(%q) ok = %v, want %v (got %q)", tc.in, ok, tc.ok, got)
			}
			if got != tc.want {
				t.Fatalf("sshToHTTPS(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEffectiveCloneURLGating pins WHEN the SSH→HTTPS rewrite applies: only when
// there is a credential AND a host it is known to be valid for. Rewriting with no
// token would trade go-git's working SSH-agent path for an unauthenticated HTTPS
// 401; rewriting with no known host is the credential leak guarded below.
func TestEffectiveCloneURLGating(t *testing.T) {
	const ssh = "git@github.com:acme/x.git"
	const https = "https://github.com/acme/x.git"
	tok := func(context.Context) (string, error) { return "ghs_tok", nil }
	armed := func(host string) *Differ { return &Differ{TokenSource: tok, SSHRewriteHost: host} }

	tests := []struct {
		name string
		d    *Differ
		in   string
		want string
	}{
		{"no token source keeps SSH for the ssh-agent path", &Differ{SSHRewriteHost: "github.com"}, ssh, ssh},
		{"credential host matches: rewrites to HTTPS", armed("github.com"), ssh, https},
		{"unset credential host refuses to rewrite", &Differ{TokenSource: tok}, ssh, ssh},
		{"foreign credential host keeps SSH", armed("ghe.example.com"), ssh, ssh},
		{"https is never touched", armed("github.com"), https, https},
		{"local path is never touched", armed("github.com"), "/srv/fixtures/repo", "/srv/fixtures/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.effectiveCloneURL(tc.in); got != tc.want {
				t.Fatalf("effectiveCloneURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSSHRewriteNeverReachesAForeignHost is the credential boundary the #495
// rewrite must not dissolve.
//
// A GitOps repoURL is cluster state: in a shared cluster, anyone who can create
// an Application chooses it. The what_changed differ runs with TokenHost EMPTY,
// so auth attaches the GitHub App installation token to whatever host it clones.
// Before the rewrite existed, "invalid auth method" killed every SSH URL before a
// byte left the process — the bug was inadvertently acting as a credential
// boundary. An unconfined rewrite would have turned
// "repoURL: ssh://git@attacker.example/x" into that token being POSTed to
// attacker.example.
//
// The clone runs on an already-cancelled context, which never touches the
// network but does reveal WHICH transport was chosen: go-git names an HTTPS
// target in the error ("Get \"https://…\"") and rejects an SSH one at
// "invalid auth method" without forming a request at all. So the error text is
// the proof that nothing was ever addressed to the foreign host.
func TestSSHRewriteNeverReachesAForeignHost(t *testing.T) {
	// Wired exactly as buildGitOpsDiffer wires what_changed: a credential for
	// github.com, and TokenHost left empty.
	newDiffer := func() *Differ {
		return &Differ{
			TokenSource:    func(context.Context) (string, error) { return "ghs_SECRET", nil },
			SSHRewriteHost: "github.com",
		}
	}

	foreign := []string{
		"ssh://git@attacker.example/org/repo.git",
		"git@attacker.example:org/repo.git",
		"ssh://git@gitlab.com/org/repo.git",
		"git@ghe.example.com:org/repo.git",
		// Lookalikes: neither is the credential's host.
		"git@github.com.attacker.example:org/repo.git",
		"git@notgithub.com:org/repo.git",
		// IDNA homoglyphs. These are the dangerous ones: hostOf lowercases with
		// strings.ToLower, which maps U+0130 to 'i', so each of these reads as
		// exactly "github.com" to the authorization check — while net/http
		// resolves through idna.Lookup.ToASCII and dials a punycode host the
		// attacker can register ("xn--github-qyd.com"), serve a valid cert for,
		// and read the App token off the Authorization header.
		"git@gİthub.com:org/repo.git",
		"ssh://git@gİthub.com/org/repo.git",
		"git@githუb.com:org/repo.git",
		"git@ｇithub.com:org/repo.git",
		"git@git\u200bhub.com:org/repo.git",
	}
	for _, raw := range foreign {
		t.Run("foreign "+raw, func(t *testing.T) {
			d := newDiffer()
			if got := d.effectiveCloneURL(raw); got != raw {
				t.Fatalf("rewrote an SSH URL toward a host the credential is not for: %q → %q", raw, got)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := d.Remote(ctx, raw, "a", "b", "")
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if strings.Contains(err.Error(), "https://") {
				t.Fatalf("an HTTPS request was addressed to a foreign host — the App token would be sent there: %v", err)
			}
			if !strings.Contains(err.Error(), "invalid auth method") {
				t.Fatalf("want the SSH transport to refuse before any request is formed, got %v", err)
			}
		})
	}

	// The credential's own host still gets the fix — the confinement must not
	// have simply disabled #495.
	t.Run("credential host is still rewritten", func(t *testing.T) {
		d := newDiffer()
		const raw = "git@github.com:org/repo.git"
		if got := d.effectiveCloneURL(raw); got != "https://github.com/org/repo.git" {
			t.Fatalf("effectiveCloneURL(%q) = %q, want the HTTPS equivalent", raw, got)
		}
	})
}

// TestCloneRewriteCannotBypassSourceRepoAllowlist is the security guard for #495.
//
// source_diff's allowlist is the boundary that stops a prompt-injected model from
// making RunLore clone arbitrary hosts (internal/sourcerepo). The tool checks the
// model's raw string and hands cloneToDisk the URL that check itself produced —
// so the clone-time SSH→HTTPS rewrite must be a strict NO-OP on anything the
// allowlist authorized. If it were not, the string checked and the string dialled
// could diverge and the boundary would be bypassable.
//
// The invariant asserted, over inputs crafted to make a sloppy rewriter diverge:
// Allowlist.Match's output is a FIXED POINT of effectiveCloneURL, and the host
// actually dialled is the host the allowlist authorized. Refused inputs must be
// refused by the allowlist, never merely by the rewrite.
func TestCloneRewriteCannotBypassSourceRepoAllowlist(t *testing.T) {
	allow, err := sourcerepo.New([]string{"github.com/acme/*"})
	if err != nil {
		t.Fatalf("build allowlist: %v", err)
	}
	// A worst-case differ for this boundary: the rewrite is armed, and armed for
	// exactly the host the allowlist authorizes — so any drift between the string
	// checked and the string cloned shows up here rather than being masked by the
	// rewrite declining for an unrelated reason.
	d := &Differ{
		TokenSource:    func(context.Context) (string, error) { return "ghs_tok", nil },
		SSHRewriteHost: "github.com",
	}

	tests := []struct {
		name, repo string
		allowed    bool
	}{
		{"plain host/org/repo", "github.com/acme/x", true},
		{"https spelling", "https://github.com/acme/x.git", true},
		{"scp-style ssh spelling", "git@github.com:acme/x.git", true},
		{"ssh scheme spelling", "ssh://git@github.com/acme/x", true},
		{"uppercase host", "git@GitHub.COM:acme/x.git", true},
		{"userinfo lookalike host", "ssh://git@github.com@evil.example/acme/x", false},
		{"allowed host smuggled into the path", "git@evil.example:github.com/acme/x", false},
		{"ssh port smuggled as a path segment", "ssh://git@github.com:22/acme/x", false},
		{"fragment carrying a second url", "git@github.com:acme/x.git#ssh://git@evil.example/a/b", false},
		{"traversal out of the allowed org", "git@github.com:acme/../evil/x.git", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cloneURL, ok := allow.Match(tc.repo)
			if ok != tc.allowed {
				t.Fatalf("Match(%q) = (%q, %v), want allowed=%v", tc.repo, cloneURL, ok, tc.allowed)
			}
			if !ok {
				return // never reaches the differ at all
			}
			if got := d.effectiveCloneURL(cloneURL); got != cloneURL {
				t.Fatalf("clone rewrite moved an ALLOWLISTED url: authorized %q but would dial %q — "+
					"the allow check and the clone must see the same string", cloneURL, got)
			}
			if h := hostOf(cloneURL); h != "github.com" {
				t.Fatalf("authorized url %q resolves to host %q, want github.com", cloneURL, h)
			}
		})
	}
}
