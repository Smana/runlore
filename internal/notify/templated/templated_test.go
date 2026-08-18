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

// TestDeliverErrorNeverLeaksTheURL: a templated instance targets Teams/Discord/
// ntfy/incident.io, whose incoming-webhook URL carries the secret IN THE PATH —
// the URL *is* the credential. net/http reports both a request-build failure and
// a transport failure as a *url.Error whose Error() prints the URL verbatim (it
// masks a userinfo password and nothing else), and Deliver's error is logged at
// Error level by the delivery path. Both sites must go through
// httpx.SanitizeURLError, which keeps op + scheme://host and drops path/query.
func TestDeliverErrorNeverLeaksTheURL(t *testing.T) {
	const secret = "T0ZZZ-B0ZZZ-DoNotLogThisWebhookSecret"

	// Transport failure: a listener that is already closed ⇒ connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL + "/services/" + secret
	srv.Close()

	// Request-build failure: a control character makes url.Parse fail, and
	// url.Parse's own *url.Error carries the raw (secret-bearing) URL.
	malformed := "https://hooks.example.com/services/" + secret + "\n"

	for name, target := range map[string]string{"transport": dead, "request build": malformed} {
		err := testNotifier(t, `{{ .Title }}`, target).Deliver(context.Background(), providers.Investigation{Title: "x"})
		if err == nil {
			t.Fatalf("%s: want a delivery error, got nil", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the webhook credential: %v", name, err)
		}
	}
}

// TestTemplatedDeliberatelyDoesNotAnnounceKBUpdates states this package's
// answer to the KB-update capability, so that answer is a decision on the
// record rather than an omission nobody noticed.
//
// It does NOT implement providers.KBUpdateNotifier, and the reason is the
// notifier's whole design: an instance is an operator-supplied text/template
// executed over notify.Payload — a finished investigation. A knowledge-base
// update is not one. Running the operator's finding template over it would
// render every field as its zero value and POST that to Teams, Discord or
// ntfy: a notification claiming an investigation with no title, no verdict and
// no confidence, which is worse than not notifying at all.
//
// Doing it properly needs a SECOND template key, for a shape no operator has
// written a template for — a config change with its own schema, validation and
// docs, not a method. Until that key exists, being skipped by notify.Multi is
// the honest behaviour, and an operator wanting a machine-readable KB update
// today has notify.webhook, which sends the whole record as JSON.
//
// Deleting this test is the way to change the decision; it should not survive
// an implementation.
func TestTemplatedDeliberatelyDoesNotAnnounceKBUpdates(t *testing.T) {
	var n providers.Notifier = &Notifier{}
	if _, ok := n.(providers.KBUpdateNotifier); ok {
		t.Fatal("templated implements providers.KBUpdateNotifier — if that is intended, delete this test and document the template key the announcement renders through; " +
			"an instance's template is written against notify.Payload, and a KB update executed through it renders an investigation of zero values")
	}
}

// TestDeliverRendersTheResourceScope is the end-to-end half of the resource-scope
// fix, on the surface that actually shipped the bug: an OPERATOR-written template
// over notify.Payload. Delivered Slack cards on 2026-08-17/18 read "Node
// observability/ip-10-11-132-8.ec2.internal"; the card was fixed, and this pins that
// a template cannot reproduce it from the payload's own fields.
//
// Both fields are asserted from ONE rendered body: .ResourceRef must carry the
// scoped identity, and the hand-join .Namespace/.Resource that a real template
// writes must no longer name a namespace the Node was never in.
func TestDeliverRendersTheResourceScope(t *testing.T) {
	for name, tc := range map[string]struct {
		w    providers.Workload
		want string
	}{
		"cluster-scoped kind": {
			w:    providers.Workload{Kind: "Node", Namespace: "observability", Name: "ip-10-11-132-8.ec2.internal"},
			want: "ref=ip-10-11-132-8.ec2.internal join=/ip-10-11-132-8.ec2.internal",
		},
		"cloud kind": {
			w:    providers.Workload{Kind: "DBInstance", Namespace: "observability", Name: "datagrok-aqemia-shared"},
			want: "ref=datagrok-aqemia-shared join=/datagrok-aqemia-shared",
		},
		"namespace object with no name": {
			w:    providers.Workload{Kind: "Namespace", Namespace: "coder-engineering"},
			want: "ref=coder-engineering join=coder-engineering/",
		},
		"namespaced kind renders unchanged": {
			w:    providers.Workload{Kind: "Pod", Namespace: "payments", Name: "api-7f9c"},
			want: "ref=payments/api-7f9c join=payments/api-7f9c",
		},
	} {
		var gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		}))
		n := testNotifier(t, `ref={{ .ResourceRef }} join={{ .Namespace }}/{{ .Resource }}`, srv.URL)
		err := n.Deliver(context.Background(), providers.Investigation{Resource: tc.w})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if gotBody != tc.want {
			t.Errorf("%s: body = %q, want %q", name, gotBody, tc.want)
		}
	}
}
