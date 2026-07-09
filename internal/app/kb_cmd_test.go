// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
)

// writeKBFixture materializes a small OKF catalog: two entries whose lexical
// overlap with the test queries is deliberately asymmetric.
func writeKBFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ts := time.Now().Add(-12 * 24 * time.Hour).UTC().Format(time.RFC3339)
	entries := map[string]string{
		"incidents/crashloop-web.md": "---\ntype: Incident\ntitle: CrashLoop web ConfigMap truncated\ndescription: web pods crashloop after kustomize bump\nresource: apps/web\ntags: [runlore, incident]\ntimestamp: \"" + ts + "\"\n---\n## Cause\n\nConfigMap truncated\n\n## Resolution\n\nrevert the patch\n",
		"incidents/oom-worker.md":    "---\ntype: Incident\ntitle: OOM worker limits too low\ndescription: worker OOMKilled under load\nresource: apps/worker\n---\n## Cause\n\nlimits too low\n",
	}
	for path, content := range entries {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestKBSearchTable(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	err := runKBSearch([]string{"--dir", dir, "crashloop", "web", "configmap"}, &out)
	if err != nil {
		t.Fatalf("runKBSearch: %v", err)
	}
	got := out.String()
	for _, want := range []string{"SCORE", "ENTRY", "TITLE", "RESOURCE", "LAST SEEN",
		"incidents/crashloop-web.md", "apps/web", "12d ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
	// The lexically-distant entry may or may not appear, but the best match must
	// be the FIRST data row.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], "crashloop-web") {
		t.Errorf("best hit is not the first row:\n%s", got)
	}
	// No RESOLVE column without --ledger.
	if strings.Contains(got, "RESOLVE") {
		t.Errorf("RESOLVE column must be absent without --ledger:\n%s", got)
	}
}

func TestKBSearchNoResultsIsError(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "zzz-nothing-matches-this"}, &out); err == nil {
		t.Fatal("want error on zero hits (non-zero exit for scripting)")
	}
}

func TestKBSearchUsageErrors(t *testing.T) {
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", t.TempDir()}, &out); err == nil {
		t.Fatal("want usage error when the query is empty")
	}
	if err := RunKB([]string{"frobnicate"}); err == nil {
		t.Fatal("want usage error on unknown subcommand")
	}
	if err := RunKB(nil); err == nil {
		t.Fatal("want usage error with no subcommand")
	}
}

func TestKBShow(t *testing.T) {
	dir := writeKBFixture(t)
	cases := []struct{ label, arg string }{
		{"exact path", "incidents/crashloop-web.md"},
		{"filename slug", "crashloop-web"},
		{"search fallback unique", "configmap truncated kustomize"},
	}
	for _, c := range cases {
		var out strings.Builder
		if err := runKBShow([]string{"--dir", dir, c.arg}, &out); err != nil {
			t.Fatalf("%s: runKBShow: %v", c.label, err)
		}
		got := out.String()
		for _, want := range []string{"CrashLoop web ConfigMap truncated", "type: Incident",
			"resource: apps/web", "## Cause", "revert the patch"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: output missing %q:\n%s", c.label, want, got)
			}
		}
	}
}

func TestKBShowNoMatchIsError(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBShow([]string{"--dir", dir, "zzz-nothing"}, &out); err == nil {
		t.Fatal("want error when nothing matches")
	}
	if err := runKBShow([]string{"--dir", dir}, &out); err == nil {
		t.Fatal("want usage error when the entry argument is missing")
	}
}

func TestRelAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ts   string
		want string
	}{
		{now.Add(-30 * time.Minute).Format(time.RFC3339), "30m ago"},
		{now.Add(-5 * time.Hour).Format(time.RFC3339), "5h ago"},
		{now.Add(-26 * time.Hour).Format(time.RFC3339), "1d ago"},
		{"", ""},           // hand-written entry without a timestamp
		{"not-a-date", ""}, // malformed
		{now.Add(time.Hour).Format(time.RFC3339), ""}, // future: clock skew, say nothing
	}
	for _, c := range cases {
		if got := relAge(c.ts); got != c.want {
			t.Errorf("relAge(%q) = %q, want %q", c.ts, got, c.want)
		}
	}
}

func TestKBSearchLedgerResolveColumn(t *testing.T) {
	dir := writeKBFixture(t)
	// Build a real ledger: the crashloop entry was recalled twice, resolved once.
	ledgerPath := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := outcome.New(ledgerPath)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	rv := true
	for _, fp := range []string{"a", "b"} {
		if err := l.Open(outcome.Event{
			Fingerprint: fp, Kind: "recall", Entry: "incidents/crashloop-web.md",
			Resolvable: &rv, At: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("seed open: %v", err)
		}
	}
	if _, _, err := l.Resolve("a", time.Now()); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--ledger", ledgerPath, "crashloop", "web"}, &out); err != nil {
		t.Fatalf("runKBSearch: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "RESOLVE") {
		t.Errorf("RESOLVE header missing:\n%s", got)
	}
	if !strings.Contains(got, "1/2") {
		t.Errorf("resolve-rate 1/2 missing:\n%s", got)
	}
}

func TestKBSearchJSON(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--json", "crashloop", "web"}, &out); err != nil {
		t.Fatalf("runKBSearch --json: %v", err)
	}
	var hits []struct {
		Path     string  `json:"path"`
		Type     string  `json:"type"`
		Title    string  `json:"title"`
		Resource string  `json:"resource"`
		Score    float64 `json:"score"`
		LastSeen string  `json:"last_seen"`
	}
	if err := json.Unmarshal([]byte(out.String()), &hits); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out.String())
	}
	if len(hits) == 0 || hits[0].Path != "incidents/crashloop-web.md" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if hits[0].Score <= 0 || hits[0].Type != "Incident" || hits[0].Resource != "apps/web" {
		t.Errorf("hit fields not mapped: %+v", hits[0])
	}
}

// The usage strings promise query-first invocations; stdlib flag alone would
// silently swallow trailing flags into the query.
func TestKBSearchFlagsAfterQuery(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBSearch([]string{"crashloop", "web", "--dir", dir}, &out); err != nil {
		t.Fatalf("flags after query must parse: %v", err)
	}
	if !strings.Contains(out.String(), "incidents/crashloop-web.md") {
		t.Errorf("expected hit missing:\n%s", out.String())
	}
}

func TestKBShowFlagsAfterArg(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBShow([]string{"crashloop-web", "--dir", dir}, &out); err != nil {
		t.Fatalf("flags after arg must parse: %v", err)
	}
	if !strings.Contains(out.String(), "CrashLoop web ConfigMap truncated") {
		t.Errorf("expected entry missing:\n%s", out.String())
	}
}

func TestKBSearchLedgerMissingWarnsAndOmits(t *testing.T) {
	dir := writeKBFixture(t)
	missing := filepath.Join(t.TempDir(), "nope.jsonl")

	// The warning itself is exercised directly against ledgerCounts's warn
	// writer — the seam that keeps it off stdout/the JSON stream.
	var warn strings.Builder
	if counts := ledgerCounts(missing, &warn); counts != nil {
		t.Errorf("missing ledger must yield nil counts, got %+v", counts)
	}
	if !strings.Contains(warn.String(), "warning: ledger") {
		t.Errorf("missing warning line:\n%s", warn.String())
	}
	// Stat-before-open: the warning path must not have created the file.
	if _, err := os.Stat(missing); err == nil {
		t.Error("--ledger must never create the ledger file")
	}

	// Through the search path, stdout (w) carries ONLY the table — no warning
	// text mixed in; diagnostics go to stderr instead.
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--ledger", missing, "crashloop"}, &out); err != nil {
		t.Fatalf("a missing ledger must not fail the search: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "warning:") {
		t.Errorf("stdout must not contain the ledger warning:\n%s", got)
	}
	if !strings.Contains(got, "SCORE") {
		t.Errorf("stdout must still contain the results table:\n%s", got)
	}

	// --json must be a clean stream: nothing before the opening bracket.
	var jsonOut strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--json", "--ledger", missing, "crashloop"}, &jsonOut); err != nil {
		t.Fatalf("--json with a missing ledger must not fail: %v", err)
	}
	jsonGot := jsonOut.String()
	if trimmed := strings.TrimLeft(jsonGot, " \t\n"); !strings.HasPrefix(trimmed, "[") {
		t.Errorf("--json output must start with '[' (no warning text mixed in):\n%s", jsonGot)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(jsonGot), &hits); err != nil {
		t.Fatalf("output is not a clean JSON array: %v\n%s", err, jsonGot)
	}
}

// The usage strings promise both orders around `--`; a literal `--` must stop
// flag parsing for good, so a query token that looks like a flag (e.g. "-k")
// stays a literal search term instead of colliding with -k/--json/etc.
func TestKBSearchDashDashTerminator(t *testing.T) {
	dir := writeKBFixture(t)

	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--", "crashloop"}, &out); err != nil {
		t.Fatalf("post-`--` query must parse: %v", err)
	}
	if !strings.Contains(out.String(), "incidents/crashloop-web.md") {
		t.Errorf("expected hit missing:\n%s", out.String())
	}

	var out2 strings.Builder
	err := runKBSearch([]string{"--dir", dir, "--", "-k"}, &out2)
	if err == nil {
		t.Fatal("want the no-match error for literal query \"-k\", not a flag parse")
	}
	if !strings.Contains(err.Error(), `no entries match "-k"`) {
		t.Errorf("want the literal no-match error, got: %v", err)
	}
}

// findEntry must not silently guess between two entries sharing a basename —
// runKBShow should surface both candidates instead of picking the first one.
func TestKBShowAmbiguousBasename(t *testing.T) {
	dir := t.TempDir()
	entries := map[string]string{
		"incidents/foo.md": "---\ntype: Incident\ntitle: Foo from incidents\n---\nbody\n",
		"runbooks/foo.md":  "---\ntype: Runbook\ntitle: Foo from runbooks\n---\nbody\n",
	}
	for path, content := range entries {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	err := runKBShow([]string{"--dir", dir, "foo"}, &out)
	if err == nil {
		t.Fatal("want a disambiguation error for a duplicate basename")
	}
	for _, want := range []string{"incidents/foo.md", "runbooks/foo.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("candidates must list %q:\n%v", want, err)
		}
	}
}
