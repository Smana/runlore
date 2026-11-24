// SPDX-License-Identifier: Apache-2.0

package kbui

import (
	"fmt"
	"html"
	"html/template"
	"strings"
)

// RenderMarkdown renders a strict, safe subset of Markdown to HTML.
//
// Every input line is HTML-escaped BEFORE any tag is emitted, so untrusted
// knowledge-base markdown can never inject raw HTML or script tags. Only
// https:// links are turned into anchors; all other URL schemes stay plain
// escaped text.
func RenderMarkdown(md string) template.HTML {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")

	var out strings.Builder
	var para []string
	var listKind string // "", "ul", "ol"
	inFence := false

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		out.WriteString("<p>")
		for i, ln := range para {
			if i > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(renderInline(html.EscapeString(ln)))
		}
		out.WriteString("</p>\n")
		para = para[:0]
	}

	closeList := func() {
		if listKind == "" {
			return
		}
		out.WriteString("</" + listKind + ">\n")
		listKind = ""
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				flushPara()
				closeList()
				inFence = true
				out.WriteString("<pre><code>")
			} else {
				inFence = false
				out.WriteString("</code></pre>\n")
			}
			continue
		}
		if inFence {
			out.WriteString(html.EscapeString(line))
			out.WriteByte('\n')
			continue
		}

		if trimmed == "" {
			flushPara()
			closeList()
			continue
		}

		if level, text, ok := parseHeading(trimmed); ok {
			flushPara()
			closeList()
			tag := headingTag(level)
			slug := headingSlug(text)
			escaped := renderInline(html.EscapeString(text))
			if slug != "" {
				fmt.Fprintf(&out, "<%s id=%q>%s</%s>\n", tag, slug, escaped, tag)
			} else {
				fmt.Fprintf(&out, "<%s>%s</%s>\n", tag, escaped, tag)
			}
			continue
		}

		if item, ok := parseUnorderedItem(trimmed); ok {
			flushPara()
			if listKind != "ul" {
				closeList()
				listKind = "ul"
				out.WriteString("<ul>\n")
			}
			fmt.Fprintf(&out, "<li>%s</li>\n", renderInline(html.EscapeString(item)))
			continue
		}

		if item, ok := parseOrderedItem(trimmed); ok {
			flushPara()
			if listKind != "ol" {
				closeList()
				listKind = "ol"
				out.WriteString("<ol>\n")
			}
			fmt.Fprintf(&out, "<li>%s</li>\n", renderInline(html.EscapeString(item)))
			continue
		}

		closeList()
		para = append(para, trimmed)
	}

	if inFence {
		out.WriteString("</code></pre>\n")
	}
	flushPara()
	closeList()

	return template.HTML(out.String()) //nolint:gosec // G203: escape-first renderer emits only whitelisted static tags over html.EscapeString output
}

func parseHeading(line string) (level int, text string, ok bool) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 4 || i >= len(line) || line[i] != ' ' {
		return 0, "", false
	}
	return i, strings.TrimSpace(line[i+1:]), true
}

func headingTag(level int) string {
	switch level {
	case 1, 2:
		return "h2"
	case 3:
		return "h3"
	default:
		return "h4"
	}
}

func headingSlug(text string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(text) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func parseUnorderedItem(line string) (string, bool) {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return strings.TrimSpace(line[2:]), true
	}
	return "", false
}

func parseOrderedItem(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(line) || line[i] != '.' || line[i+1] != ' ' {
		return "", false
	}
	return strings.TrimSpace(line[i+2:]), true
}

// renderInline transforms inline code, https:// links, and **bold** on an
// ALREADY html.EscapeString-escaped string.
func renderInline(escaped string) string {
	var out strings.Builder
	rest := escaped
	for len(rest) > 0 {
		idx := strings.IndexByte(rest, '`')
		if idx < 0 {
			out.WriteString(renderLinksAndBold(rest))
			break
		}
		end := strings.IndexByte(rest[idx+1:], '`')
		if end < 0 {
			out.WriteString(renderLinksAndBold(rest))
			break
		}
		out.WriteString(renderLinksAndBold(rest[:idx]))
		code := rest[idx+1 : idx+1+end]
		out.WriteString("<code>")
		out.WriteString(code)
		out.WriteString("</code>")
		rest = rest[idx+1+end+1:]
	}
	return out.String()
}

func renderLinksAndBold(escaped string) string {
	var out strings.Builder
	rest := escaped
	for len(rest) > 0 {
		openBracket := strings.IndexByte(rest, '[')
		if openBracket < 0 {
			out.WriteString(renderBold(rest))
			break
		}
		closeBracket := strings.Index(rest[openBracket+1:], "](")
		if closeBracket < 0 {
			out.WriteString(renderBold(rest[:openBracket+1]))
			rest = rest[openBracket+1:]
			continue
		}
		labelStart := openBracket + 1
		labelEnd := labelStart + closeBracket
		urlStart := labelEnd + 2
		closeParen := strings.IndexByte(rest[urlStart:], ')')
		if closeParen < 0 {
			out.WriteString(renderBold(rest[:openBracket+1]))
			rest = rest[openBracket+1:]
			continue
		}
		urlEnd := urlStart + closeParen
		label := rest[labelStart:labelEnd]
		href := rest[urlStart:urlEnd]

		out.WriteString(renderBold(rest[:openBracket]))
		if isSafeHTTPSURL(href) {
			out.WriteString(`<a href="`)
			out.WriteString(href)
			out.WriteString(`" rel="noopener noreferrer">`)
			out.WriteString(renderBold(label))
			out.WriteString(`</a>`)
		} else {
			out.WriteString(renderBold(rest[openBracket : urlEnd+1]))
		}
		rest = rest[urlEnd+1:]
	}
	return out.String()
}

func isSafeHTTPSURL(escapedURL string) bool {
	if !strings.HasPrefix(escapedURL, "https://") || len(escapedURL) <= len("https://") {
		return false
	}
	if strings.ContainsAny(escapedURL, " \t\r\n<>\"'") {
		return false
	}
	for _, bad := range []string{"&lt;", "&gt;", "&#34;", "&#39;"} {
		if strings.Contains(escapedURL, bad) {
			return false
		}
	}
	return true
}

func renderBold(escaped string) string {
	var out strings.Builder
	rest := escaped
	for len(rest) > 0 {
		start := strings.Index(rest, "**")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		end := strings.Index(rest[start+2:], "**")
		if end <= 0 {
			out.WriteString(rest[:start+2])
			rest = rest[start+2:]
			continue
		}
		out.WriteString(rest[:start])
		out.WriteString("<strong>")
		out.WriteString(rest[start+2 : start+2+end])
		out.WriteString("</strong>")
		rest = rest[start+2+end+2:]
	}
	return out.String()
}
