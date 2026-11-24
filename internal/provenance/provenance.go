// SPDX-License-Identifier: Apache-2.0

// Package provenance constructs 100% server-derived, validated HTTPS permalinks
// for observed GitOps/source-repo revisions, changed files (with unified-diff
// hunk line anchors), and matched knowledge-base section anchors.
//
// Unknown Git hosts and non-HTTPS or malformed URLs fail closed (returning ""
// or false) so callers degrade cleanly to escaped plain text.
package provenance

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// ForgeStyle selects the URL path conventions used by a Git hosting forge.
type ForgeStyle string

const (
	// StyleGitHub generates GitHub-style URLs (/compare/, /commit/, /blob/).
	StyleGitHub ForgeStyle = "github"
	// StyleGitLab generates GitLab-style URLs (/-/compare/, /-/commit/, /-/blob/).
	StyleGitLab ForgeStyle = "gitlab"
)

// HostStyles maps lowercase ASCII Git hostnames to their ForgeStyle.
// Hosts absent from the map fail closed so unrecognized internal hosts never
// receive fabricated 404 URLs.
type HostStyles map[string]ForgeStyle

// DefaultHostStyles seeds HostStyles with the public GitHub and GitLab hosts
// plus any self-hosted forge host derived from the existing forge configuration
// (forge.provider, forge.git_host, forge.gitlab.url).
func DefaultHostStyles(forgeProvider, gitHost, gitlabURL string) HostStyles {
	styles := HostStyles{
		"github.com": StyleGitHub,
		"gitlab.com": StyleGitLab,
	}
	style := StyleGitHub
	if strings.EqualFold(strings.TrimSpace(forgeProvider), string(StyleGitLab)) {
		style = StyleGitLab
	}
	if h := normalizeHostInput(gitHost); h != "" {
		styles[h] = style
	}
	if h := normalizeHostInput(gitlabURL); h != "" {
		styles[h] = StyleGitLab
	}
	return styles
}

// NormalizeRepoHTTPS converts a Git repository URL (https://, ssh://, or
// SCP-style git@host:org/repo) into a clean https:// base URL without userinfo,
// query, fragment, or .git suffix, and resolves its ForgeStyle from styles.
//
// A nil styles map defaults to the public GitHub and GitLab hosts. Unknown
// hosts, cleartext http://, file://, embedded credentials, path traversal, and
// malformed URLs fail closed and return ("", "", false).
func NormalizeRepoHTTPS(rawURL string, styles HostStyles) (baseHTTPS string, style ForgeStyle, ok bool) {
	if rawURL == "" || hasForbiddenChars(rawURL) {
		return "", "", false
	}
	if styles == nil {
		styles = DefaultHostStyles("", "", "")
	}

	var host, repoPath string
	switch {
	case strings.Contains(rawURL, "://"):
		u, err := url.Parse(rawURL)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "https":
			if u.User != nil {
				return "", "", false
			}
		case "ssh":
			// SSH remotes legitimately carry git@ userinfo, which is stripped
			// when converting to the web HTTPS base URL.
		default:
			return "", "", false
		}
		host = u.Hostname()
		repoPath = u.Path
	default:
		// SCP-style remote: git@host:org/repo(.git)
		at := strings.IndexByte(rawURL, '@')
		colon := strings.IndexByte(rawURL, ':')
		if at <= 0 || colon <= at+1 {
			return "", "", false
		}
		host = rawURL[at+1 : colon]
		if strings.ContainsAny(host, "/?#@") {
			return "", "", false
		}
		repoPath = rawURL[colon+1:]
	}

	host, hostOK := asciiLowerHost(host)
	if !hostOK || host == "" {
		return "", "", false
	}
	style, ok = styles[host]
	if !ok {
		return "", "", false
	}

	cleanPath, pathOK := cleanRepoPath(repoPath)
	if !pathOK {
		return "", "", false
	}
	baseHTTPS = "https://" + host + "/" + cleanPath
	if !SafeHTTPS(baseHTTPS) {
		return "", "", false
	}
	return baseHTTPS, style, true
}

// CompareURL builds a validated HTTPS compare permalink for fromRev...toRev.
// Returns "" if baseHTTPS, style, or either revision token is invalid.
func CompareURL(baseHTTPS string, style ForgeStyle, fromRev, toRev string) string {
	if !SafeHTTPS(baseHTTPS) || !validRefToken(fromRev) || !validRefToken(toRev) {
		return ""
	}
	base := strings.TrimRight(baseHTTPS, "/")
	var out string
	switch style {
	case StyleGitHub:
		out = base + "/compare/" + fromRev + "..." + toRev
	case StyleGitLab:
		out = base + "/-/compare/" + fromRev + "..." + toRev
	default:
		return ""
	}
	if !SafeHTTPS(out) {
		return ""
	}
	return out
}

// CommitURL builds a validated HTTPS commit permalink for sha.
// Returns "" if baseHTTPS, style, or sha is invalid.
func CommitURL(baseHTTPS string, style ForgeStyle, sha string) string {
	if !SafeHTTPS(baseHTTPS) || !validRefToken(sha) {
		return ""
	}
	base := strings.TrimRight(baseHTTPS, "/")
	var out string
	switch style {
	case StyleGitHub:
		out = base + "/commit/" + sha
	case StyleGitLab:
		out = base + "/-/commit/" + sha
	default:
		return ""
	}
	if !SafeHTTPS(out) {
		return ""
	}
	return out
}

// FileURL builds a validated HTTPS file permalink at rev for filePath, with an
// optional #L<line> anchor when line > 0. Path segments are percent-encoded
// (including spaces, #, and parentheses) so the URL is safe in Markdown and chat
// renderers. Returns "" if any input is invalid or unsafe.
func FileURL(baseHTTPS string, style ForgeStyle, rev, filePath string, line int) string {
	if !SafeHTTPS(baseHTTPS) || !validRefToken(rev) || filePath == "" {
		return ""
	}
	encodedPath, ok := encodeFilePath(filePath)
	if !ok {
		return ""
	}
	base := strings.TrimRight(baseHTTPS, "/")
	var out string
	switch style {
	case StyleGitHub:
		out = base + "/blob/" + rev + "/" + encodedPath
	case StyleGitLab:
		out = base + "/-/blob/" + rev + "/" + encodedPath
	default:
		return ""
	}
	if line > 0 {
		out += fmt.Sprintf("#L%d", line)
	}
	if !SafeHTTPS(out) {
		return ""
	}
	return out
}

// FirstAddedLine parses the first unified-diff hunk header
// ("@@ -oldStart[,oldCount] +newStart[,newCount] @@") in unifiedPatch and
// returns newStart when > 0. Returns 0 for deleted files (+0,0), binary diffs,
// or patches without a hunk header.
func FirstAddedLine(unifiedPatch string) int {
	for _, line := range strings.Split(unifiedPatch, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		rest := line[3:]
		end := strings.Index(rest, " @@")
		if end < 0 {
			continue
		}
		header := rest[:end]
		plusIdx := strings.IndexByte(header, '+')
		if plusIdx < 0 {
			continue
		}
		numPart := header[plusIdx+1:]
		if sep := strings.IndexAny(numPart, ", "); sep >= 0 {
			numPart = numPart[:sep]
		}
		n, err := strconv.Atoi(numPart)
		if err == nil && n > 0 {
			return n
		}
		return 0
	}
	return 0
}

// SafeHTTPS reports whether rawURL is a well-formed https:// URL free of
// userinfo, whitespace, control characters, and chat/markup delimiters.
func SafeHTTPS(rawURL string) bool {
	if !strings.HasPrefix(rawURL, "https://") || hasForbiddenChars(rawURL) {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	return true
}

// AppendAnchor appends anchor (e.g. "#resolution") onto rawURL when rawURL is a
// safe HTTPS URL without an existing fragment and the combined URL remains safe.
// Otherwise returns rawURL unchanged.
func AppendAnchor(rawURL, anchor string) string {
	if anchor == "" || !SafeHTTPS(rawURL) || strings.Contains(rawURL, "#") {
		return rawURL
	}
	if !strings.HasPrefix(anchor, "#") {
		anchor = "#" + anchor
	}
	combined := rawURL + anchor
	if !SafeHTTPS(combined) {
		return rawURL
	}
	return combined
}

func normalizeHostInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	host := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		host = u.Hostname()
	} else if idx := strings.IndexByte(raw, '/'); idx >= 0 {
		host = raw[:idx]
	}
	lower, ok := asciiLowerHost(host)
	if !ok {
		return ""
	}
	return lower
}

func asciiLowerHost(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return "", false
		}
	}
	return strings.ToLower(host), true
}

func hasForbiddenChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return true
		}
		switch r {
		case '|', '<', '>', '"', '\'', '`', '\\':
			return true
		}
	}
	return false
}

func validRefToken(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "..") {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		isAlphaNum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '.' && c != '_' && c != '/' && c != '-' {
			return false
		}
	}
	return true
}

func cleanRepoPath(rawPath string) (string, bool) {
	p := strings.TrimPrefix(rawPath, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	if p == "" {
		return "", false
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", false
	}
	encoded := make([]string, len(segs))
	for i, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
		encoded[i] = escapePathSegment(seg)
	}
	return strings.Join(encoded, "/"), true
}

func encodeFilePath(filePath string) (string, bool) {
	if strings.ContainsAny(filePath, "\x00\r\n") {
		return "", false
	}
	p := strings.TrimPrefix(filePath, "/")
	if p == "" {
		return "", false
	}
	segs := strings.Split(p, "/")
	encoded := make([]string, len(segs))
	for i, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return "", false
			}
		}
		encoded[i] = escapePathSegment(seg)
	}
	return strings.Join(encoded, "/"), true
}

// escapePathSegment percent-encodes a single URL path segment, including
// parentheses so Markdown link syntax [label](url) is never broken.
func escapePathSegment(seg string) string {
	esc := url.PathEscape(seg)
	esc = strings.ReplaceAll(esc, "(", "%28")
	esc = strings.ReplaceAll(esc, ")", "%29")
	return esc
}
