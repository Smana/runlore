// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// getting-started.md inlines the ENTIRE values-minimal.yaml profile under this
// heading — the one values file short enough to read on the page. An inlined file
// is a copy, and a copy rots: the shipped profile is CI-validated against both the
// chart schema and the agent config schema (internal/config), while the page's
// duplicate is validated by nobody, so a reader could paste a block that no longer
// loads and blame RunLore.
//
// This pins the two together. It compares the DECODED YAML, not the text: comments
// and spacing are free to differ (the file carries an install header the page does
// not need), but a key, value, or block present in one and not the other fails.
const (
	minimalProfileHeading = "### Start: `values-minimal.yaml`"
	minimalProfilePath    = "../../deploy/helm/runlore/values-minimal.yaml"
	gettingStartedPath    = "../../website/content/docs/getting-started.md"
)

func TestGettingStartedInlinesMinimalProfileVerbatim(t *testing.T) {
	page, err := os.ReadFile(gettingStartedPath)
	if err != nil {
		t.Fatalf("read getting-started.md: %v", err)
	}
	// The heading's line number, so the fence can be picked out of the page's
	// scanned fences by position — one fence-finder for the whole package (see
	// scanYAMLFences) rather than a second expression that can drift from it.
	headingLine := -1
	for n, l := range strings.Split(string(page), "\n") {
		if strings.TrimSpace(l) == minimalProfileHeading {
			headingLine = n + 1
			break
		}
	}
	if headingLine < 0 {
		// Guard the guard: a renamed heading would leave this test matching nothing
		// while still reporting success.
		t.Fatalf("heading %q not found in getting-started.md — the section was renamed "+
			"and this guard is now inert", minimalProfileHeading)
	}
	fences, _, err := scanYAMLFences(gettingStartedPath, page)
	if err != nil {
		t.Fatalf("scan fences: %v", err)
	}
	var block string
	for _, f := range fences {
		if f.Line > headingLine {
			block = f.Body
			break
		}
	}
	if block == "" {
		t.Fatal("no YAML fence after the values-minimal heading — the profile is no longer inlined")
	}

	shipped, err := os.ReadFile(minimalProfilePath)
	if err != nil {
		t.Fatalf("read values-minimal.yaml: %v", err)
	}

	var fromPage, fromFile map[string]any
	if err := yaml.Unmarshal([]byte(block), &fromPage); err != nil {
		t.Fatalf("the inlined block is not valid YAML: %v", err)
	}
	if err := yaml.Unmarshal(shipped, &fromFile); err != nil {
		t.Fatalf("values-minimal.yaml is not valid YAML: %v", err)
	}
	if !reflect.DeepEqual(fromPage, fromFile) {
		t.Errorf("getting-started.md's inlined values-minimal block has drifted from "+
			"%s — re-copy the file into the fence.\npage: %#v\nfile: %#v",
			filepath.Base(minimalProfilePath), fromPage, fromFile)
	}
}
