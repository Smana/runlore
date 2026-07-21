// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The kb-steward Claude Code plugin is served from this repo (see
// docs/kb-steward.md). These tests keep the skill's documented OKF contract
// and the plugin manifests from drifting as the loader evolves.

const pluginRoot = "../../plugins/kb-steward"

var parsedFieldsRE = regexp.MustCompile("`([a-z_]+)`")

// TestOKFFormatDocMatchesLoader pins the skill's documented frontmatter field
// list to what the loader actually parses (the frontmatter fields of Entry).
func TestOKFFormatDocMatchesLoader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pluginRoot, "skills/kb-steward/references/okf-format.md"))
	if err != nil {
		t.Fatalf("read okf-format.md: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "<!-- parsed-fields:start -->")
	end := strings.Index(doc, "<!-- parsed-fields:end -->")
	if start < 0 || end < 0 || end < start {
		t.Fatal("okf-format.md must contain a <!-- parsed-fields:start -->…<!-- parsed-fields:end --> block")
	}
	documented := map[string]bool{}
	for _, m := range parsedFieldsRE.FindAllStringSubmatch(doc[start:end], -1) {
		documented[m[1]] = true
	}

	parsed := map[string]bool{}
	et := reflect.TypeOf(Entry{})
	for i := 0; i < et.NumField(); i++ {
		name := et.Field(i).Name
		if name == "Body" || name == "Path" { // not frontmatter
			continue
		}
		parsed[snakeCase(name)] = true
	}

	for f := range parsed {
		if !documented[f] {
			t.Errorf("loader parses frontmatter field %q but okf-format.md does not document it", f)
		}
	}
	for f := range documented {
		if !parsed[f] {
			t.Errorf("okf-format.md documents field %q but the loader does not parse it", f)
		}
	}
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
