// SPDX-License-Identifier: Apache-2.0

package provenance

import "testing"

func TestNormalizeRepoHTTPS(t *testing.T) {
	customGitLab := DefaultHostStyles("gitlab", "git.corp.example", "https://gitlab.internal.example/subpath")
	customGHE := DefaultHostStyles("github", "ghe.corp.example", "")

	tests := []struct {
		name      string
		rawURL    string
		styles    HostStyles
		wantBase  string
		wantStyle ForgeStyle
		wantOK    bool
	}{
		{
			name:      "github https with .git",
			rawURL:    "https://github.com/org/repo.git",
			styles:    DefaultHostStyles("", "", ""),
			wantBase:  "https://github.com/org/repo",
			wantStyle: StyleGitHub,
			wantOK:    true,
		},
		{
			name:      "github scp ssh",
			rawURL:    "git@github.com:org/repo.git",
			styles:    DefaultHostStyles("", "", ""),
			wantBase:  "https://github.com/org/repo",
			wantStyle: StyleGitHub,
			wantOK:    true,
		},
		{
			name:      "gitlab ssh scheme with subgroups",
			rawURL:    "ssh://git@gitlab.com/group/sub/repo.git",
			styles:    DefaultHostStyles("", "", ""),
			wantBase:  "https://gitlab.com/group/sub/repo",
			wantStyle: StyleGitLab,
			wantOK:    true,
		},
		{
			name:      "gitlab ssh scheme with port stripped",
			rawURL:    "ssh://git@gitlab.com:2222/group/repo.git",
			styles:    DefaultHostStyles("", "", ""),
			wantBase:  "https://gitlab.com/group/repo",
			wantStyle: StyleGitLab,
			wantOK:    true,
		},
		{
			name:      "self-hosted gitlab via git_host",
			rawURL:    "git@git.corp.example:platform/infra.git",
			styles:    customGitLab,
			wantBase:  "https://git.corp.example/platform/infra",
			wantStyle: StyleGitLab,
			wantOK:    true,
		},
		{
			name:      "self-hosted gitlab via gitlab.url",
			rawURL:    "https://gitlab.internal.example/platform/infra",
			styles:    customGitLab,
			wantBase:  "https://gitlab.internal.example/platform/infra",
			wantStyle: StyleGitLab,
			wantOK:    true,
		},
		{
			name:      "self-hosted GHE",
			rawURL:    "https://GHE.Corp.Example/org/repo.git",
			styles:    customGHE,
			wantBase:  "https://ghe.corp.example/org/repo",
			wantStyle: StyleGitHub,
			wantOK:    true,
		},
		{
			name:      "nil styles defaults to public github and gitlab",
			rawURL:    "https://github.com/org/repo",
			styles:    nil,
			wantBase:  "https://github.com/org/repo",
			wantStyle: StyleGitHub,
			wantOK:    true,
		},
		{
			name:   "unregistered host fails closed",
			rawURL: "https://git.unknown.example/a/b",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "cleartext http rejected",
			rawURL: "http://github.com/org/repo",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "file scheme rejected",
			rawURL: "file:///tmp/repo",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "embedded credentials rejected",
			rawURL: "https://tok@github.com/org/repo",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "path traversal rejected",
			rawURL: "https://github.com/org/../evil",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "query string rejected",
			rawURL: "https://github.com/org/repo?foo=bar",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "fragment rejected",
			rawURL: "https://github.com/org/repo#frag",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "whitespace rejected",
			rawURL: "https://github.com/org/repo evil",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "single segment path rejected",
			rawURL: "https://github.com/onlyorg",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
		{
			name:   "empty input rejected",
			rawURL: "",
			styles: DefaultHostStyles("", "", ""),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotBase, gotStyle, gotOK := NormalizeRepoHTTPS(tc.rawURL, tc.styles)
			if gotOK != tc.wantOK || gotBase != tc.wantBase || gotStyle != tc.wantStyle {
				t.Errorf("NormalizeRepoHTTPS(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.rawURL, gotBase, gotStyle, gotOK, tc.wantBase, tc.wantStyle, tc.wantOK)
			}
		})
	}
}

func TestCompareCommitFileURLs(t *testing.T) {
	t.Run("CompareURL", func(t *testing.T) {
		tests := []struct {
			name           string
			base           string
			style          ForgeStyle
			fromRev, toRev string
			want           string
		}{
			{"github", "https://github.com/org/repo", StyleGitHub, "aaa1111", "bbb2222", "https://github.com/org/repo/compare/aaa1111...bbb2222"},
			{"gitlab", "https://gitlab.com/grp/repo", StyleGitLab, "v1.2.0", "v1.3.0", "https://gitlab.com/grp/repo/-/compare/v1.2.0...v1.3.0"},
			{"unsafe rev with space", "https://github.com/org/repo", StyleGitHub, "a b", "bbb2222", ""},
			{"unsafe rev with dots", "https://github.com/org/repo", StyleGitHub, "a..b", "bbb2222", ""},
			{"non-https base", "http://github.com/org/repo", StyleGitHub, "aaa", "bbb", ""},
			{"unknown style", "https://github.com/org/repo", ForgeStyle("other"), "aaa", "bbb", ""},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := CompareURL(tc.base, tc.style, tc.fromRev, tc.toRev); got != tc.want {
					t.Errorf("CompareURL() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("CommitURL", func(t *testing.T) {
		tests := []struct {
			name  string
			base  string
			style ForgeStyle
			sha   string
			want  string
		}{
			{"github", "https://github.com/org/repo", StyleGitHub, "abc1234", "https://github.com/org/repo/commit/abc1234"},
			{"gitlab", "https://gitlab.com/grp/repo", StyleGitLab, "abc1234", "https://gitlab.com/grp/repo/-/commit/abc1234"},
			{"unsafe sha", "https://github.com/org/repo", StyleGitHub, "abc?x=1", ""},
			{"empty sha", "https://github.com/org/repo", StyleGitHub, "", ""},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := CommitURL(tc.base, tc.style, tc.sha); got != tc.want {
					t.Errorf("CommitURL() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("FileURL", func(t *testing.T) {
		tests := []struct {
			name     string
			base     string
			style    ForgeStyle
			rev      string
			filePath string
			line     int
			want     string
		}{
			{
				name:     "github with line",
				base:     "https://github.com/org/repo",
				style:    StyleGitHub,
				rev:      "bbb2222",
				filePath: "apps/harbor/values.yaml",
				line:     42,
				want:     "https://github.com/org/repo/blob/bbb2222/apps/harbor/values.yaml#L42",
			},
			{
				name:     "gitlab with line",
				base:     "https://gitlab.com/grp/repo",
				style:    StyleGitLab,
				rev:      "bbb2222",
				filePath: "apps/harbor/values.yaml",
				line:     42,
				want:     "https://gitlab.com/grp/repo/-/blob/bbb2222/apps/harbor/values.yaml#L42",
			},
			{
				name:     "no line when zero",
				base:     "https://github.com/org/repo",
				style:    StyleGitHub,
				rev:      "bbb2222",
				filePath: "apps/harbor/values.yaml",
				line:     0,
				want:     "https://github.com/org/repo/blob/bbb2222/apps/harbor/values.yaml",
			},
			{
				name:     "encodes spaces hash and parentheses",
				base:     "https://github.com/org/repo",
				style:    StyleGitHub,
				rev:      "bbb2222",
				filePath: "apps/my app/v(1)#2.yaml",
				line:     7,
				want:     "https://github.com/org/repo/blob/bbb2222/apps/my%20app/v%281%29%232.yaml#L7",
			},
			{
				name:     "rejects traversal path",
				base:     "https://github.com/org/repo",
				style:    StyleGitHub,
				rev:      "bbb2222",
				filePath: "apps/../etc/passwd",
				line:     1,
				want:     "",
			},
			{
				name:     "rejects unsafe rev",
				base:     "https://github.com/org/repo",
				style:    StyleGitHub,
				rev:      "bbb|evil",
				filePath: "a.yaml",
				line:     1,
				want:     "",
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := FileURL(tc.base, tc.style, tc.rev, tc.filePath, tc.line); got != tc.want {
					t.Errorf("FileURL() = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

func TestFirstAddedLine(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  int
	}{
		{
			name:  "standard hunk",
			patch: "--- a/values.yaml\n+++ b/values.yaml\n@@ -10,4 +12,6 @@ spec:\n-  tag: v1\n+  tag: v2\n",
			want:  12,
		},
		{
			name:  "single line hunk",
			patch: "@@ -1 +1 @@\n-old\n+new\n",
			want:  1,
		},
		{
			name:  "deleted file",
			patch: "@@ -10,5 +0,0 @@\n-removed\n",
			want:  0,
		},
		{
			name:  "binary diff",
			patch: "Binary files a/img.png and b/img.png differ\n",
			want:  0,
		},
		{
			name:  "empty patch",
			patch: "",
			want:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstAddedLine(tc.patch); got != tc.want {
				t.Errorf("FirstAddedLine() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSafeHTTPS(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://github.com/org/repo/compare/a...b", true},
		{"https://github.com/org/repo/blob/main/a.md#resolution", true},
		{"http://github.com/org/repo", false},
		{"javascript:alert(1)", false},
		{"https://user:pass@github.com/org/repo", false},
		{"https://github.com/org/repo|evil", false},
		{"https://github.com/org/repo<script>", false},
		{"https://github.com/org/repo\nevil", false},
		{"https://github.com/org/repo space", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := SafeHTTPS(tc.url); got != tc.want {
			t.Errorf("SafeHTTPS(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
