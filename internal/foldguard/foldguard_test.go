// SPDX-License-Identifier: Apache-2.0

package foldguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/idna"
)

// ---------------------------------------------------------------------------
// The repo-level assertions
// ---------------------------------------------------------------------------

// TestNoValueIsCaseNormalisedTwoDifferentWays is the general arm of the guard,
// and the one that pays for the whole file.
//
// The rule is one sentence: WITHIN A PACKAGE, A VALUE MUST NOT BE REACHED BY TWO
// DIFFERENT CASE NORMALISATIONS. That is the whole of instance 2 — kind was
// ToLower'd for the Secret refusal and EqualFold'd for the resolution, and the
// gap between Unicode simple case MAPPING and simple case FOLDING (ToLower does
// not fold U+017F to 's'; EqualFold does) is the bypass. It does not matter which
// normaliser is "right": as soon as two of them see one value, an input exists
// that the check and the use disagree about, and the attacker picks it.
//
// The allowlist is EMPTY, deliberately. Measured over the 218 shipped files under
// internal/ this rule fires ZERO times, and it fires exactly once — on
// resourcespec.go — against the branch carrying instance 2. A rule with no false
// positives on the real tree can be enforced outright, which is worth far more
// than a rule with an escape hatch: there is nothing here to add yourself to.
//
// If this fails, the fix is never "pick the other normaliser at the second site".
// It is to normalise ONCE, at the boundary, and pass the normalised value down —
// or to refuse non-ASCII input, which is what makes every normaliser agree (see
// TestTheNormalisersReallyDisagreeAndOnlyOnNonASCII).
func TestNoValueIsCaseNormalisedTwoDifferentWays(t *testing.T) {
	pkgs, fset := internalPackages(t)

	// Inertness: a reader that stops finding packages, stops finding folds, or
	// whose normaliser table has been emptied would pass forever having checked
	// nothing. That is the failure mode this whole file exists to prevent, so it
	// is asserted rather than hoped for.
	if len(normalisers) == 0 {
		t.Fatal("the normaliser table is empty — this guard is inert")
	}
	total := 0
	for _, p := range pkgs {
		total += len(scanFolds(p.files, fset))
	}
	if total < minFoldSites {
		t.Fatalf("found %d case-normalisation call sites under internal/, expected at least %d — "+
			"either the tree moved or scanFolds stopped matching, and this guard is now checking nothing",
			total, minFoldSites)
	}

	for _, p := range pkgs {
		for _, v := range divergentValues(scanFolds(p.files, fset)) {
			t.Error(v)
		}
	}
}

// TestEveryCaseFoldedHostComparisonIsPinned is the arm for instance 1, and it is
// shaped differently ON PURPOSE — because the second normaliser is not in this
// repo at all.
//
// hostOf folded with strings.ToLower; the disagreeing normaliser was
// idna.Lookup.ToASCII, called by net/http several packages deep inside a
// dependency. No AST walk over internal/ can see it. What it CAN see is the
// premise: a string that names a URL HOST is always IDNA-normalised by whatever
// eventually dials it, so case-folding a host in Go and comparing the result is
// ALREADY two normalisations of one value — the rule above, with the second
// normaliser supplied by the world rather than by the source.
//
// That reading is correct but it is not free: five such comparisons exist in
// shipped code today, and this guard cannot tell a protected one from an exposed
// one (protection lives in a caller, or in a byte-range allowlist several
// functions away). So this arm does not try to. It PINS THE POPULATION: the set
// of functions comparing a case-folded host must be exactly knownHostComparisons,
// each entry carrying, in prose, why it is or is not safe.
//
// The pin RE-ARMS in both directions, which is the only reason it is worth
// having:
//
//   - a NEW function that compares a case-folded host fails until its author
//     either refuses non-ASCII or writes down why it is safe. That is precisely
//     the review #495 did not get: it added effectiveCloneURL, a new authorisation
//     comparing hostOf(...) == SSHRewriteHost, to a package whose existing fold
//     was already pinned — this guard would have stopped it at introduction.
//   - a pin that stops matching fails as STALE, so a site that gets fixed or
//     renamed cannot leave a permanent acknowledgement behind. That is the
//     property hack/check-screenshots-fresh.sh had to learn the hard way.
func TestEveryCaseFoldedHostComparisonIsPinned(t *testing.T) {
	pkgs, _ := internalPackages(t)

	found := map[string]bool{}
	for _, p := range pkgs {
		for _, site := range hostComparisons(p.name, p.files) {
			found[site] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("found no case-folded host comparison anywhere under internal/, but " +
			"knownHostComparisons names several — the reader has gone inert")
	}

	for site := range found {
		if _, ok := knownHostComparisons[site]; !ok {
			t.Errorf("%s compares a CASE-FOLDED HOST.\n"+
				"Go folds it with strings.ToLower/EqualFold (Unicode simple case mapping/folding); "+
				"whatever dials the URL resolves the SAME host with idna.Lookup.ToASCII, and the two "+
				"disagree on non-ASCII input — ToLower(U+0130) is 'i' while IDNA yields \"i\"+U+0307, a "+
				"DIFFERENT registrable label. That is how #498 authorised gİthub.com and dialled "+
				"xn--github-qyd.com with a GitHub App token attached.\n"+
				"Refuse any host carrying a byte above 7-bit ASCII before folding it — "+
				"internal/sourcerepo/allowlist.go firstDisallowedRepoChar is the pattern — or add %q to "+
				"knownHostComparisons with a written reason it is safe.", site, site)
		}
	}
	for site, why := range knownHostComparisons {
		if !found[site] {
			t.Errorf("knownHostComparisons pins %q, which no longer compares a case-folded host "+
				"(%s). Delete the entry: a pin that has stopped matching is an acknowledgement that has "+
				"silently become permanent.", site, why)
		}
	}
}

// knownHostComparisons is every function under internal/ that compares a
// case-folded host, with the reason it is (or is not yet) safe.
//
// This is an inventory, NOT an exoneration. Two of these are known to be
// unprotected; they are recorded rather than fixed because each fix is a product
// decision — refusing non-ASCII hosts outright would break a legitimately
// internationalised self-hosted forge — and belongs in its own change with its
// own tests. What the inventory buys is that the list cannot GROW in silence.
//
// A FIXED SITE STILL NEEDS AN ENTRY, and that is deliberate. The remediation both
// incidents converged on refuses non-ASCII input; it does not remove the fold, so
// the comparison is still here and this reader still sees it. Verified against
// fix/gitops-ssh-repourl-normalise: after the ASCII guard lands, whatchanged's
// effectiveCloneURL and sshToHTTPS still report, and the branch will have to add
// two entries here saying so. That is the entry doing its job — the prose is where
// "this one is protected, and by what" gets written down, which is exactly the
// knowledge #495 was missing when it reused a fold that predated it.
var knownHostComparisons = map[string]string{
	"whatchanged auth": "confines a GitHub App installation token to TokenHost. This is instance 1's " +
		"package. The fold is hostOf's strings.ToLower; the ASCII refusal that closes it lands in " +
		"sshToHTTPS on fix/gitops-ssh-repourl-normalise. Until that merges, an HTTPS clone URL with a " +
		"non-ASCII host still reaches auth — a hole that PREDATES the SSH rewrite and is called out in " +
		"effectiveCloneURL's own 'WHAT THIS DOES NOT DO' comment.",

	"httpx DenyInternalRedirect": "decides whether to strip provider key headers when a redirect " +
		"changes host. UNPROTECTED: neither hostname is ASCII-checked. The exploitable direction is " +
		"fold-equal but IDNA-DIFFERENT (headers retained across a real host change); no such pair is " +
		"demonstrated today because IDNA's UTS-46 mapping subsumes simple folding for the runes tried, " +
		"which is a reason to keep watching it, not a proof that it is safe.",

	"config isPrivateHost": "classifies a config endpoint as private so a key may travel over plain " +
		"http. UNPROTECTED, but the exploitable direction needs ToLower to PRODUCE one of 'localhost', " +
		"'.local', '.internal', '.svc', '.cluster.local' from a host IDNA maps elsewhere; ToLower does no " +
		"folding, so no such input is known. Pure and network-free by design, so it never dials what it " +
		"classified — the disagreement has no second half here.",

	"app githubGitHost": "derives the git host from the configured GitHub API URL and compares it to " +
		"api.github.com. Operator-supplied config rather than cluster state, so it is not the same " +
		"threat as a repoURL anyone with Application create can set; still folded, still compared, " +
		"still worth failing on if a second comparison is added here.",

	"thread prNumberInRepo": "matches a link's host against the configured forge repo before " +
		"reading a PR number out of it. Both sides are operator config, and the function's own comment " +
		"already reasons about non-ASCII for the PATH comparison — it does not reason about the HOST one.",
}

// minFoldSites is a floor on how many case-normalisation call sites the reader
// must find under internal/, so a reader that silently stops matching (a moved
// tree, a renamed stdlib helper, a parser flag change) fails instead of passing
// on an empty set. It is deliberately far below the real count (70 at the time of
// writing) so ordinary churn never touches it.
const minFoldSites = 40

// ---------------------------------------------------------------------------
// The guard's own mutation test
// ---------------------------------------------------------------------------

// TestFoldGuardCatchesTheShapesItClaimsTo feeds both historical instances back
// through the checkers as source, together with the shapes the checkers must stay
// SILENT on.
//
// Both halves are load-bearing. A checker that matched nothing would pass the two
// repo-level tests above forever — the allowlist for the first is empty and the
// pin for the second is satisfied by finding nothing new — which is exactly the
// inert-guard failure this file is about. And a checker that matched everything
// would be muted within a week, which is the argument hack/check-screenshots-fresh.sh
// makes about itself and then watched come true.
func TestFoldGuardCatchesTheShapesItClaimsTo(t *testing.T) {
	for _, tc := range []struct {
		name, src, wantDiverge, wantHost string
		// noFolds marks the one fixture that deliberately contains no case
		// normalisation at all, so the emptiness check below stays an assertion
		// everywhere else.
		noFolds bool
	}{
		// ---- instance 2, as it shipped: two normalisers, one value ----
		{
			name: "INSTANCE 2: the Secret refusal folds one way and the resolution another",
			src: `package cluster

func (r *SpecReader) resolveKind(kind string) ([]resolved, error) {
	for _, l := range lists {
		for _, ar := range l.APIResources {
			if strings.Contains(ar.Name, "/") || !strings.EqualFold(ar.Kind, kind) {
				continue
			}
			out = append(out, resolved{gvr: gv.WithResource(ar.Name)})
		}
	}
	return out, nil
}

func (r *SpecReader) ResourceSpec(ctx context.Context, w providers.Workload) (providers.ResourceSpec, error) {
	if why, refused := refusedKinds[strings.ToLower(w.Kind)]; refused {
		out.Outcome = providers.ResourceForbidden
		out.Detail = why
		return out, nil
	}
	return r.read(r.resolveKind(w.Kind))
}
`,
			wantDiverge: `"kind"`,
		},
		{
			name: "INSTANCE 2's fix: one normaliser for both the refusal and the resolution",
			src: `package cluster

func (r *SpecReader) resolveKind(kind string) ([]resolved, error) {
	if !strings.EqualFold(ar.Kind, kind) {
		return nil, nil
	}
	return out, nil
}

func (r *SpecReader) ResourceSpec(w providers.Workload) error {
	for refused := range refusedKinds {
		if strings.EqualFold(w.Kind, refused) {
			return errRefused
		}
	}
	return nil
}
`,
		},

		// ---- instance 1: a case-folded host reaching an authorisation ----
		{
			name: "INSTANCE 1: the SSH rewrite authorises on a ToLower'd host",
			src: `package whatchanged

func hostOf(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (d *Differ) effectiveCloneURL(rawURL string) string {
	httpsURL, ok := sshToHTTPS(rawURL)
	if !ok || d.SSHRewriteHost == "" {
		return rawURL
	}
	if hostOf(httpsURL) != d.SSHRewriteHost {
		return rawURL
	}
	return httpsURL
}
`,
			wantHost: "whatchanged effectiveCloneURL",
		},
		{
			name: "INSTANCE 1: the token confinement in auth, folded the same way",
			src: `package whatchanged

func hostOf(cloneURL string) string { return strings.ToLower(u.Hostname()) }

func (d *Differ) auth(ctx context.Context, cloneURL string) (transport.AuthMethod, error) {
	if d.TokenHost != "" && hostOf(cloneURL) != d.TokenHost {
		return nil, nil
	}
	return &http.BasicAuth{Username: "x-access-token", Password: tok}, nil
}
`,
			wantHost: "whatchanged auth",
		},
		{
			name: "INSTANCE 1 written inline, without the hostOf helper",
			src: `package whatchanged

func (d *Differ) auth(cloneURL string) error {
	if strings.ToLower(u.Hostname()) != d.TokenHost {
		return nil
	}
	return attach()
}
`,
			wantHost: "whatchanged auth",
		},
		{
			name: "a host compared with EqualFold folds and compares in one call",
			src: `package httpx

func DenyInternalRedirect(req *http.Request, via []*http.Request) error {
	if !strings.EqualFold(req.URL.Hostname(), via[len(via)-1].URL.Hostname()) {
		strip(req)
	}
	return nil
}
`,
			wantHost: "httpx DenyInternalRedirect",
		},
		{
			name: "a folded host used as a map key is a comparison too",
			src: `package forge

func pick(raw string) bool {
	u, _ := url.Parse(raw)
	h := strings.ToLower(u.Hostname())
	return trustedHosts[h]
}
`,
			wantHost: "forge pick",
		},
		{
			name: "a folded host reaching a comparison through a local variable",
			src: `package forge

func pick(raw string) bool {
	u, _ := url.Parse(raw)
	got := hostOf(raw)
	alias := got
	return alias == "github.com"
}

func hostOf(s string) string { return strings.ToLower(u.Hostname()) }
`,
			wantHost: "forge pick",
		},

		{
			name: "a folded host reaching a comparison through a BACKWARD alias chain",
			src: `package forge

func pick(raws []string) bool {
	var alias, got string
	for _, raw := range raws {
		if alias == "github.com" {
			return true
		}
		alias = got
		got = hostOf(raw)
	}
	return false
}

func hostOf(s string) string { return strings.ToLower(u.Hostname()) }
`,
			wantHost: "forge pick",
		},

		// ---- shapes the guard MUST stay silent on ----
		{
			name: "one normaliser used many times on one value is fine",
			src: `package kbimport

func inferTags(typ string, tags []string) []string {
	out := []string{strings.ToLower(typ)}
	for _, t := range tags {
		out = append(out, strings.ToLower(strings.TrimSpace(t)))
	}
	return out
}
`,
		},
		{
			name: "two normalisers on two DIFFERENT values is fine",
			src: `package notify

func route(status, title string) bool {
	if strings.ToLower(status) == "retired" {
		return false
	}
	return strings.EqualFold(title, alertName)
}
`,
		},
		{
			name: "deriving a host without comparing it is not a decision",
			src: `package app

func forgeWebHost(cfg *config.Config) string {
	u, err := url.Parse(cfg.Forge.GitLab.BaseURL)
	if err != nil || u.Hostname() == "" {
		return "gitlab.com"
	}
	return strings.ToLower(u.Hostname())
}
`,
		},
		{
			name: "folding a NON-host value and comparing it is the other arm's business",
			src: `package logging

func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	}
	return 0, errUnknown
}
`,
		},
		{
			name: "the repo's ASCII-anchored EqualFold scan is a single normaliser",
			src: `package thread

func commandTokenIndex(s, prefix string) (int, bool) {
	for i := 0; i+len(prefix) <= len(s); i++ {
		if !strings.EqualFold(s[i:i+len(prefix)], prefix) {
			continue
		}
		return i, true
	}
	return 0, false
}
`,
		},
		{
			name: "a fold of a string LITERAL pins nothing and must not group",
			src: `package catalog

func want() bool {
	return strings.EqualFold(h, "Summary") && strings.ToLower("Summary") == h
}
`,
		},

		// ---- the checkers must match on the NORMALISER, not on the shape ----
		{
			name: "swapping EqualFold for a non-normalising call leaves one normaliser",
			src: `package cluster

func (r *SpecReader) resolveKind(kind string) error {
	if !strings.Contains(ar.Kind, kind) {
		return nil
	}
	return nil
}

func (r *SpecReader) ResourceSpec(w providers.Workload) error {
	if _, refused := refusedKinds[strings.ToLower(w.Kind)]; refused {
		return errRefused
	}
	return nil
}
`,
		},
		{
			name: "comparing a host that was never folded is not this guard's business",
			src: `package whatchanged

func (d *Differ) auth(cloneURL string) error {
	if u.Hostname() != d.TokenHost {
		return nil
	}
	return attach()
}
`,
			noFolds: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			files := parseSource(t, fset, tc.src)
			pkg := files[0].Name.Name

			sites := scanFolds(files, fset)
			// Without this, a fixture that failed to exercise the reader at all
			// would "pass" every silence case and prove nothing.
			if (len(sites) == 0) != tc.noFolds {
				t.Fatalf("fixture has %d case normalisations but noFolds=%v — a fixture that "+
					"never reaches the reader proves nothing", len(sites), tc.noFolds)
			}

			diverged := strings.Join(divergentValues(sites), "\n")
			switch {
			case tc.wantDiverge == "" && diverged != "":
				t.Errorf("want no divergence reported, got:\n%s", diverged)
			case tc.wantDiverge != "" && !strings.Contains(diverged, tc.wantDiverge):
				t.Errorf("want a divergence naming %s, got:\n%s", tc.wantDiverge, diverged)
			}

			hosts := hostComparisons(pkg, files)
			switch {
			case tc.wantHost == "" && len(hosts) != 0:
				t.Errorf("want no case-folded host comparison reported, got %v", hosts)
			case tc.wantHost != "" && !slices.Contains(hosts, tc.wantHost):
				t.Errorf("want the host comparison %q reported, got %v", tc.wantHost, hosts)
			}
		})
	}
}

// TestTheNormalisersReallyDisagreeAndOnlyOnNonASCII pins the PREMISE the two
// guards above rest on, against the real normaliser implementations rather than
// against a restatement of what they are believed to do.
//
// It is not here to discover that Unicode is hard. It is here so that (a) the
// exact inputs from both incidents are on the record as inputs that genuinely
// diverge, rather than as a story in a comment, and (b) if a Go or x/net upgrade
// ever changed these tables, the guard's justification would fail loudly instead
// of the guard quietly protecting against nothing.
//
// The second half is the more important one, because it is the whole reason the
// remediation both incidents converged on works: ON PURE 7-BIT ASCII, EVERY ONE
// OF THESE NORMALISERS AGREES. Refusing a non-ASCII byte therefore kills the
// class rather than the rune, which is what internal/sourcerepo/allowlist.go had
// already worked out and what internal/whatchanged now does too.
func TestTheNormalisersReallyDisagreeAndOnlyOnNonASCII(t *testing.T) {
	// U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE and U+017F LATIN SMALL LETTER
	// LONG S are written as escapes on purpose: staticcheck ST1018 rejects the
	// literals, and a test about invisible-difference characters that contains
	// them literally is the same trap one level up.
	const (
		dottedCapitalI = "\u0130" // ToLower -> "i"; IDNA -> "i" + U+0307
		longS          = "\u017F" // EqualFold -> "s"; ToLower -> itself
	)

	// Instance 1: strings.ToLower vs idna.Lookup.ToASCII on a host.
	host := "g" + dottedCapitalI + "thub.com"
	if got := strings.ToLower(host); got != "github.com" {
		t.Fatalf("ToLower(%q) = %q, want %q — instance 1's premise no longer holds", host, got, "github.com")
	}
	viaIDNA, err := idna.Lookup.ToASCII(host)
	if err == nil && viaIDNA == "github.com" {
		t.Fatalf("idna.Lookup.ToASCII(%q) = %q — it now AGREES with ToLower, so the "+
			"whatchanged bypass is closed by the tables themselves", host, viaIDNA)
	}

	// Instance 2: strings.ToLower vs strings.EqualFold on a kind.
	kind := longS + "ecret"
	if strings.ToLower(kind) == "secret" {
		t.Fatalf("ToLower(%q) now equals %q — instance 2's premise no longer holds", kind, "secret")
	}
	if !strings.EqualFold(kind, "secret") {
		t.Fatalf("EqualFold(%q, %q) is now false — instance 2's premise no longer holds", kind, "secret")
	}

	// And the remediation: on ASCII every normaliser lands in the same place, so
	// no pair of them can be played off against each other.
	for r := rune(0); r < 0x80; r++ {
		s := "a" + string(r) + "z"
		low := strings.ToLower(s)
		if !strings.EqualFold(s, low) {
			t.Errorf("ASCII %U: EqualFold disagrees with ToLower (%q vs %q)", r, s, low)
		}
		if got := strings.ToLower(low); got != low {
			t.Errorf("ASCII %U: ToLower is not idempotent (%q -> %q)", r, low, got)
		}
	}
	for _, h := range []string{"github.com", "GitHub.COM", "gitlab.example.internal", "SVC.cluster.local"} {
		viaIDNA, err := idna.Lookup.ToASCII(h)
		if err != nil {
			t.Errorf("idna.Lookup.ToASCII(%q): %v", h, err)
			continue
		}
		if got := strings.ToLower(h); got != viaIDNA {
			t.Errorf("ASCII host %q: ToLower gives %q but IDNA gives %q — the ASCII "+
				"remediation does not hold and both guards need rethinking", h, got, viaIDNA)
		}
	}
}

// ---------------------------------------------------------------------------
// The checkers
//
// WHAT THIS FILE DOES NOT COVER, stated plainly so nobody mistakes a green run
// for an absence of this defect class:
//
//   - VALUE IDENTITY IS SYNTACTIC. Two folds are considered to touch the same
//     value when they are in the same package and their arguments share a final
//     identifier (w.Kind, ar.Kind and kind all key to "kind"). That over-groups
//     two unrelated `kind`s in one package (measured: zero such collisions across
//     the 218 shipped files) and under-groups a value renamed as it crosses a
//     package boundary — a fold in package A and a differing fold in package B on
//     the same string are INVISIBLE here.
//   - ONLY CASE NORMALISERS. Unicode normalisation (NFC/NFKC), percent-decoding,
//     path cleaning, punycode and trailing-dot stripping are all the same defect
//     class and none of them are tracked. The table is small because the repo's
//     normaliser vocabulary is small (44 ToLower, 14 EqualFold, nothing else);
//     adding golang.org/x/text/unicode/norm to internal/ would need this table
//     extended, and nothing forces that.
//   - NO DATA FLOW. There is no type checker here. A value folded, stored in a
//     struct field, and folded differently three calls later is not followed.
//   - THE HOST ARM PINS, IT DOES NOT PROVE. It cannot tell a host comparison
//     protected by an ASCII refusal from an exposed one; the judgement lives in
//     prose in knownHostComparisons, and prose can be wrong.
//   - HOSTS ARE RECOGNISED BY SHAPE. `.Hostname()`, `.Host`, or an identifier
//     named host/hostname all key as a host. A host held in a differently-named
//     variable is missed.
//   - THE HOST ARM IS KEYED BY FUNCTION, so it sees a NEW comparison, not a new
//     USE of one already pinned. #495 happened to add its authorisation in a new
//     function (effectiveCloneURL) and is therefore caught — verified by running
//     this guard against a820c95. Had it instead widened what the already-pinned
//     auth accepts, nothing here would have moved. Two same-named methods on
//     different types in one package also collide into one key.
// ---------------------------------------------------------------------------

// normalisers maps a fully-qualified callee to the case normalisation it applies.
// Two entries mapping to DIFFERENT names is what "normalised two ways" means, so
// strings.ToLower and bytes.ToLower deliberately share a name: they agree.
var normalisers = map[string]string{
	"strings.ToLower":        "ToLower",
	"strings.ToUpper":        "ToUpper",
	"strings.ToTitle":        "ToTitle",
	"strings.ToLowerSpecial": "ToLowerSpecial",
	"strings.ToUpperSpecial": "ToUpperSpecial",
	"strings.EqualFold":      "EqualFold",
	"bytes.ToLower":          "ToLower",
	"bytes.ToUpper":          "ToUpper",
	"bytes.EqualFold":        "EqualFold",
}

// passthrough calls do not change case, so a fold of one of them is a fold of its
// first argument: strings.ToLower(strings.TrimSuffix(host, ".")) normalises host.
var passthrough = map[string]bool{
	"strings.TrimSpace":  true,
	"strings.TrimSuffix": true,
	"strings.TrimPrefix": true,
	"strings.Trim":       true,
	"strings.TrimLeft":   true,
	"strings.TrimRight":  true,
	"string":             true,
}

// foldSite is one case normalisation applied to one value.
type foldSite struct {
	fn    string // enclosing function, for the report
	norm  string // which normalisation
	value string // the syntactic value key (see valueKey)
	pos   string // file:line
}

// scanFolds returns every case normalisation applied to a non-literal value in a
// package's shipped files.
func scanFolds(files []*ast.File, fset *token.FileSet) []foldSite {
	var out []foldSite
	for _, f := range files {
		eachScope(f, func(fn string, n ast.Node) {
			ast.Inspect(n, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				norm, ok := normalisers[types.ExprString(call.Fun)]
				if !ok {
					return true
				}
				for _, arg := range call.Args {
					// A literal is its own canonical form; grouping on one would
					// tie together every site that happens to mention "secret".
					if _, isLit := unwrapPassthrough(arg).(*ast.BasicLit); isLit {
						continue
					}
					key, _ := valueKey(arg)
					if key == "" {
						continue
					}
					out = append(out, foldSite{
						fn:    fn,
						norm:  norm,
						value: key,
						pos:   position(fset, call.Pos()),
					})
				}
				return true
			})
		})
	}
	return out
}

// divergentValues reports every value in a package reached by more than one case
// normalisation — the shape of instance 2, and the general shape of the class.
func divergentValues(sites []foldSite) []string {
	byValue := map[string]map[string][]foldSite{}
	for _, s := range sites {
		if byValue[s.value] == nil {
			byValue[s.value] = map[string][]foldSite{}
		}
		byValue[s.value][s.norm] = append(byValue[s.value][s.norm], s)
	}

	var out []string
	for _, value := range slices.Sorted(maps.Keys(byValue)) {
		norms := byValue[value]
		if len(norms) < 2 {
			continue
		}
		var where []string
		for _, norm := range slices.Sorted(maps.Keys(norms)) {
			for _, s := range norms[norm] {
				where = append(where, fmt.Sprintf("%s at %s (%s)", norm, s.pos, s.fn))
			}
		}
		out = append(out, fmt.Sprintf(
			"the value %q is case-normalised %d DIFFERENT ways in this package:\n\t%s\n"+
				"An input exists that these normalisers disagree about, so whichever of them gates a "+
				"refusal can be stepped around by the other — that is exactly how kind %q skipped the "+
				"Secret refusal and still resolved to v1/secrets (ToLower does not fold U+017F to 's'; "+
				"EqualFold does). Normalise ONCE at the boundary and pass the normalised value down, or "+
				"refuse non-ASCII input.",
			value, len(norms), strings.Join(where, "\n\t"), "\u017Fecret"))
	}
	return out
}

// hostComparisons returns "<pkg> <func>" for every function that COMPARES a
// case-folded host — the visible half of instance 1.
//
// A value is a folded host when it is a case normalisation of a host-shaped
// expression, a call to a same-package function that returns one (hostOf,
// lowerHost, githubGitHost), or a local bound to either. A comparison is ==, !=,
// a map index, or EqualFold — which folds and compares in a single call and so
// needs no separate fold to have happened first.
func hostComparisons(pkg string, files []*ast.File) []string {
	hostFuncs := hostReturningFuncs(files)

	var out []string
	for _, f := range files {
		eachScope(f, func(fn string, n ast.Node) {
			c := &hostChecker{hostFuncs: hostFuncs, folded: map[string]bool{}}
			c.bindFixpoint(n)
			if c.compares(n) {
				out = append(out, pkg+" "+fn)
			}
		})
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// hostReturningFuncs finds the same-package functions whose body case-normalises
// a host and whose result is a string, so `hostOf(cloneURL) != d.TokenHost` in a
// DIFFERENT function is still recognised as comparing a folded host. Without this
// the guard would only ever see hosts folded and compared in one breath, which is
// not how either incident was written.
func hostReturningFuncs(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil || !returnsString(fd.Type) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || normalisers[types.ExprString(call.Fun)] == "" {
					return true
				}
				for _, arg := range call.Args {
					if _, host := valueKey(arg); host {
						out[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	return out
}

type hostChecker struct {
	hostFuncs map[string]bool
	folded    map[string]bool // expressions holding a case-folded host
}

// bindFixpoint repeats the binding pass until it stops learning, so a chain of
// assignments is followed regardless of the order the passes see them in.
func (c *hostChecker) bindFixpoint(n ast.Node) {
	for {
		before := len(c.folded)
		c.bind(n)
		if len(c.folded) == before {
			return
		}
	}
}

func (c *hostChecker) bind(n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if i < len(s.Lhs) && c.isFoldedHost(rhs) {
					c.folded[types.ExprString(s.Lhs[i])] = true
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if i < len(s.Values) && c.isFoldedHost(s.Values[i]) {
					c.folded[id.Name] = true
				}
			}
		}
		return true
	})
}

// isFoldedHost reports whether e evaluates to a host that has been case-folded in
// Go — and therefore normalised differently from the way the dialler will.
func (c *hostChecker) isFoldedHost(e ast.Expr) bool {
	if call, ok := e.(*ast.CallExpr); ok {
		callee := types.ExprString(call.Fun)
		if normalisers[callee] != "" {
			for _, arg := range call.Args {
				if _, host := valueKey(arg); host {
					return true
				}
			}
			return false
		}
		if id, ok := call.Fun.(*ast.Ident); ok && c.hostFuncs[id.Name] {
			return true
		}
		return false
	}
	return c.folded[types.ExprString(e)]
}

// compares reports whether the scope makes a decision on a folded host.
func (c *hostChecker) compares(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if (e.Op == token.EQL || e.Op == token.NEQ) &&
				(c.isFoldedHost(e.X) || c.isFoldedHost(e.Y)) {
				found = true
			}
		case *ast.IndexExpr:
			if c.isFoldedHost(e.Index) {
				found = true
			}
		case *ast.CallExpr:
			// EqualFold folds and compares in one call, so the host does not need
			// to have been folded into a variable first.
			if normalisers[types.ExprString(e.Fun)] != "EqualFold" {
				return true
			}
			for _, arg := range e.Args {
				if _, host := valueKey(arg); host {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// Shared syntax helpers
// ---------------------------------------------------------------------------

// valueKey reduces an expression to the identity two folds must share to count as
// touching the same value, and reports whether that value names a URL host.
//
// The key is the expression's final identifier, lowercased, after stripping
// case-preserving wrappers — so w.Kind, ar.Kind and kind all key to "kind", which
// is what ties instance 2's refusal to instance 2's resolution. It is a
// HEURISTIC and the file's limitations comment says so.
func valueKey(e ast.Expr) (key string, host bool) {
	base := types.ExprString(unwrapPassthrough(e))
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(strings.TrimSuffix(base, "()"))
	// The trailing "()" strip is what makes u.Hostname() and req.URL.Hostname()
	// key as "hostname", so url.URL's accessor and a plain `host` variable are
	// recognised by the same one-line rule.
	return base, base == "host" || base == "hostname"
}

// unwrapPassthrough peels case-preserving calls off an expression.
func unwrapPassthrough(e ast.Expr) ast.Expr {
	for {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 || !passthrough[types.ExprString(call.Fun)] {
			return e
		}
		e = call.Args[0]
	}
}

// eachScope visits each top-level declaration with the name to report it under.
// Package-level declarations are visited too: a `var x = strings.ToLower(y)` is
// as much a normalisation as one inside a function.
func eachScope(f *ast.File, visit func(fn string, n ast.Node)) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			visit("<package-level>", d)
			continue
		}
		if fd.Body == nil {
			continue
		}
		visit(fd.Name.Name, fd.Body)
	}
}

func returnsString(t *ast.FuncType) bool {
	if t.Results == nil {
		return false
	}
	for _, r := range t.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok && id.Name == "string" {
			return true
		}
	}
	return false
}

func position(fset *token.FileSet, p token.Pos) string {
	pos := fset.Position(p)
	return fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

type parsedPkg struct {
	name  string // directory path relative to internal/, e.g. "providers/cluster"
	files []*ast.File
}

// internalPackages parses the SHIPPED (non-test) Go files under internal/, one
// entry per directory. A guard that read test files could be satisfied by a test.
func internalPackages(t *testing.T) ([]parsedPkg, *token.FileSet) {
	t.Helper()
	root := ".."
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", p, perr)
		}
		dir := filepath.ToSlash(strings.TrimPrefix(filepath.Dir(p), root+string(filepath.Separator)))
		byDir[dir] = append(byDir[dir], f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(byDir) == 0 {
		t.Fatalf("no shipped Go packages found under %s — this guard is inert", root)
	}

	out := make([]parsedPkg, 0, len(byDir))
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		out = append(out, parsedPkg{name: pkgLabel(dir), files: byDir[dir]})
	}
	return out, fset
}

// pkgLabel shortens a directory to the label used in reports and in
// knownHostComparisons: the last path element, or the last two when the parent
// carries the meaning (providers/cluster, forge/github).
func pkgLabel(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + " " + parts[len(parts)-1]
	}
	return dir
}

// parseSource is internalPackages over an in-memory file, for the mutation test.
func parseSource(t *testing.T, fset *token.FileSet, src string) []*ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	return []*ast.File{f}
}
