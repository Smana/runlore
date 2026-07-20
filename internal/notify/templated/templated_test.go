// SPDX-License-Identifier: Apache-2.0

package templated

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers"
)

func decodeExtra(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0] // unwrap the document node
}

func TestBuildParsesInstances(t *testing.T) {
	t.Setenv("T_URL", "https://example.com/hook")
	node := decodeExtra(t, `
- name: teams
  url_env: T_URL
  template: '{"text": {{ toJSON .Title }}}'
`)
	n, err := build(node)
	if err != nil || n == nil || len(n.instances) != 1 {
		t.Fatalf("n=%+v err=%v", n, err)
	}
	if got := n.instances[0].contentType; got != "application/json" {
		t.Errorf("default content_type = %q", got)
	}
}

func TestBuildFailsClosedOnBadConfig(t *testing.T) {
	t.Setenv("T_URL", "https://example.com/hook")
	for name, y := range map[string]string{
		"parse error":    "- {name: a, url_env: T_URL, template: '{{ .Title }'}",
		"missing name":   "- {url_env: T_URL, template: ok}",
		"missing url":    "- {name: a, template: ok}",
		"missing tmpl":   "- {name: a, url_env: T_URL}",
		"duplicate name": "- {name: a, url_env: T_URL, template: ok}\n- {name: a, url_env: T_URL, template: ok}",
	} {
		if _, err := build(decodeExtra(t, y)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestBuildDisablesInstanceOnUnsetEnv(t *testing.T) {
	node := decodeExtra(t, "- {name: a, url_env: T_UNSET_NEVER, template: ok}")
	n, err := build(node)
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Errorf("all instances env-disabled ⇒ nil notifier, got %+v", n)
	}
}

func testNotifier(t *testing.T, tmplBody, url string) *Notifier {
	t.Helper()
	t.Setenv("T_URL", url)
	n, err := build(decodeExtra(t, "- name: teams\n  url_env: T_URL\n  token_env: T_TOK\n  template: '"+tmplBody+"'"))
	if err != nil || n == nil {
		t.Fatalf("build: n=%v err=%v", n, err)
	}
	return n
}

func TestDeliverRendersAndPosts(t *testing.T) {
	t.Setenv("T_TOK", "sekret")
	var gotBody, gotCT, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotAuth = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
	}))
	defer srv.Close()
	n := testNotifier(t, `{"text": {{ toJSON .Title }}}`, srv.URL)
	inv := providers.Investigation{Title: `quote " and \ slash`}
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"text": "quote \" and \\ slash"}` {
		t.Errorf("body = %s", gotBody) // toJSON must escape — raw splice would be JSON injection
	}
	if gotCT != "application/json" || gotAuth != "Bearer sekret" {
		t.Errorf("ct=%q auth=%q", gotCT, gotAuth)
	}
}

func TestDeliverExecErrorFailsLoudWithoutPost(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posted = true }))
	defer srv.Close()
	n := testNotifier(t, `{{ .NoSuchField }}`, srv.URL) // parses fine, fails at exec
	if err := n.Deliver(context.Background(), providers.Investigation{Title: "x"}); err == nil {
		t.Error("want exec error")
	}
	if posted {
		t.Error("exec error must not POST")
	}
}

func TestDeliverNon2xxAndSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if err := testNotifier(t, `ok`, srv.URL).Deliver(context.Background(), providers.Investigation{}); err == nil {
		t.Error("want non-2xx error")
	}
	big := testNotifier(t, `{{ .Title }}`, srv.URL)
	inv := providers.Investigation{Title: strings.Repeat("A", maxBody+1)}
	if err := big.Deliver(context.Background(), inv); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("want size-cap error, got %v", err)
	}
}
