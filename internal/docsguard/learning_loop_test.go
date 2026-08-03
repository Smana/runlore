// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

const learningLoopPath = "../../website/content/docs/concepts/learning-loop.md"

// The learning-loop page's §6 states, in one direction or the other, whether a
// RunLore-drafted entry carries `last_validated`. It matters because the whole
// freshness story hangs off it: if drafts stamped the field, the revalidation
// pass would have nothing to earn and the age gate would key on a date no human
// ever confirmed. The page ALREADY shipped the wrong claim once ("stamped at
// entry creation (= timestamp)") while the drafter left it unset, so both
// possible claims are phrase-anchored here and the truth is read out of the
// forge client itself.
var (
	unsetClaimRE   = regexp.MustCompile("`last_validated` is unset when RunLore drafts an entry")
	stampedClaimRE = regexp.MustCompile("`last_validated` is stamped at entry creation")
)

// TestLearningLoopLastValidatedClaimMatchesTheDrafter pins §6's claim to what the
// forge client actually writes. The truth is DERIVED, not restated: a real
// OpenPR is driven against a stub GitHub API and the drafted entry's frontmatter
// is inspected, so making renderEntry stamp the field flips the required doc
// phrase and fails this test until the page follows.
func TestLearningLoopLastValidatedClaimMatchesTheDrafter(t *testing.T) {
	stamped := strings.Contains(draftedEntry(t), "last_validated:")

	doc := readDoc(t, learningLoopPath)
	claimsUnset := unsetClaimRE.MatchString(doc)
	claimsStamped := stampedClaimRE.MatchString(doc)

	switch {
	case !claimsUnset && !claimsStamped:
		t.Fatalf("learning-loop.md §6 states no claim about last_validated on RunLore drafts; "+
			"it must say one of %q or %q", unsetClaimRE, stampedClaimRE)
	case claimsUnset && claimsStamped:
		t.Fatal("learning-loop.md claims both that drafts leave last_validated unset and that they stamp it — fix the page")
	case stamped && claimsUnset:
		t.Error("the forge now stamps last_validated on every draft, but learning-loop.md §6 says it is unset — " +
			"update §6 (and reconsider the revalidation pass, which exists to earn that field)")
	case !stamped && claimsStamped:
		t.Error("the forge leaves last_validated unset on drafts, but learning-loop.md §6 says it is stamped at creation — update §6")
	}
}

// TestLastValidatedClaimREsFlip is the mutation test for the two matchers above:
// each must fire on its own claim, stay quiet on the other, and neither may be
// satisfied by the page's other prose about the field (the §5 revalidation
// paragraph and the age-gate bullet both name it).
func TestLastValidatedClaimREsFlip(t *testing.T) {
	cases := []struct {
		name, text           string
		wantUnset, wantStamp bool
	}{
		{"unset claim", "**`last_validated` is unset when RunLore drafts an entry** — the field claims", true, false},
		{"stamped claim", "`last_validated` is stamped at entry creation (= `timestamp`)", false, true},
		{"age-gate bullet is neither", "an entry whose `last_validated` (else `timestamp`) predates the horizon", false, false},
		{"revalidation paragraph is neither", "proposing to stamp `last_validated` with that resolve date", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unsetClaimRE.MatchString(c.text); got != c.wantUnset {
				t.Errorf("unsetClaimRE.MatchString(%q) = %v, want %v", c.text, got, c.wantUnset)
			}
			if got := stampedClaimRE.MatchString(c.text); got != c.wantStamp {
				t.Errorf("stampedClaimRE.MatchString(%q) = %v, want %v", c.text, got, c.wantStamp)
			}
		})
	}
}

// curateBlocks maps the config-key prefixes the learning-loop page cites to the
// struct that actually defines them, so a renamed YAML tag fails here rather than
// silently leaving a reader configuring a key that no longer exists.
// curateBlocks maps each `curate.<block>.` prefix to the struct that defines it,
// DERIVED from config.Curate's own yaml tags rather than hand-listed. A
// hand-listed map only ever covers the blocks someone remembered: the page also
// cites curate.sweeps.* keys, which an earlier alternation of just
// (retirement|revalidation) skipped while the "did I check anything?" backstop
// stayed green — so the guard read as full coverage and was a subset.
func curateBlocks() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	rt := reflect.TypeOf(config.Curate{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Struct {
			continue // a scalar knob like stale_after, not a block of keys
		}
		if tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); tag != "" && tag != "-" {
			out["curate."+tag+"."] = f.Type
		}
	}
	return out
}

var curateKeyRE = regexp.MustCompile(`curate\.([a-z_]+)\.([a-z_]+)`)

// TestLearningLoopCitesRealCurateKeys checks every `curate.<block>.<key>` the page
// names against the yaml tags of the struct that defines the block, in both
// directions of trust: the block and the key must exist, and the guard must have
// found something to check at all.
func TestLearningLoopCitesRealCurateKeys(t *testing.T) {
	doc := readDoc(t, learningLoopPath)
	blocks := curateBlocks()
	var checked int
	for _, m := range curateKeyRE.FindAllStringSubmatch(doc, -1) {
		block, key := m[1], m[2]
		checked++
		rt, ok := blocks["curate."+block+"."]
		if !ok {
			t.Errorf("learning-loop.md cites curate.%s.%s, but config.Curate declares no %q block — "+
				"fix the page or the config struct", block, key, block)
			continue
		}
		if !yamlTags(rt)[key] {
			t.Errorf("learning-loop.md cites curate.%s.%s, which is not a yaml key of %s — "+
				"fix the page or the config struct", block, key, rt)
		}
	}
	if checked == 0 {
		t.Fatal("no curate.<block>.<key> citations found in learning-loop.md — this guard is now inert")
	}
}

// TestCurateBlockDerivationIsComplete is the mutation guard for the derivation
// above. It must find EVERY struct block on config.Curate — the whole point of
// deriving it — and yamlTags must accept only keys a block really declares.
func TestCurateBlockDerivationIsComplete(t *testing.T) {
	blocks := curateBlocks()
	for _, want := range []string{"curate.retirement.", "curate.revalidation.", "curate.sweeps."} {
		if _, ok := blocks[want]; !ok {
			t.Errorf("derivation missed %s — keys under it would go unchecked", want)
		}
	}
	rt := reflect.TypeOf(config.Curate{})
	var wantBlocks int
	for i := range rt.NumField() {
		if rt.Field(i).Type.Kind() == reflect.Struct {
			wantBlocks++
		}
	}
	if len(blocks) != wantBlocks {
		t.Errorf("derived %d blocks from %d struct fields on config.Curate — a block is being dropped",
			len(blocks), wantBlocks)
	}
	// Both directions on the tag lookup, so a yamlTags that answered "yes" (or
	// "no") to everything cannot pass.
	sweeps := yamlTags(blocks["curate.sweeps."])
	if !sweeps["mode"] || !sweeps["interval"] {
		t.Error("yamlTags missed a key config.Sweeps declares")
	}
	if sweeps["schedule"] || sweeps["Mode"] {
		t.Error("yamlTags accepted a key config.Sweeps does not declare")
	}
}

// TestLearningLoopStatesRevalidationDefaults pins §5's two quoted anti-spam knobs
// to reality on BOTH axes: the key NAME comes from the struct's yaml tag and the
// VALUE from ApplyDefaults, so a rename and a retuning each fail here. They are
// the numbers an operator budgets review time against, so a silent change to
// either is exactly the drift this package exists to catch.
func TestLearningLoopStatesRevalidationDefaults(t *testing.T) {
	var c config.Config
	c.Curate.Revalidation.Enabled = true
	config.ApplyDefaults(&c)
	r := c.Curate.Revalidation

	doc := readDoc(t, learningLoopPath)
	for _, want := range []string{
		fmt.Sprintf("`%s` (default **%s**)", tagOf(r, "MinInterval"), shortDuration(r.MinInterval.Std())),
		fmt.Sprintf("`%s` (default **%d**)", tagOf(r, "MaxOpen"), r.MaxOpen),
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("learning-loop.md §5 must state the shipped knob verbatim as %q — "+
				"the key or its default changed underneath the page", want)
		}
	}
}

// tagOf returns the yaml key one struct field is serialized under, failing loudly
// if the field is gone (a rename must not silently make the guard check nothing).
func tagOf(v any, field string) string {
	f, ok := reflect.TypeOf(v).FieldByName(field)
	if !ok {
		panic("docsguard: no field " + field + " on " + reflect.TypeOf(v).String())
	}
	tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return tag
}

// shortDuration renders a duration the way an operator writes it in YAML and the
// way the docs quote it ("720h"), dropping the zero minute/second tail that
// time.Duration.String() always appends. Only the whole-hours shape is folded —
// a sub-hour default keeps its exact string rather than being mangled.
func shortDuration(d time.Duration) string {
	if s := d.String(); strings.HasSuffix(s, "h0m0s") {
		return strings.TrimSuffix(s, "0m0s")
	}
	return d.String()
}

// yamlTags returns the set of yaml key names declared by a struct type's fields.
func yamlTags(rt reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := range rt.NumField() {
		tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("yaml"), ",")
		if tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	return out
}

// readDoc reads a documentation page, failing the test if it is missing.
func readDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo doc path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// draftedEntry drives the REAL github.Client.OpenPR against a stub GitHub API and
// returns the OKF markdown it wrote for the entry — the drafter's actual output,
// which is the only honest source for what a fresh draft's frontmatter contains.
// Only the FIRST contents PUT is captured: that is the entry itself, before the
// best-effort bundle maintenance writes index.md/log.md.
func draftedEntry(t *testing.T) string {
	t.Helper()
	var entry string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
	})
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if entry == "" {
			raw, _ := base64.StdEncoding.DecodeString(body["content"].(string))
			entry = string(raw)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"html_url":"https://forge/pr/1","number":1}`))
	})
	mux.HandleFunc("POST /repos/o/r/issues/1/labels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := github.New(srv.URL, "o", "r", "main", func(context.Context) (string, error) { return "tok", nil })
	if _, err := c.OpenPR(context.Background(), providers.KBEntry{
		Type: "Incident", Title: "Drift guard", Description: "d", Body: "## Symptom\nx",
	}); err != nil {
		t.Fatalf("OpenPR against the stub forge: %v", err)
	}
	if entry == "" {
		// Guard the guard: no captured PUT means OpenPR changed shape and this
		// test would otherwise "pass" by inspecting an empty string.
		t.Fatal("OpenPR wrote no entry file — this guard is now inert")
	}
	return entry
}
