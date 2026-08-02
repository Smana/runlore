// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
	i := bytes.Index(page, []byte(minimalProfileHeading))
	if i < 0 {
		// Guard the guard: a renamed heading would leave this test matching nothing
		// while still reporting success.
		t.Fatalf("heading %q not found in getting-started.md — the section was renamed "+
			"and this guard is now inert", minimalProfileHeading)
	}
	m := yamlFence.FindSubmatch(page[i:])
	if m == nil {
		t.Fatal("no YAML fence after the values-minimal heading — the profile is no longer inlined")
	}

	shipped, err := os.ReadFile(minimalProfilePath)
	if err != nil {
		t.Fatalf("read values-minimal.yaml: %v", err)
	}

	var fromPage, fromFile map[string]any
	if err := yaml.Unmarshal(m[1], &fromPage); err != nil {
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
