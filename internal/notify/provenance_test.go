// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func marshalNoEscape(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.String()
}

func TestSlackProvenanceAndChangeURL(t *testing.T) {
	t.Run("main card hyperlinks What changed in place without adding blocks", func(t *testing.T) {
		base := providers.Investigation{
			Title:      "Harbor crashloop",
			Verdict:    providers.VerdictActionRequired,
			Confidence: 0.9,
			RootCauses: []providers.Hypothesis{{
				Summary:   "tag bump broke startup",
				ChangeRef: "aaa1111..bbb2222",
				Evidence:  []string{"pod CrashLoopBackOff"},
			}},
		}
		withURL := base
		withURL.RootCauses = []providers.Hypothesis{{
			Summary:   "tag bump broke startup",
			ChangeRef: "aaa1111..bbb2222",
			ChangeURL: "https://github.com/org/infra/compare/aaa1111...bbb2222",
			Evidence:  []string{"pod CrashLoopBackOff"},
		}}

		baseBlocks := summaryBlocks(base)
		urlBlocks := summaryBlocks(withURL)
		if len(urlBlocks) != len(baseBlocks) {
			t.Fatalf("summaryBlocks count changed: got %d, want %d (must not add blocks)", len(urlBlocks), len(baseBlocks))
		}
		s := marshalNoEscape(urlBlocks)
		want := `<https://github.com/org/infra/compare/aaa1111...bbb2222|aaa1111..bbb2222>`
		if !strings.Contains(s, want) {
			t.Errorf("summaryBlocks missing clickable ChangeURL %q in: %s", want, s)
		}
	})

	t.Run("mrkdwn special chars in label are escaped and unsafe ChangeURL degrades", func(t *testing.T) {
		inv := providers.Investigation{
			Title:      "Injection test",
			Confidence: 0.8,
			RootCauses: []providers.Hypothesis{
				{
					Summary:   "cause 1",
					ChangeRef: "ref|with><chars",
					ChangeURL: "https://github.com/org/infra/commit/abc1234",
				},
				{
					Summary:   "cause 2",
					ChangeRef: "unsafe-url-ref",
					ChangeURL: "javascript:alert(1)",
				},
			},
		}
		summaryJSON := marshalNoEscape(summaryBlocks(inv))
		if !strings.Contains(summaryJSON, `<https://github.com/org/infra/commit/abc1234|ref&#124;with&gt;&lt;chars>`) {
			t.Errorf("summary did not escape pipe/angle brackets in link label: %s", summaryJSON)
		}
		detailJSON := marshalNoEscape(detailBlocks(inv))
		if strings.Contains(detailJSON, "javascript:alert") {
			t.Errorf("detail rendered unsafe non-HTTPS URL: %s", detailJSON)
		}
		if !strings.Contains(detailJSON, "`unsafe-url-ref`") {
			t.Errorf("detail did not degrade unsafe URL to plain code span: %s", detailJSON)
		}
	})

	t.Run("detail blocks render up to 5 provenance file links", func(t *testing.T) {
		inv := providers.Investigation{
			Title:      "Harbor crashloop",
			Confidence: 0.9,
			RootCauses: []providers.Hypothesis{{
				Summary:   "tag bump",
				ChangeRef: "aaa1111..bbb2222",
				ChangeURL: "https://github.com/org/infra/compare/aaa1111...bbb2222",
				Evidence:  []string{"e1"},
				Provenance: []providers.ProvenanceLink{
					{Label: "f1.yaml#L10", URL: "https://github.com/org/infra/blob/bbb2222/f1.yaml#L10", Kind: "file"},
					{Label: "f2.yaml#L20", URL: "https://github.com/org/infra/blob/bbb2222/f2.yaml#L20", Kind: "file"},
					{Label: "f3.yaml#L30", URL: "https://github.com/org/infra/blob/bbb2222/f3.yaml#L30", Kind: "file"},
					{Label: "f4.yaml#L40", URL: "https://github.com/org/infra/blob/bbb2222/f4.yaml#L40", Kind: "file"},
					{Label: "f5.yaml#L50", URL: "https://github.com/org/infra/blob/bbb2222/f5.yaml#L50", Kind: "file"},
					{Label: "f6.yaml#L60", URL: "https://github.com/org/infra/blob/bbb2222/f6.yaml#L60", Kind: "file"},
				},
			}},
		}
		detail := detailBlocks(inv)
		if len(detail) == 0 {
			t.Fatal("expected detailBlocks to render when Provenance links are present")
		}
		s := marshalNoEscape(detail)
		if !strings.Contains(s, `<https://github.com/org/infra/blob/bbb2222/f1.yaml#L10|f1.yaml#L10>`) {
			t.Errorf("detail missing first provenance link: %s", s)
		}
		if !strings.Contains(s, `<https://github.com/org/infra/blob/bbb2222/f5.yaml#L50|f5.yaml#L50>`) {
			t.Errorf("detail missing 5th provenance link: %s", s)
		}
		if strings.Contains(s, "f6.yaml#L60") {
			t.Errorf("detail exceeded 5-link cap: %s", s)
		}
	})

	t.Run("MatchedKnowledge appends #resolution anchor when SectionAnchor is set", func(t *testing.T) {
		inv := providers.Investigation{
			Title:      "Known issue",
			Confidence: 0.85,
			MatchedKnowledge: &providers.MatchedEntry{
				Path:          "entries/harbor.md",
				Title:         "Harbor runbook",
				URL:           "https://github.com/org/kb/blob/main/entries/harbor.md",
				SectionAnchor: "#resolution",
				Score:         6.0,
			},
		}
		s := marshalNoEscape(summaryBlocks(inv))
		wantTitleLink := `<https://github.com/org/kb/blob/main/entries/harbor.md#resolution|Harbor runbook>`
		wantFooterLink := `<https://github.com/org/kb/blob/main/entries/harbor.md#resolution|view entry>`
		if !strings.Contains(s, wantTitleLink) {
			t.Errorf("summaryBlocks missing hyperlinked MatchedKnowledge title %q: %s", wantTitleLink, s)
		}
		if !strings.Contains(s, wantFooterLink) {
			t.Errorf("summaryBlocks missing #resolution footer entryLink %q: %s", wantFooterLink, s)
		}
	})
}

func TestFormatProvenanceAndChangeURL(t *testing.T) {
	inv := providers.Investigation{
		Title:      "Harbor crashloop",
		Confidence: 0.9,
		MatchedKnowledge: &providers.MatchedEntry{
			Path:          "entries/harbor.md",
			Title:         "Harbor runbook",
			URL:           "https://github.com/org/kb/blob/main/entries/harbor.md",
			SectionAnchor: "#resolution",
			Score:         6.0,
		},
		RootCauses: []providers.Hypothesis{{
			Summary:   "tag bump broke startup",
			ChangeRef: "aaa1111..bbb2222",
			ChangeURL: "https://github.com/org/infra/compare/aaa1111...bbb2222",
			Provenance: []providers.ProvenanceLink{
				{Label: "apps/harbor/values.yaml#L42", URL: "https://github.com/org/infra/blob/bbb2222/apps/harbor/values.yaml#L42", Kind: "file"},
				{Label: "bad", URL: "http://evil.example/x", Kind: "file"},
			},
			Evidence: []string{"CrashLoopBackOff"},
		}},
	}
	out := Format(inv)
	for _, want := range []string{
		"What changed: aaa1111..bbb2222 (https://github.com/org/infra/compare/aaa1111...bbb2222)",
		"↳ apps/harbor/values.yaml#L42: https://github.com/org/infra/blob/bbb2222/apps/harbor/values.yaml#L42",
		"Matches known runbook: Harbor runbook — https://github.com/org/kb/blob/main/entries/harbor.md#resolution",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Format missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "http://evil.example") {
		t.Errorf("Format emitted non-HTTPS provenance link:\n%s", out)
	}
}

func TestMatrixProvenanceAndChangeURL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"event_id":"$prov"}`))
	}))
	defer srv.Close()

	inv := providers.Investigation{
		Title:      "Harbor crashloop",
		Confidence: 0.9,
		MatchedKnowledge: &providers.MatchedEntry{
			Path:          "entries/harbor.md",
			Title:         "Harbor <runbook>",
			URL:           "https://github.com/org/kb/blob/main/entries/harbor.md",
			SectionAnchor: "#resolution",
			Score:         6.0,
		},
		RootCauses: []providers.Hypothesis{{
			Summary:   "tag bump",
			ChangeRef: "aaa1111..<bbb2222>",
			ChangeURL: "https://github.com/org/infra/compare/aaa1111...bbb2222",
			Provenance: []providers.ProvenanceLink{
				{Label: "apps/harbor/values.yaml#L42", URL: "https://github.com/org/infra/blob/bbb2222/apps/harbor/values.yaml#L42", Kind: "file"},
			},
			Evidence: []string{"untrusted url https://evil.example/phish"},
		}},
	}

	if err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	fb, _ := gotBody["formatted_body"].(string)
	for _, want := range []string{
		`<a href="https://github.com/org/infra/compare/aaa1111...bbb2222">aaa1111..&lt;bbb2222&gt;</a>`,
		`<a href="https://github.com/org/infra/blob/bbb2222/apps/harbor/values.yaml#L42">apps/harbor/values.yaml#L42</a>`,
		`<a href="https://github.com/org/kb/blob/main/entries/harbor.md#resolution">Harbor &lt;runbook&gt;</a>`,
	} {
		if !strings.Contains(fb, want) {
			t.Errorf("Matrix formatted_body missing %q in:\n%s", want, fb)
		}
	}
	if strings.Contains(fb, `<a href="https://evil.example/phish"`) {
		t.Errorf("Matrix auto-linkified untrusted evidence URL:\n%s", fb)
	}
}
