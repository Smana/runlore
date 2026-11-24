// SPDX-License-Identifier: Apache-2.0

package kbui

import (
	"strings"
	"testing"
)

func TestRenderMarkdownXSSNeutralized(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		forbidden []string
		wantSub   []string
	}{
		{
			name:      "raw script tag",
			input:     `<script>alert(1)</script>`,
			forbidden: []string{"<script", "</script>"},
			wantSub:   []string{"&lt;script&gt;alert(1)&lt;/script&gt;"},
		},
		{
			name:      "img onerror",
			input:     `<img src=x onerror=alert(1)>`,
			forbidden: []string{"<img"},
			wantSub:   []string{"&lt;img src=x onerror=alert(1)&gt;"},
		},
		{
			name:      "attribute breakout svg onload",
			input:     `"><svg onload=alert(1)>`,
			forbidden: []string{"<svg"},
			wantSub:   []string{"&#34;&gt;&lt;svg onload=alert(1)&gt;"},
		},
		{
			name:      "javascript link scheme",
			input:     `Click [evil](javascript:alert(1)) now.`,
			forbidden: []string{"<a ", "href="},
			wantSub:   []string{"[evil](javascript:alert(1))"},
		},
		{
			name:      "data url link scheme",
			input:     `Click [evil](data:text/html,<script>alert(1)</script>) now.`,
			forbidden: []string{"<a ", "href=", "<script"},
			wantSub:   []string{"[evil](data:text/html,&lt;script&gt;alert(1)&lt;/script&gt;)"},
		},
		{
			name:      "http non-https link scheme rejected",
			input:     `See [insecure](http://example.com/a).`,
			forbidden: []string{"<a ", "href="},
			wantSub:   []string{"[insecure](http://example.com/a)"},
		},
		{
			name:      "script inside fenced code block",
			input:     "```html\n<script>alert(1)</script>\n```",
			forbidden: []string{"<script"},
			wantSub:   []string{"<pre><code>", "&lt;script&gt;alert(1)&lt;/script&gt;", "</code></pre>"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(RenderMarkdown(tc.input))
			for _, bad := range tc.forbidden {
				if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
					t.Errorf("rendered HTML contains forbidden %q:\n%s", bad, got)
				}
			}
			for _, want := range tc.wantSub {
				if !strings.Contains(got, want) {
					t.Errorf("rendered HTML missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestRenderMarkdownFormatting(t *testing.T) {
	md := strings.Join([]string{
		"## Resolution",
		"",
		"See [safe](https://github.com/org/repo/commit/abc) and **restart** `deployment/web`.",
		"",
		"### Steps to Verify",
		"",
		"- Check `kubectl get pods`",
		"- Confirm **ready** replicas",
		"",
		"1. Run rollout status",
		"2. Check logs",
		"",
		"```bash",
		"kubectl rollout undo deploy/web",
		"```",
	}, "\n")

	got := string(RenderMarkdown(md))
	wants := []string{
		`<h2 id="resolution">Resolution</h2>`,
		`<a href="https://github.com/org/repo/commit/abc" rel="noopener noreferrer">safe</a>`,
		`<strong>restart</strong>`,
		`<code>deployment/web</code>`,
		`<h3 id="steps-to-verify">Steps to Verify</h3>`,
		`<ul>`,
		`<li>Check <code>kubectl get pods</code></li>`,
		`<li>Confirm <strong>ready</strong> replicas</li>`,
		`</ul>`,
		`<ol>`,
		`<li>Run rollout status</li>`,
		`<li>Check logs</li>`,
		`</ol>`,
		`<pre><code>kubectl rollout undo deploy/web`,
		`</code></pre>`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("rendered HTML missing %q:\n%s", w, got)
		}
	}
}
