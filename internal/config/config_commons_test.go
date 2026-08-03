// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// commonsCfg builds the smallest valid config with the commons enabled.
func commonsCfg(branch, ref string) *Config {
	c := &Config{}
	c.Catalog.Dir = "/var/lib/runlore/catalog"
	c.Catalog.Commons.URL = "https://github.com/Smana/runlore-kb-commons"
	c.Catalog.Commons.Dir = "/var/lib/runlore/commons"
	c.Catalog.Commons.Branch = branch
	c.Catalog.Commons.Ref = ref
	return c
}

// TestCommonsRefNormalisesToAPinnedRevision locks the contract between the config
// surface (branch | ref) and the single revision the syncer receives. The syncer
// tells a pin from a branch by the SPELLING of that string — refs/tags/<name> or a
// full object id — so what ApplyDefaults writes here is load-bearing, not cosmetic.
// The catalog-side half of the contract is TestSyncPinnedTagChecksOutThatRevision
// and friends in internal/catalog/sync_pinned_test.go.
func TestCommonsRefNormalisesToAPinnedRevision(t *testing.T) {
	const sha = "1a2b3c4d5e6f70819293a4b5c6d7e8f901234567" // 40 hex
	for _, tc := range []struct {
		name, branch, ref, want string
	}{
		{"unset tracks main", "", "", "main"},
		{"explicit branch is untouched", "release", "", "release"},
		{"bare ref is a tag", "", "v1.2.0", "refs/tags/v1.2.0"},
		{"qualified tag ref passes through", "", "refs/tags/v1.2.0", "refs/tags/v1.2.0"},
		{"full commit id passes through", "", sha, sha},
		{"uppercase commit id passes through", "", strings.ToUpper(sha), strings.ToUpper(sha)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := commonsCfg(tc.branch, tc.ref)
			ApplyDefaults(c)
			if got := c.Catalog.Commons.Branch; got != tc.want {
				t.Fatalf("effective commons revision = %q, want %q", got, tc.want)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestCommonsBranchAndRefAreMutuallyExclusive: the two keys mean opposite things
// (track a moving branch / freeze an immutable revision). Silently preferring one
// would leave an operator who pinned a ref believing their corpus is frozen while
// it tracks main — the exact failure the option exists to prevent.
func TestCommonsBranchAndRefAreMutuallyExclusive(t *testing.T) {
	c := commonsCfg("main", "v1.2.0")
	ApplyDefaults(c)
	err := c.Validate()
	if err == nil {
		t.Fatal("branch + ref together must be rejected, not silently resolved")
	}
	for _, want := range []string{"catalog.commons", "branch", "ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestCommonsRefRejectsUnpinnableSpellings: a value that is not an immutable
// revision must fail loudly at load. refs/heads/main under `ref` is the dangerous
// one — the syncer would treat any qualified ref as a pin and never fetch again,
// so the commons would silently freeze on whatever main pointed at that day.
func TestCommonsRefRejectsUnpinnableSpellings(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{
		{"branch ref", "refs/heads/main"},
		{"remote branch ref", "refs/remotes/origin/main"},
		{"abbreviated commit id", "1a2b3c4"},
		{"half a commit id", "1a2b3c4d5e6f7081"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := commonsCfg("", tc.ref)
			ApplyDefaults(c)
			if err := c.Validate(); err == nil {
				t.Fatalf("catalog.commons.ref = %q must be rejected", tc.ref)
			}
		})
	}
}

// TestCommonsBranchRejectsAPinnedSpelling keeps the two keys honest in the other
// direction: `branch: refs/tags/v1` would pin the corpus while the key says it
// tracks a branch. It cannot work today either (it clones refs/heads/refs/tags/v1),
// so nothing that works is broken by failing it at load instead of at clone.
func TestCommonsBranchRejectsAPinnedSpelling(t *testing.T) {
	for _, branch := range []string{"refs/tags/v1.2.0", "1a2b3c4d5e6f70819293a4b5c6d7e8f901234567"} {
		c := commonsCfg(branch, "")
		ApplyDefaults(c)
		if err := c.Validate(); err == nil {
			t.Fatalf("catalog.commons.branch = %q must be rejected in favour of ref", branch)
		}
	}
}

// TestCatalogGitBranchRejectsAPinnedSpelling: the operator's OWN catalog is
// deliberately not pinnable — the curator opens PRs against it, and a frozen read
// root would make "merged" and "in use" diverge forever with no signal. The syncer
// is shared, so a pinned spelling there would silently freeze the learning loop.
func TestCatalogGitBranchRejectsAPinnedSpelling(t *testing.T) {
	c := &Config{}
	c.Catalog.Dir = "/var/lib/runlore/catalog"
	c.Catalog.Git.URL = "https://github.com/acme/runlore-kb"
	c.Catalog.Git.Branch = "refs/tags/v1.2.0"
	ApplyDefaults(c)
	err := c.Validate()
	if err == nil {
		t.Fatal("catalog.git.branch must reject a pinned revision spelling")
	}
	if !strings.Contains(err.Error(), "catalog.git.branch") {
		t.Errorf("error %q does not name catalog.git.branch", err)
	}
}

// TestCommonsRefLoadsFromYAML: the key has to survive the strict decoder
// (KnownFields) — a struct field nobody can spell in runlore.yaml is dead weight.
func TestCommonsRefLoadsFromYAML(t *testing.T) {
	c := loadDoc(t, `
catalog:
  dir: /var/lib/runlore/catalog
  commons:
    url: https://github.com/Smana/runlore-kb-commons
    ref: v1.2.0
    dir: /var/lib/runlore/commons
`)
	if got := c.Catalog.Commons.Branch; got != "refs/tags/v1.2.0" {
		t.Fatalf("effective commons revision = %q, want refs/tags/v1.2.0", got)
	}
}
