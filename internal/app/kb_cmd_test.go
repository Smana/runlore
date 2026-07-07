package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
