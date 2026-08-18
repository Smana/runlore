// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// dottedCapitalI is U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE. strings.ToLower
// maps it to a plain ASCII 'i' (Unicode simple case mapping); idna.Lookup.ToASCII,
// which is what actually resolves a host before it is dialled, maps it to "i" +
// U+0307 and yields a different registrable label. It is written as an escape so
// this file does not itself contain the confusable it is about.
const dottedCapitalI = "\u0130"

// TestForgeGitHostNeverFoldsANonASCIIForgeHost pins the DERIVATION, on every key
// that can produce the credential boundary.
//
// forgeGitHost's result is Differ.TokenHost and Differ.SSHRewriteHost for
// what_changed and Differ.TokenHost for source_diff, which is to say: it decides
// who receives the forge credential. It used to reach that decision through a
// bare strings.ToLower, so a forge at "gİtlab.example.com" produced the boundary
// "gitlab.example.com" — a name someone else can register — while the operator's
// own instance, resolved through IDNA, matched nothing and was refused the
// credential it owns.
//
// config.Validate now refuses these configs outright (see
// TestValidateRefusesANonASCIIDerivedForgeHost), which is the loud half. This is
// the backstop: a *config.Config assembled without Load must still not produce a
// boundary that is fold-equal to a host it is not.
func TestForgeGitHostNeverFoldsANonASCIIForgeHost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     func(t *testing.T) *config.Config
		notWant string // the ASCII host the old fold produced
	}{
		{
			name: "gitlab base_url",
			cfg: func(t *testing.T) *config.Config {
				return gitlabForge(t, "g"+dottedCapitalI+"tlab.example.com")
			},
			notWant: "gitlab.example.com",
		},
		{
			name: "github api url",
			cfg: func(t *testing.T) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHubAPIURL = "https://g" + dottedCapitalI + "he.example.com/api/v3"
				return c
			},
			notWant: "ghe.example.com",
		},
		{
			name: "explicit git_host",
			cfg: func(t *testing.T) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHost = "g" + dottedCapitalI + "thub.com"
				return c
			},
			notWant: "github.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forgeGitHost(tc.cfg(t))
			if got == tc.notWant {
				t.Fatalf("forgeGitHost folded a non-ASCII forge host into %q. That host is "+
					"separately registrable and now collects the forge credential, while the "+
					"operator's own forge — which resolves elsewhere through IDNA — is refused "+
					"it, emptying what_changed with only a data-gaps line to show for it", got)
			}
			// Empty is not an acceptable answer either: whatchanged.Differ reads
			// an empty TokenHost as "attach the credential to EVERY clone".
			if got == "" {
				t.Fatal("forgeGitHost returned \"\", which whatchanged.Differ reads as an " +
					"unconfined credential — worse than the fold it replaced")
			}
			if !strings.Contains(got, dottedCapitalI) {
				t.Fatalf("forgeGitHost = %q: a host it will not fold must be returned "+
					"unchanged, so that it equals no clone URL's host at all", got)
			}
		})
	}

	// The other direction: ASCII case folding is exactly what this function is
	// for, and it must keep doing it. An operator who writes GIT_HOST in caps, or
	// a GHE API URL with a capitalised host, still gets a boundary that matches
	// the lowercased host every clone URL is reduced to.
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) *config.Config
		want string
	}{
		{"github.com by default", forgeConfigured, "github.com"},
		{
			name: "github enterprise, mixed case api url",
			cfg: func(t *testing.T) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHubAPIURL = "https://GHE.Example.COM/api/v3"
				return c
			},
			want: "ghe.example.com",
		},
		{
			name: "self-hosted gitlab, mixed case base_url",
			cfg:  func(t *testing.T) *config.Config { return gitlabForge(t, "GitLab.Example.COM") },
			want: "gitlab.example.com",
		},
		{
			name: "punycode idn forge is ASCII and folds normally",
			cfg:  func(t *testing.T) *config.Config { return gitlabForge(t, "XN--Gtlab-56a.example.com") },
			want: "xn--gtlab-56a.example.com",
		},
		{
			name: "explicit git_host, mixed case",
			cfg: func(t *testing.T) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHost = "  GHE.Example.COM  "
				return c
			},
			want: "ghe.example.com",
		},
	} {
		t.Run("ascii "+tc.name, func(t *testing.T) {
			if got := forgeGitHost(tc.cfg(t)); got != tc.want {
				t.Fatalf("forgeGitHost = %q, want %q — an ASCII forge that stops matching its "+
					"own clone URLs is the silent withholding of #495", got, tc.want)
			}
		})
	}
}

// TestForgeCredentialIsNotMintedForAHomoglyphOfTheForgeHost executes the
// credential decision rather than reasoning about it.
//
// whatchanged.Differ.auth returns BEFORE calling TokenSource when the clone URL
// is off-host, so "was the credential minted at all" is a direct, observable
// answer to "would this clone have carried the token" — no DNS and no reachable
// remote required, and it does not depend on where the subsequent dial ends up.
//
// The attacking clone URL here is the ASCII host the OLD fold produced. On the
// unfixed derivation the differ's TokenHost was exactly that string, so this
// clone minted and attached the forge credential.
func TestForgeCredentialIsNotMintedForAHomoglyphOfTheForgeHost(t *testing.T) {
	// .invalid is guaranteed never to resolve (RFC 2606), so the clone fails fast
	// and locally after the credential decision has already been made.
	const asciiFold = "gitlab.invalid"

	cfg := gitlabForge(t, "g"+dottedCapitalI+"tlab.invalid")
	d := buildGitOpsDiffer(cfg, discardLog())
	if d.TokenSource == nil {
		t.Fatal("no clone credential was wired; this test's premise is stale")
	}
	var minted atomic.Int32
	d.TokenSource = func(context.Context) (string, error) {
		minted.Add(1)
		return cloneSecret, nil
	}

	if _, err := d.Remote(context.Background(), "https://"+asciiFold+"/org/repo.git", "a", "b", ""); err == nil {
		t.Fatal("want a clone error from an unresolvable host, got nil")
	}
	if n := minted.Load(); n != 0 {
		t.Fatalf("the forge credential was minted %d time(s) for %q — the ASCII host the "+
			"operator's non-ASCII forge folds to. Anyone can register that name; on the "+
			"unfixed derivation it received the token", n, asciiFold)
	}

	// Non-vacuity, and the reason this probe means anything: the same probe on
	// the same differ DOES mint when the clone URL is the host the credential
	// belongs to. Without this, a differ that never minted for any reason at all
	// would pass the assertion above.
	t.Run("the probe is not vacuous", func(t *testing.T) {
		ascii := gitlabForge(t, asciiFold)
		ad := buildGitOpsDiffer(ascii, discardLog())
		var got atomic.Int32
		ad.TokenSource = func(context.Context) (string, error) {
			got.Add(1)
			return cloneSecret, nil
		}
		if _, err := ad.Remote(context.Background(), "https://"+asciiFold+"/org/repo.git", "a", "b", ""); err == nil {
			t.Fatal("want a clone error from an unresolvable host, got nil")
		}
		if got.Load() == 0 {
			t.Fatalf("a legitimate ASCII forge did not mint its own credential for its own "+
				"repo (%s) — the assertion above would pass vacuously, and a real deployment "+
				"would see an empty what_changed", asciiFold)
		}
	})
}

// TestSelfHostedForgeStillAuthenticatesItsOwnRepo is the transport-level other
// direction, on the provider path this change touched.
//
// TestWhatChangedAuthenticatesItsOwnGitHost already covers GitHub Enterprise
// with an explicit git_host. This covers the DERIVED path — the one that carries
// the defect and the one most deployments use — for a self-hosted GitLab: the
// boundary comes from forge.gitlab.base_url through forgeWebHost, and the token
// has to arrive on the wire at that host.
func TestSelfHostedForgeStillAuthenticatesItsOwnRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T, host string) *config.Config
	}{
		{
			name: "self-hosted gitlab derives its host from base_url",
			cfg:  gitlabForge,
		},
		{
			name: "github enterprise derives its host from github_api_url",
			cfg: func(t *testing.T, host string) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHubAPIURL = "https://" + host + "/api/v3"
				return c
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forge := newRecordingRemote(t)
			d := buildGitOpsDiffer(tc.cfg(t, forge.host(t)), discardLog())
			d.TokenSource = func(context.Context) (string, error) { return cloneSecret, nil }

			if _, err := d.Remote(context.Background(), forge.cloneURL(), "a", "b", ""); err == nil {
				t.Fatal("want a clone error from the stub remote, got nil")
			}
			hits, saw, tok := forge.observed()
			if hits == 0 {
				t.Fatal("no request reached the operator's own forge")
			}
			if !saw || tok != cloneSecret {
				t.Fatalf("the operator's own GitOps repo cloned WITHOUT the forge credential "+
					"(saw=%v token=%q) — a private repo would produce no diff at all, which is "+
					"the silent data gap this must not reintroduce", saw, tok)
			}
		})
	}
}
