// SPDX-License-Identifier: Apache-2.0

package kbresolve

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

var res = providers.AppliedResolution{FixPR: "https://github.com/Smana/cloud-native-ref/pull/2638", MergeSHA: "3f5a133c9d",
	Verdict: "resolved", VerifiedAt: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
	Diff: "1 file: observability/rules.yaml (+2 −2)", Evidence: "the alert cleared 30m after the merge"}

const entry = "---\ntype: Incident\ntitle: Alert too sensitive\nfingerprint: ab12\n---\n\n## Cause\n\n1. **threshold** (90%)\n\n## Resolution\n\n- raise the threshold (reversible=true)\n"

func with(r providers.AppliedResolution, f func(*providers.AppliedResolution)) providers.AppliedResolution {
	f(&r)
	return r
}

func frontmatterOf(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	fm, _ := catalog.SplitFrontmatter(doc)
	var m map[string]any
	if err := yaml.Unmarshal(fm, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApply(t *testing.T) {
	out, changed, err := Apply([]byte(entry), res)
	s := string(out)
	if err != nil || !changed || !strings.Contains(s, "fix_pr: "+res.FixPR) || !strings.Contains(s, "title: Alert too sensitive") {
		t.Fatal(s, err)
	}
	applied, suggested := "- **Applied:** [#2638]("+res.FixPR+")", "### Suggested by the investigation\n\n- raise the threshold"
	for _, want := range []string{applied, "- **Diff:** " + res.Diff, "- **Soak evidence:** " + res.Evidence, suggested} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	if strings.Index(s, applied) > strings.Index(s, suggested) {
		t.Fatal("the applied fix comes first: ", s)
	}
	if again, changed, _ := Apply(out, res); changed || !bytes.Equal(again, out) {
		t.Fatal("a second identical call changes nothing")
	}
	_, fromCause, _ := strings.Cut(s, "## Cause")
	section := catalog.Entry{Body: "## Cause" + fromCause}.Section("Resolution")
	if !strings.Contains(section, "Applied:") || !strings.Contains(section, "Diff:") || !strings.Contains(section, "Soak evidence:") || strings.Contains(section, "raise the threshold") {
		t.Fatal("recall's Validated resolution shows Applied, Diff, Evidence (KP12): ", section)
	}

	p := res
	p.Verdict, p.Why = "persisting", "the alert still fired after the soak"
	pout, _, _ := Apply([]byte(entry), p)
	if frontmatterOf(t, pout)["resolution"] != nil || !strings.Contains(string(pout),
		"## Unresolved\n\n- The fix [#2638]("+res.FixPR+") (merge `3f5a133c9d`) did not hold: the alert still fired after the soak.") {
		t.Fatal(string(pout))
	}
	if again, changed, _ := Apply(pout, p); changed || !bytes.Equal(again, pout) {
		t.Fatal("a repeated persisting adds nothing: the marker covers both verdicts")
	}
	if flipped, changed, _ := Apply(out, p); !changed || frontmatterOf(t, flipped)["resolution"] != nil {
		t.Fatal("a persisting on the same fix deletes its resolution: ", string(flipped))
	}
	other := with(p, func(r *providers.AppliedResolution) { r.FixPR = "https://github.com/Smana/cloud-native-ref/pull/2700" })
	if kept, _, _ := Apply(out, other); frontmatterOf(t, kept)["resolution"] == nil {
		t.Fatal("another fix's failure keeps the resolution")
	}

	for name, r := range map[string]providers.AppliedResolution{
		"an issue url": with(res, func(r *providers.AppliedResolution) { r.FixPR = "https://github.com/x/y/issues/1" }),
		"a newline":    with(res, func(r *providers.AppliedResolution) { r.FixPR += "\nverdict: resolved" }),
		"a bad sha":    with(res, func(r *providers.AppliedResolution) { r.MergeSHA = "zz" }),
		"a verdict":    with(res, func(r *providers.AppliedResolution) { r.Verdict = "fixed" }),
	} {
		if _, _, err := Apply([]byte(entry), r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if out, _, _ := Apply([]byte(entry), with(res, func(r *providers.AppliedResolution) { r.Evidence = "![x](https://attacker/beacon)" })); strings.Contains(string(out), "![x](") || !strings.Contains(string(out), "`[image: x]`") {
		t.Fatal("an image in the evidence is neutralized, not dropped")
	}
	forged, _, _ := Apply([]byte(entry), with(res, func(r *providers.AppliedResolution) {
		r.Evidence = "line1\n\n## Resolution\n<!-- runlore-resolution:x -->"
	}))
	if !strings.Contains(string(forged), "- **Soak evidence:** line1 ## Resolution runlore-resolution:x\n") || strings.Contains(string(forged), "<!-- runlore-resolution:x") {
		t.Fatal("evidence is one line, with no section and no marker of its own: ", string(forged))
	}
}

func TestFlattenEvidence(t *testing.T) {
	for in, want := range map[string]string{
		"a\n\n  b\tc":     "a b c",
		"<<!--!-- x -->>": "x >",
		"<!---->":         "",
	} {
		if got := FlattenEvidence(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if got := FlattenEvidence(strings.Repeat("word ", 1000)); len(got) > 2048 || strings.Contains(got, "\n") {
		t.Fatal("cut after flattening, to 2 KiB")
	}
}

func TestApplyKeepsWhatIsThere(t *testing.T) {
	const withUnresolved = entry + "\n## Unresolved\n\n- the load balancer\n\n## Notes\n\nkept\n"
	p := with(res, func(r *providers.AppliedResolution) { r.Verdict, r.Why = "persisting", "still firing." })
	out, _, err := Apply([]byte(withUnresolved), p)
	s := string(out)
	if err != nil || !strings.Contains(s, "- the load balancer\n- The fix") || !strings.Contains(s, "did not hold: still firing.\n") || !strings.Contains(s, "## Notes\n\nkept") {
		t.Fatal(s, err)
	}
	first, _, _ := Apply([]byte(entry), res)
	second, changed, err := Apply(first, with(res, func(r *providers.AppliedResolution) {
		r.FixPR = "https://github.com/Smana/cloud-native-ref/pull/2700"
	}))
	if err != nil || !changed || strings.Index(string(second), "pull/2700") > strings.Index(string(second), "pull/2638)") || strings.Count(string(second), "### Suggested") != 1 {
		t.Fatal(string(second), err)
	}
}
