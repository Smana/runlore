// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestRenderFrontmatterAndBody(t *testing.T) {
	out := Render(providers.KBEntry{
		Type: "Playbook", Title: "Redis failover", Description: "how to fail over redis",
		Tags: []string{"imported", "playbook"}, Body: "# Redis failover\n\nsteps",
	}, Meta{Timestamp: "2024-03-01"})
	for _, want := range []string{
		"---\n", "type: Playbook\n", "title: Redis failover\n",
		"timestamp: \"2024-03-01\"", "tags:\n", "- imported\n",
		"# Redis failover\n\nsteps\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderOmitsEmptyMeta(t *testing.T) {
	out := Render(providers.KBEntry{Type: "Playbook", Title: "T", Description: "d"}, Meta{})
	for _, absent := range []string{"timestamp:", "status:", "last_validated:", "fingerprint:", "resource:"} {
		if strings.Contains(out, absent) {
			t.Fatalf("empty %s must be omitted:\n%s", absent, out)
		}
	}
}

func TestRenderYAMLInjectionSafeTitle(t *testing.T) {
	// Marshaled (not string-formatted), so a colon-bearing title can't inject keys.
	out := Render(providers.KBEntry{Type: "Playbook", Title: "a: b\nresource: evil", Description: "d"}, Meta{})
	if strings.Contains(out, "\nresource: evil\n") {
		t.Fatalf("newline title must not inject a frontmatter key:\n%s", out)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Redis failover — March 2024!", "redis-failover-march-2024"},
		{"  KubePodCrashLooping  ", "kubepodcrashlooping"},
		{"---", ""},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
