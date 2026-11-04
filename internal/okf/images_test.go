// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"strings"
	"testing"
)

// TestNeutralizeImages is the unit test for the image-beacon neutralizer (S2).
func TestNeutralizeImages(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"no image", "plain text", "plain text"},
		{"empty alt", "![](https://attacker/beacon)", "`[image]`"},
		{"with alt", "![screenshot](https://attacker/leak?x=1)", "`[image: screenshot]`"},
		{"data url", "![x](data:image/png;base64,abc)", "`[image: x]`"},
		{"surrounding text", "before ![logo](https://cdn/logo.png) after", "before `[image: logo]` after"},
		{"multiple images", "![a](u1) and ![b](u2)", "`[image: a]` and `[image: b]`"},
		{"not an image link", "[text](https://example.com)", "[text](https://example.com)"},
		{"inline code preserved", "`code`", "`code`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeutralizeImages(tc.in); got != tc.want {
				t.Errorf("NeutralizeImages(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefuseMarkup(t *testing.T) {
	for _, in := range []string{"![a[b]](https://attacker/b)", "![a\\]](https://attacker/b)", "<img src=https://attacker/b>", "x <!-- c --> y", "![x](u)"} {
		if got := DefuseMarkup(in); strings.Contains(got, "![") || strings.Contains(got, "<img") || strings.Contains(got, "<!") {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if got := DefuseMarkup("replicas < 3 ![x](u)"); got != "replicas < 3 `[image: x]`" {
		t.Errorf("got %q", got)
	}
}
