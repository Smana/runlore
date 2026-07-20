// SPDX-License-Identifier: Apache-2.0

package custom

import "testing"

func TestParseAndLookup(t *testing.T) {
	doc := map[string]any{
		"alerts": []any{
			map[string]any{"labels": map[string]any{"alertname": "HighCPU", "code": float64(503), "firing": true}},
		},
		"title": "root",
	}
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"title", "root", true},
		{"alerts[0].labels.alertname", "HighCPU", true},
		{"alerts[0].labels.code", "503", true},
		{"alerts[0].labels.firing", "true", true},
		{"alerts[1].labels.alertname", "", false}, // index out of range
		{"alerts[0].labels.missing", "", false},
		{"alerts", "", false}, // array leaf refuses coercion
	}
	for _, c := range cases {
		p, err := parsePath(c.path)
		if err != nil {
			t.Fatalf("parsePath(%q): %v", c.path, err)
		}
		v, found := p.lookup(doc)
		got, coerced := "", false
		if found {
			got, coerced = coerce(v)
		}
		if (found && coerced) != c.ok || got != c.want {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", c.path, got, found && coerced, c.want, c.ok)
		}
	}
}

func TestParsePathRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "a[", "a[x]", "a[-1]", "a..b", "[0]", "a["} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("parsePath(%q): want error, got nil", bad)
		}
	}
}
