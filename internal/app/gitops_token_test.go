// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// cloneSecret is the credential these tests watch for on the wire. It stands in
// for the minted GitHub App installation token / the GitLab access token: the
// real sources are swapped out AFTER buildGitOpsDiffer has wired the differ, so
// what is under test is the confinement, not the minting (which would need a
// live forge).
const cloneSecret = "tok_CLONE_SECRET"

// recordingRemote is a stand-in git remote. It answers every request with 404 —
// go-git gives up immediately — while recording that the request ARRIVED and
// which credential it carried.
//
// Both halves matter. The credential assertion alone is vacuous: a differ that
// never dials at all (a refused rewrite, a parse failure, a typo in the test's
// URL) also records no Authorization header, so every leak test here first
// asserts the request reached the remote. That is the HTTPS analogue of
// TestSSHRewriteNeverReachesAForeignHost's paired "invalid auth method" check.
type recordingRemote struct {
	srv *httptest.Server

	mu    sync.Mutex
	hits  int
	saw   bool   // a request carried HTTP basic auth
	token string // the password it carried
}

func newRecordingRemote(t *testing.T) *recordingRemote {
	t.Helper()
	r := &recordingRemote{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, pass, ok := req.BasicAuth()
		r.mu.Lock()
		r.hits++
		if ok {
			r.saw, r.token = true, pass
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// host is the remote's bare host, with no port — the spelling forge.git_host
// takes and the spelling the differ compares a clone URL against.
func (r *recordingRemote) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(r.srv.URL)
	if err != nil {
		t.Fatalf("parse stub remote url: %v", err)
	}
	return u.Hostname()
}

func (r *recordingRemote) cloneURL() string { return r.srv.URL + "/org/repo.git" }

func (r *recordingRemote) observed() (hits int, saw bool, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, r.saw, r.token
}

// gitlabForge returns a config whose forge is a self-managed GitLab at host,
// with a live access token in the environment.
func gitlabForge(t *testing.T, host string) *config.Config {
	t.Helper()
	t.Setenv("TEST_GITOPS_GITLAB_TOKEN", cloneSecret)
	cfg := &config.Config{}
	cfg.Forge.Provider = "gitlab"
	cfg.Forge.GitLab.BaseURL = "https://" + host
	cfg.Forge.GitLab.TokenEnv = "TEST_GITOPS_GITLAB_TOKEN"
	return cfg
}

// TestBuildGitOpsDifferConfinesTheForgeCredential pins WHICH host the
// what_changed differ may send its forge credential to.
//
// A GitOps spec.source.repoURL is cluster state: in a shared cluster anyone who
// can create an Argo CD Application or a Flux GitRepository chooses it. Until
// this was confined, the differ ran with TokenHost EMPTY, so "repoURL:
// https://attacker.example/org/repo.git" received the GitHub App installation
// token. Confining costs nothing an operator relies on — a forge credential is
// only ever valid on its own forge, so a clone that authenticates today keeps
// authenticating and a clone on any other host was already going to fail, just
// noisily instead of leaking.
//
// SSHRewriteHost must track the same host: the rewrite turns an SSH repoURL into
// a live HTTPS request, and it may only do that toward the host the credential
// is for.
func TestBuildGitOpsDifferConfinesTheForgeCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) *config.Config
		want string
	}{
		{
			name: "default github",
			cfg:  forgeConfigured,
			want: "github.com",
		},
		{
			name: "github enterprise, api on the git host",
			cfg: func(t *testing.T) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHubAPIURL = "https://ghe.example.com/api/v3"
				return c
			},
			want: "ghe.example.com",
		},
		{
			// Subdomain isolation: the API is at api.HOST, git is at HOST. The
			// API URL cannot answer which host serves git, so the operator names
			// it — config.Validate refuses this shape without the override
			// rather than letting the credential be withheld from every repo.
			name: "github enterprise with subdomain isolation, git host named",
			cfg: func(t *testing.T) *config.Config {
				c := forgeConfigured(t)
				c.Forge.GitHubAPIURL = "https://api.ghe.example.com"
				c.Forge.GitHost = "ghe.example.com"
				return c
			},
			want: "ghe.example.com",
		},
		{
			name: "gitlab follows its instance root",
			cfg:  func(t *testing.T) *config.Config { return gitlabForge(t, "gitlab.example.com") },
			want: "gitlab.example.com",
		},
		{
			name: "gitlab.com by default",
			cfg: func(t *testing.T) *config.Config {
				c := gitlabForge(t, "unused.example")
				c.Forge.GitLab.BaseURL = ""
				return c
			},
			want: "gitlab.com",
		},
		{
			// An explicit override wins on every provider, not only GitHub.
			name: "git_host overrides the derived host",
			cfg: func(t *testing.T) *config.Config {
				c := gitlabForge(t, "gitlab.example.com")
				c.Forge.GitHost = "Git.Example.COM"
				return c
			},
			want: "git.example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := buildGitOpsDiffer(tc.cfg(t), discardLog())
			if d.TokenSource == nil {
				t.Fatal("no clone credential was wired; this test's premise is stale")
			}
			if d.TokenHost != tc.want {
				t.Fatalf("TokenHost = %q, want %q — an unconfined differ sends the forge "+
					"credential to whatever host a repoURL names", d.TokenHost, tc.want)
			}
			if d.SSHRewriteHost != tc.want {
				t.Fatalf("SSHRewriteHost = %q, want %q — the rewrite must aim at the same host "+
					"the credential is for", d.SSHRewriteHost, tc.want)
			}
		})
	}

	// No credential at all (an unconfigured forge; a GitLab one whose token_env is
	// unset). There is nothing to confine, and the rewrite must NOT be armed on a
	// host merely derived from a forge URL — rewriting an SSH repoURL with no token
	// in hand trades go-git's SSH-agent path for an anonymous HTTPS 401 (#495).
	t.Run("no credential arms nothing", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Forge.Provider = "gitlab"
		d := buildGitOpsDiffer(cfg, discardLog())
		if d.TokenSource != nil {
			t.Fatal("an unconfigured forge must yield no clone credential; this test's premise is stale")
		}
		if d.SSHRewriteHost != "" || d.TokenHost != "" {
			t.Fatalf("TokenHost = %q, SSHRewriteHost = %q with no credential, want both empty",
				d.TokenHost, d.SSHRewriteHost)
		}
	})
}

// TestWhatChangedCredentialNeverReachesAForeignHost is the transport-level
// proof: not "auth() returned nil" but "nothing carrying the credential arrived
// at the attacker's server".
func TestWhatChangedCredentialNeverReachesAForeignHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) *config.Config
	}{
		{"github app", forgeConfigured},
		{"gitlab token", func(t *testing.T) *config.Config { return gitlabForge(t, "gitlab.example.com") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attacker := newRecordingRemote(t)
			d := buildGitOpsDiffer(tc.cfg(t), discardLog())
			d.TokenSource = func(context.Context) (string, error) { return cloneSecret, nil }

			if _, err := d.Remote(context.Background(), attacker.cloneURL(), "a", "b", ""); err == nil {
				t.Fatal("want a clone error from the stub remote, got nil")
			}
			hits, saw, tok := attacker.observed()
			if hits == 0 {
				t.Fatal("no request reached the stub remote — the credential assertion below " +
					"would pass vacuously")
			}
			if saw {
				t.Fatalf("the forge credential was sent to a host the repoURL merely named: "+
					"basic-auth password %q reached %s", tok, attacker.host(t))
			}
		})
	}
}

// TestWhatChangedAuthenticatesItsOwnGitHost is the other direction, and the
// reason this confinement was deferred once already: withholding the credential
// from the operator's OWN GitOps repo turns every what_changed into an empty
// result, reported only as a data-gaps line (RunLore #495).
//
// The config here is the shape that makes the derivation ambiguous — GitHub
// Enterprise with subdomain isolation, API at api.HOST and git at HOST — resolved
// by naming the git host explicitly.
func TestWhatChangedAuthenticatesItsOwnGitHost(t *testing.T) {
	forge := newRecordingRemote(t)
	cfg := forgeConfigured(t)
	cfg.Forge.GitHubAPIURL = "https://api." + forge.host(t)
	cfg.Forge.GitHost = forge.host(t)

	d := buildGitOpsDiffer(cfg, discardLog())
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
			"(saw=%v token=%q) — a private repo would produce no diff at all, the silent "+
			"data gap this confinement must not reintroduce", saw, tok)
	}
}
